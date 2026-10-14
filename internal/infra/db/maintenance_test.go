package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/diskfree"
)

// newTempDB 在临时目录建一个文件库，返回维护器与库路径。
func newTempDB(t *testing.T) (*DBMaintenance, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return NewDBMaintenance(database, path), path
}

// newLegacyTempDB 建一个 auto_vacuum=NONE 的库，模拟"Open 引入增量回收默认值之前
// 就已存在"的旧库。新建库现在会被 Open 自动设为 INCREMENTAL，故这里绕过 Open 建库。
func newLegacyTempDB(t *testing.T) (*DBMaintenance, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	database, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	// 必须在建表前设置：库一旦非空，切换 auto_vacuum 就只能靠 VACUUM 生效。
	if _, err := database.Exec("PRAGMA auto_vacuum=NONE"); err != nil {
		t.Fatalf("set auto_vacuum=NONE: %v", err)
	}
	if err := createTables(database); err != nil {
		t.Fatalf("createTables: %v", err)
	}
	return NewDBMaintenance(database, path), path
}

// autoVacuumMode 读取当前 auto_vacuum 取值。
func autoVacuumMode(t *testing.T, m *DBMaintenance) int {
	t.Helper()
	var v int
	if err := m.db.QueryRow("PRAGMA auto_vacuum").Scan(&v); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	return v
}

// insertRace 插入 n 条竞速记录（ts 从 baseTS 起逐条 +1）。
func insertRace(t *testing.T, m *DBMaintenance, baseTS int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := m.db.Exec(
			"INSERT INTO race_log (ts, req_id, node_name, node_uri, outcome, elapsed_ms) VALUES (?,?,?,?,?,?)",
			baseTS+int64(i), "r", "n", "vless://x", "win", 1,
		); err != nil {
			t.Fatalf("insert race_log: %v", err)
		}
	}
}

// TestDBMaintenanceStats 验证体检字段自洽：体积/页数/行数/磁盘余量都能取到。
func TestDBMaintenanceStats(t *testing.T) {
	m, path := newTempDB(t)
	insertRace(t, m, 1000, 5)

	st, err := m.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Path != path {
		t.Errorf("Path = %q, want %q", st.Path, path)
	}
	if st.PageSize <= 0 || st.PageCount <= 0 {
		t.Errorf("页信息异常: page_size=%d page_count=%d", st.PageSize, st.PageCount)
	}
	if st.FileBytes <= 0 {
		t.Errorf("FileBytes 应大于 0, got %d", st.FileBytes)
	}
	if st.FreelistBytes < 0 || st.ReclaimRatio < 0 || st.ReclaimRatio > 1 {
		t.Errorf("空闲页信息异常: bytes=%d ratio=%f", st.FreelistBytes, st.ReclaimRatio)
	}
	if st.DiskFreeBytes <= 0 || st.DiskTotalBytes <= 0 {
		t.Errorf("磁盘信息异常: free=%d total=%d", st.DiskFreeBytes, st.DiskTotalBytes)
	}
	if st.JournalMode == "" {
		t.Error("JournalMode 不应为空")
	}

	var race *TableStat
	for i := range st.Tables {
		if st.Tables[i].Name == "race_log" {
			race = &st.Tables[i]
		}
	}
	if race == nil {
		t.Fatalf("Tables 应包含 race_log, got %+v", st.Tables)
	}
	if race.Rows != 5 {
		t.Errorf("race_log 行数 = %d, want 5", race.Rows)
	}
	if race.MinTS != 1000 || race.MaxTS != 1004 {
		t.Errorf("race_log 时间跨度 = [%d,%d], want [1000,1004]", race.MinTS, race.MaxTS)
	}
}

// TestDBMaintenancePruneOlderThan 验证按龄清理只删早于 cutoff 的记录，且三表都被覆盖。
func TestDBMaintenancePruneOlderThan(t *testing.T) {
	m, _ := newTempDB(t)
	insertRace(t, m, 1000, 5) // ts 1000..1004

	deleted, err := m.PruneOlderThan(1003)
	if err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}
	if got := deleted["race_log"]; got != 3 {
		t.Errorf("应删除 3 条（ts<1003），got %d", got)
	}
	for _, name := range []string{"usage_log", "http_log"} {
		if _, ok := deleted[name]; !ok {
			t.Errorf("返回值应包含 %s 的删除数, got %v", name, deleted)
		}
	}
	st, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range st.Tables {
		if tb.Name == "race_log" && tb.Rows != 2 {
			t.Errorf("清理后 race_log 行数 = %d, want 2", tb.Rows)
		}
	}
}

// TestDBMaintenanceClear 验证整表清空与非法表名拒绝。
func TestDBMaintenanceClear(t *testing.T) {
	m, _ := newTempDB(t)
	insertRace(t, m, 1000, 4)

	n, err := m.Clear("race_log")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if n != 4 {
		t.Errorf("删除行数 = %d, want 4", n)
	}

	if _, err := m.Clear("nodes"); !errors.Is(err, ErrUnknownTable) {
		t.Errorf("非法表名应返回 ErrUnknownTable, got %v", err)
	}
	if _, err := m.Clear("race_log; DROP TABLE nodes"); !errors.Is(err, ErrUnknownTable) {
		t.Errorf("注入式表名应被白名单拒绝, got %v", err)
	}
}

// TestIncrementalVacuumSafeWithoutAutoVacuum 验证 auto_vacuum=NONE 时调用增量回收
// 不会报错（SQLite 直接忽略），也不会误报回收量。
func TestIncrementalVacuumSafeWithoutAutoVacuum(t *testing.T) {
	m, _ := newLegacyTempDB(t)
	if mode := autoVacuumMode(t, m); mode != AutoVacuumNone {
		t.Fatalf("前置条件：旧库应为 auto_vacuum=NONE, got %d", mode)
	}
	freed, err := m.IncrementalVacuum(100)
	if err != nil {
		t.Fatalf("IncrementalVacuum 不应报错: %v", err)
	}
	if freed != 0 {
		t.Errorf("未开启 auto_vacuum 时不应报告回收字节, got %d", freed)
	}
}

// TestFullVacuumRefusesWhenSpaceInsufficient 验证空间不足时返回 ErrInsufficientSpace
// 而不是中途失败。磁盘余量经 diskFreeFn 注入伪造。
func TestFullVacuumRefusesWhenSpaceInsufficient(t *testing.T) {
	m, _ := newTempDB(t)
	insertRace(t, m, 1000, 10)

	orig := diskFreeFn
	diskFreeFn = func(string) (diskfree.Space, error) { return diskfree.Space{Free: 1, Total: 100}, nil }
	t.Cleanup(func() { diskFreeFn = orig })

	_, err := m.FullVacuum()
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("应返回 ErrInsufficientSpace, got %v", err)
	}
	// 拒绝必须发生在执行前：数据仍然完整。
	st, statErr := m.Stats()
	if statErr != nil {
		t.Fatal(statErr)
	}
	for _, tb := range st.Tables {
		if tb.Name == "race_log" && tb.Rows != 10 {
			t.Errorf("拒绝后数据应完好，race_log 行数 = %d, want 10", tb.Rows)
		}
	}
}

// TestFullVacuumShrinksBloatedFile 验证完整 VACUUM 能把删除留下的空洞真正归还文件系统。
func TestFullVacuumShrinksBloatedFile(t *testing.T) {
	m, path := newTempDB(t)

	// 灌入带载荷的数据再删掉大部分，制造 freelist。
	for i := 0; i < 20000; i++ {
		if _, err := m.db.Exec(
			"INSERT INTO race_log (ts, req_id, node_uri, outcome) VALUES (?,?,?,?)",
			int64(i), "r", "vless://"+string(make([]byte, 300)), "win",
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := m.db.Exec("DELETE FROM race_log WHERE ts > 100"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	before := fileSizeOrZero(path)
	freed, err := m.FreelistBytes()
	if err != nil {
		t.Fatal(err)
	}
	if freed <= 0 {
		t.Fatalf("前置条件：应产生空闲页, got %d", freed)
	}

	if _, err := m.FullVacuum(); err != nil {
		t.Fatalf("FullVacuum: %v", err)
	}
	after := fileSizeOrZero(path)
	if after >= before {
		t.Errorf("VACUUM 后文件应缩小: before=%d after=%d", before, after)
	}
	if res, err := m.QuickCheck(); err != nil || res != "ok" {
		t.Errorf("VACUUM 后 quick_check = %q err=%v, want ok", res, err)
	}
}

// TestQuickCheckHealthy 验证健康库返回 ok。
func TestQuickCheckHealthy(t *testing.T) {
	m, _ := newTempDB(t)
	res, err := m.QuickCheck()
	if err != nil {
		t.Fatalf("QuickCheck: %v", err)
	}
	if res != "ok" {
		t.Errorf("QuickCheck = %q, want ok", res)
	}
}

// TestFileInUse 验证占用探测：持有句柄时报占用，释放后恢复。
// 仅 Windows 有实现（其余平台恒 false）。
func TestFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if FileInUse(path) {
			t.Error("非 Windows 平台应恒返回 false")
		}
		t.Skip("FileInUse 的占用语义仅在 Windows 生效")
	}

	if FileInUse(path) {
		t.Fatal("无人占用时不应报告占用")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !FileInUse(path) {
		t.Error("持有句柄时应报告占用")
	}
	_ = f.Close()
	if FileInUse(path) {
		t.Error("释放句柄后不应报告占用")
	}
	if FileInUse(filepath.Join(dir, "not-exist.txt")) {
		t.Error("文件不存在不应报告占用")
	}
}
