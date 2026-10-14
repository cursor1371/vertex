package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// runDBVacuum 实现 `vertex-proxy vacuum` 子命令：离线压缩数据库文件。
//
// SQLite 的 DELETE 只把页放进 freelist，auto_vacuum=NONE 时文件永不收缩，
// 长期运行会积累成数十 GB 的空洞。本命令重建数据库文件把空洞归还文件系统，
// 并顺带把新库切到 auto_vacuum=INCREMENTAL，使此后每轮保留清理都能增量回收。
//
// 流程：占用探测 → VACUUM INTO 新库 → 新库开启 auto_vacuum 并重建 → quick_check
// → 原子替换（原库留作 data.db.old）→ 清理陈旧 -wal/-shm。
// 必须在服务停止后运行；被占用时直接报错退出，不改动任何文件。
func runDBVacuum() int {
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════════════╗")
	fmt.Println("  ║   Vertex AI Proxy — 数据库离线压缩                   ║")
	fmt.Println("  ╚══════════════════════════════════════════════════════╝")
	fmt.Println()

	dbPath := filepath.Join(config.ConfigDir(), "data.db")
	newPath := dbPath + ".new"
	oldPath := dbPath + ".old"

	if _, err := os.Stat(dbPath); err != nil {
		fmt.Printf("  [X] 找不到数据库文件：%s\n", dbPath)
		return 1
	}
	// 占用探测：主库与 -shm 任一被占用即判定服务仍在运行。
	for _, p := range []string{dbPath, dbPath + "-shm"} {
		if db.FileInUse(p) {
			fmt.Printf("  [X] 数据库正被占用（%s 被其他进程打开）。\n", filepath.Base(p))
			fmt.Println("      请先停止 vertex-proxy 服务，再重新运行本命令。")
			return 1
		}
	}
	if _, err := os.Stat(oldPath); err == nil {
		fmt.Printf("  [X] 已存在 %s。请先确认它无用并删除，避免覆盖你的备份。\n", filepath.Base(oldPath))
		return 1
	}
	_ = os.Remove(newPath) // 清理上次中断留下的半成品

	beforeBytes := fileSizeOrZero(dbPath)
	fmt.Printf("  [i] 目标库：%s\n", dbPath)
	fmt.Printf("  [i] 压缩前：%s\n", humanBytes(beforeBytes))
	fmt.Println()

	src, err := db.Open(dbPath)
	if err != nil {
		fmt.Printf("  [X] 打开数据库失败：%v\n", err)
		return 1
	}
	defer func() { _ = src.Close() }()

	st, err := db.NewDBMaintenance(src, dbPath).Stats()
	if err != nil {
		fmt.Printf("  [X] 读取库体积信息失败：%v\n", err)
		return 1
	}
	// 活数据体积 = (总页数 - 空闲页数) × 页大小。压缩后文件约等于这个量级，
	// 且新库还要再重建一次（临时空间同量级），故按 3 倍留余量。
	liveBytes := (st.PageCount - st.FreelistPages) * st.PageSize
	need := liveBytes * 3
	if st.DiskFreeBytes < need {
		fmt.Printf("  [X] 磁盘余量不足：本次压缩峰值约需 %s，当前可用 %s。\n",
			humanBytes(need), humanBytes(st.DiskFreeBytes))
		return 1
	}
	fmt.Printf("  [i] 空闲页 %s（占 %.0f%%），活数据约 %s\n",
		humanBytes(st.FreelistBytes), st.ReclaimRatio*100, humanBytes(liveBytes))
	fmt.Printf("  [i] 磁盘可用 %s（峰值约需 %s）\n", humanBytes(st.DiskFreeBytes), humanBytes(need))
	fmt.Println()

	// ── 1. 重建到新文件 ──
	fmt.Println("  [1/4] VACUUM INTO 新文件 ...")
	if _, err := src.Exec("VACUUM INTO '" + escapeSQLString(newPath) + "'"); err != nil {
		fmt.Printf("  [X] VACUUM INTO 失败：%v\n", err)
		_ = os.Remove(newPath)
		return 1
	}
	fmt.Printf("        已生成 %s（%s）\n", filepath.Base(newPath), humanBytes(fileSizeOrZero(newPath)))

	// ── 2. 新库开启增量自动回收并重建 ──
	// 复用 DBMaintenance 的实现（与后台一次性初始化同一段逻辑），避免两处各写一套。
	fmt.Println("  [2/4] 开启 auto_vacuum=INCREMENTAL 并重建新库 ...")
	dst, err := db.Open(newPath)
	if err != nil {
		fmt.Printf("  [X] 打开新库失败：%v\n", err)
		_ = os.Remove(newPath)
		return 1
	}
	if err := db.NewDBMaintenance(dst, newPath).ApplyIncrementalAutoVacuum(context.Background()); err != nil {
		_ = dst.Close()
		fmt.Printf("  [X] 重建新库失败：%v\n", err)
		_ = os.Remove(newPath)
		return 1
	}

	// ── 3. 校验新库 ──
	fmt.Println("  [3/4] 校验新库完整性 ...")
	check, checkErr := db.NewDBMaintenance(dst, newPath).QuickCheck()
	_ = dst.Close() // 替换前必须释放句柄
	if checkErr != nil {
		fmt.Printf("  [X] 完整性检查失败：%v\n", checkErr)
		_ = os.Remove(newPath)
		return 1
	}
	if check != "ok" {
		fmt.Printf("  [X] 新库完整性检查未通过：%s\n", check)
		fmt.Printf("      已保留 %s 供排查，原库未改动。\n", filepath.Base(newPath))
		return 1
	}
	fmt.Println("        quick_check = ok")

	// 替换前必须释放源库句柄，否则 Windows 会以"文件被占用"拒绝重命名。
	// Close 幂等，defer 里的那次重复关闭无害。
	if err := src.Close(); err != nil {
		fmt.Printf("  [X] 关闭源库失败：%v\n", err)
		return 1
	}

	// ── 4. 原子替换 ──
	fmt.Println("  [4/4] 替换文件 ...")
	if err := os.Rename(dbPath, oldPath); err != nil {
		fmt.Printf("  [X] 无法重命名原库（可能服务仍在运行）：%v\n", err)
		fmt.Printf("      原库未改动；新库保留在 %s。\n", filepath.Base(newPath))
		return 1
	}
	if err := os.Rename(newPath, dbPath); err != nil {
		// 回滚，保证 data.db 始终存在。
		_ = os.Rename(oldPath, dbPath)
		fmt.Printf("  [X] 替换新库失败：%v（已回滚，原库完好）\n", err)
		return 1
	}
	// 陈旧的 WAL/SHM 属于旧库，必须删除，否则会被误当作新库的日志应用。
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	afterBytes := fileSizeOrZero(dbPath)
	fmt.Println()
	fmt.Println("  ══════════════════════════════════════════════════════")
	fmt.Printf("  [OK] 压缩完成：%s → %s（释放 %s）\n",
		humanBytes(beforeBytes), humanBytes(afterBytes), humanBytes(beforeBytes-afterBytes))
	fmt.Println("  ══════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Printf("  原库已保留为 %s。\n", filepath.Base(oldPath))
	fmt.Println("  请启动 vertex-proxy，确认后台「数据库」页数据正常后，再删除该备份文件。")
	fmt.Println("  此后 auto_vacuum=INCREMENTAL 已开启，保留清理会自动回收空间，不再累积空洞。")
	fmt.Println()
	return 0
}

func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func fileSizeOrZero(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// humanBytes 把字节数格式化为易读文本（仅用于面向用户的输出）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
