package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/diskfree"
)

// ErrInsufficientSpace 表示完整 VACUUM 所需临时空间不足（调用方据此返回 4xx 而非 5xx）。
var ErrInsufficientSpace = errors.New("insufficient disk space for VACUUM")

// ErrUnknownTable 表示请求操作的表不在日志表白名单内。
var ErrUnknownTable = errors.New("unknown table")

// PRAGMA auto_vacuum 的取值（与 SQLite 定义一致）。
const (
	AutoVacuumNone        = 0
	AutoVacuumFull        = 1
	AutoVacuumIncremental = 2
)

// diskFreeFn 读取磁盘余量。抽成变量是为了让测试构造"空间不足"场景
// （真实磁盘很难在小体量下凑出不足）。
//
//nolint:gochecknoglobals // 测试接缝
var diskFreeFn = diskfree.Of

// logTables 是有 ts 列、可按龄清理的日志表（按体积从大到小排列）。
//
//nolint:gochecknoglobals // Read-only table list
var logTables = []string{"race_log", "usage_log", "http_log"}

// statsTables 是体检展示的表；hasTS 标记是否有 ts 列（可算时间跨度）。
//
//nolint:gochecknoglobals // Read-only table list
var statsTables = []struct {
	name  string
	hasTS bool
}{
	{"race_log", true},
	{"usage_log", true},
	{"http_log", true},
	{"nodes", false},
	{"node_health", false},
	{"entry_nodes", false},
	{"entry_node_health", false},
}

// DBMaintenance 提供数据库体检与维护操作：体积/空闲页统计、按龄清理、清空、
// 空间回收与完整性检查。
//
// 与 UsageStore / HTTPLogStore 共享同一个 *sql.DB（生产装配下三者是同一连接池，
// 池上限为 1），因此不做任何连接管理；本类型不持有所有权，Close 责任仍在 main。
type DBMaintenance struct {
	db   *sql.DB
	path string
}

// NewDBMaintenance 装配维护器。dbPath 用于取文件实际体积与磁盘余量。
func NewDBMaintenance(database *sql.DB, dbPath string) *DBMaintenance {
	return &DBMaintenance{db: database, path: dbPath}
}

// TableStat 是单张表的行数与时间跨度（无 ts 列时 MinTS/MaxTS 为 0）。
type TableStat struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	MinTS int64  `json:"min_ts"`
	MaxTS int64  `json:"max_ts"`
}

// DBStats 是数据库体检快照。
type DBStats struct {
	Path           string      `json:"path"`
	FileBytes      int64       `json:"file_bytes"`
	WALBytes       int64       `json:"wal_bytes"`
	SHMBytes       int64       `json:"shm_bytes"`
	PageSize       int64       `json:"page_size"`
	PageCount      int64       `json:"page_count"`
	FreelistPages  int64       `json:"freelist_pages"`
	FreelistBytes  int64       `json:"freelist_bytes"`
	ReclaimRatio   float64     `json:"reclaim_ratio"` // 空闲页占比（0~1），即文件可回收比例
	AutoVacuum     int         `json:"auto_vacuum"`   // 0=none 1=full 2=incremental
	JournalMode    string      `json:"journal_mode"`
	DiskFreeBytes  int64       `json:"disk_free_bytes"`
	DiskTotalBytes int64       `json:"disk_total_bytes"`
	Tables         []TableStat `json:"tables"`
}

// Stats 汇总数据库体检信息。
//
// 磁盘余量与 WAL/SHM 体积取不到时按 0 返回（不影响其余字段），PRAGMA 或行数
// 统计失败则返回错误——那些字段是判断能否回收空间的依据，不能静默缺省。
func (m *DBMaintenance) Stats() (DBStats, error) {
	s := DBStats{Path: m.path, Tables: make([]TableStat, 0, len(statsTables))}

	if err := m.db.QueryRow("PRAGMA page_size").Scan(&s.PageSize); err != nil {
		return s, fmt.Errorf("error: %w", err)
	}
	if err := m.db.QueryRow("PRAGMA page_count").Scan(&s.PageCount); err != nil {
		return s, fmt.Errorf("error: %w", err)
	}
	if err := m.db.QueryRow("PRAGMA freelist_count").Scan(&s.FreelistPages); err != nil {
		return s, fmt.Errorf("error: %w", err)
	}
	if err := m.db.QueryRow("PRAGMA auto_vacuum").Scan(&s.AutoVacuum); err != nil {
		return s, fmt.Errorf("error: %w", err)
	}
	if err := m.db.QueryRow("PRAGMA journal_mode").Scan(&s.JournalMode); err != nil {
		return s, fmt.Errorf("error: %w", err)
	}

	s.FreelistBytes = s.FreelistPages * s.PageSize
	if total := s.PageCount * s.PageSize; total > 0 {
		s.ReclaimRatio = float64(s.FreelistBytes) / float64(total)
	}

	s.FileBytes = fileSizeOrZero(m.path)
	s.WALBytes = fileSizeOrZero(m.path + "-wal")
	s.SHMBytes = fileSizeOrZero(m.path + "-shm")
	if sp, err := diskFreeFn(m.path); err == nil {
		s.DiskFreeBytes, s.DiskTotalBytes = sp.Free, sp.Total
	}

	for _, t := range statsTables {
		st := TableStat{Name: t.name}
		query := "SELECT COUNT(*) FROM " + t.name
		if t.hasTS {
			query = "SELECT COUNT(*), COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM " + t.name
			if err := m.db.QueryRow(query).Scan(&st.Rows, &st.MinTS, &st.MaxTS); err != nil {
				return s, fmt.Errorf("error: %w", err)
			}
		} else if err := m.db.QueryRow(query).Scan(&st.Rows); err != nil {
			return s, fmt.Errorf("error: %w", err)
		}
		s.Tables = append(s.Tables, st)
	}
	return s, nil
}

// PruneOlderThan 删除三张日志表中 ts < cutoff 的记录，返回各表删除行数。
func (m *DBMaintenance) PruneOlderThan(cutoff int64) (map[string]int64, error) {
	out := make(map[string]int64, len(logTables))
	for _, t := range logTables {
		res, err := m.db.Exec("DELETE FROM "+t+" WHERE ts < ?", cutoff)
		if err != nil {
			return out, fmt.Errorf("error: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return out, fmt.Errorf("error: %w", err)
		}
		out[t] = n
	}
	return out, nil
}

// PruneFaultLog 按固定保留期（FaultLogRetentionDays，1 天）清理故障记录表。
// 单独成方法而非并入 PruneOlderThan：日志表按 ts 判龄，故障表按 last_seen_at，
// 且两者保留窗口口径不同（可配置 vs 固定 1 天）。
func (m *DBMaintenance) PruneFaultLog(now int64) (int64, error) {
	return PruneFaultLog(m.db, FaultLogCutoff(now))
}

// Clear 清空指定日志表，返回删除行数。table 取 usage_log / race_log / http_log / all。
func (m *DBMaintenance) Clear(table string) (int64, error) {
	if table == "all" {
		var total int64
		for _, t := range logTables {
			n, err := m.Clear(t)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	if !isLogTable(table) {
		return 0, fmt.Errorf("%w: %q", ErrUnknownTable, table)
	}
	res, err := m.db.Exec("DELETE FROM " + table)
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return n, nil
}

// IncrementalVacuum 执行 PRAGMA incremental_vacuum，把空闲页归还文件系统，
// 返回实际回收的**字节数**。pages <= 0 表示不限量（回收全部空闲页）。
//
// 在线执行、不需要额外磁盘空间，但仅当库的 auto_vacuum=INCREMENTAL 时才真正回收；
// auto_vacuum=NONE 时 SQLite 直接忽略该指令（不报错、页数不变），故本方法恒安全。
//
// 关键实现细节：incremental_vacuum **每步只释放一页**，且把每次释放作为一行结果返回。
// 必须用 Query 并把结果集遍历干净才会完成全部回收——用 Exec 只会前进一步、回收一页
// （实测：716 个空闲页经 Exec 后仍剩 715）。这是"接口成功但空间没回收"的根源。
func (m *DBMaintenance) IncrementalVacuum(pages int) (int64, error) {
	before, err := m.freelistPages()
	if err != nil {
		return 0, err
	}
	stmt := "PRAGMA incremental_vacuum"
	if pages > 0 {
		// PRAGMA 不接受占位符绑定，只能拼接；pages 是 int，无注入面。
		stmt += "(" + strconv.Itoa(pages) + ")"
	}
	rows, err := m.db.Query(stmt)
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	for rows.Next() { //nolint:revive // 必须遍历：每一步释放一页
	}
	drainErr := rows.Err()
	_ = rows.Close()
	if drainErr != nil {
		return 0, fmt.Errorf("error: %w", drainErr)
	}

	after, err := m.freelistPages()
	if err != nil {
		return 0, err
	}
	if before <= after {
		return 0, nil
	}
	var pageSize int64
	if err := m.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return (before - after) * pageSize, nil
}

// FullVacuum 执行完整 VACUUM 重建数据库文件，返回随后的 checkpoint 结果。
//
// VACUUM 需要与当前库等量的临时空间（SQLite 先写副本再替换），因此先检查磁盘余量：
// 不足时返回包装了 ErrInsufficientSpace 的错误，避免在中途失败留下半成品。
func (m *DBMaintenance) FullVacuum() (CheckpointResult, error) {
	if err := m.checkVacuumSpace(); err != nil {
		return CheckpointResult{}, err
	}
	if _, err := m.db.Exec("VACUUM"); err != nil {
		return CheckpointResult{}, fmt.Errorf("error: %w", err)
	}
	// WAL 模式下 VACUUM 的重建结果先写进 WAL，主库文件要到 checkpoint 才截断；
	// 不做这一步文件体积不会变小，用户会以为回收失败。
	return m.Checkpoint(true)
}

// checkVacuumSpace 校验 VACUUM 所需的临时空间（约 1.1× 当前文件大小，VACUUM 期间
// 还会产生日志与临时页）。空间不足时返回包装了 ErrInsufficientSpace 的错误。
func (m *DBMaintenance) checkVacuumSpace() error {
	fi, err := os.Stat(m.path)
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	need := fi.Size() + fi.Size()/10
	if sp, err := diskFreeFn(m.path); err == nil && sp.Free < need {
		return fmt.Errorf("%w：需要约 %s，当前可用 %s；请改用离线压缩（scripts/压缩数据库.bat 或 vertex-proxy vacuum）",
			ErrInsufficientSpace, humanBytes(need), humanBytes(sp.Free))
	}
	return nil
}

// CheckpointResult 是 WAL checkpoint 的真实结果（不忽略失败与忙碌）。
//
// 口径说明：WAL 体积与主库体积是两件事。checkpoint 只把 WAL 内容并入主库并截断
// WAL，因此 WAL 缩小不等于主库缩小；两者分别上报，避免把 WAL 的变化误算成回收量。
type CheckpointResult struct {
	Applicable        bool  `json:"applicable"`         // false = 非 WAL 模式，无 WAL 可 checkpoint
	Busy              bool  `json:"busy"`               // true = 有其他连接在读，未能完全 checkpoint
	LogPages          int64 `json:"log_pages"`          // WAL 中的总页数
	CheckpointedPages int64 `json:"checkpointed_pages"` // 本次写入主库的页数
	WALBytesBefore    int64 `json:"wal_bytes_before"`
	WALBytesAfter     int64 `json:"wal_bytes_after"`
}

// Checkpoint 执行 WAL checkpoint 并返回真实结果。
// truncate=true 使用 wal_checkpoint(TRUNCATE)，成功后 WAL 截断到 0，主库体积才反映真实状态。
// 非 WAL 库返回 Applicable=false 且不报错；checkpoint 失败/无返回行则返回错误而非静默忽略。
func (m *DBMaintenance) Checkpoint(truncate bool) (CheckpointResult, error) {
	res := CheckpointResult{WALBytesBefore: fileSizeOrZero(m.path + "-wal")}

	var mode string
	if err := m.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return res, fmt.Errorf("error: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		res.WALBytesAfter = res.WALBytesBefore
		return res, nil
	}
	res.Applicable = true

	stmt := "PRAGMA wal_checkpoint(PASSIVE)"
	if truncate {
		stmt = "PRAGMA wal_checkpoint(TRUNCATE)"
	}
	var busy, logPages, checkpointed int64
	err := m.db.QueryRow(stmt).Scan(&busy, &logPages, &checkpointed)
	if errors.Is(err, sql.ErrNoRows) {
		// WAL 库正常情况下必有一行；无行说明驱动/版本异常，如实上报而不是假装成功。
		return res, fmt.Errorf("wal_checkpoint 未返回结果（journal_mode=%s）", mode)
	}
	if err != nil {
		return res, fmt.Errorf("error: %w", err)
	}
	res.Busy = busy != 0
	res.LogPages = logPages
	res.CheckpointedPages = checkpointed
	res.WALBytesAfter = fileSizeOrZero(m.path + "-wal")
	return res, nil
}

// ApplyIncrementalAutoVacuum 在当前库上设置 auto_vacuum=INCREMENTAL 并重建页面结构，
// 最后校验模式确实写入头部。
//
// auto_vacuum 只在 VACUUM 重建时生效，且 PRAGMA 与 VACUUM 必须落在同一条连接上，
// 因此这里固定一条 sql.Conn 执行（连接池上限为 1，但 Conn 能排除中途被回收的可能）。
// 本方法不做磁盘余量检查（由 EnableIncrementalAutoVacuum 负责），调用方需保证
// 没有其他连接正在写该库。
func (m *DBMaintenance) ApplyIncrementalAutoVacuum(ctx context.Context) error {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	var mode int
	if err := conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	if mode != AutoVacuumIncremental {
		return fmt.Errorf("auto_vacuum 未生效（当前值 %d）", mode)
	}
	return nil
}

// EnableIncrementalAutoVacuum 是"旧库一次性迁移"入口：把 auto_vacuum=NONE 的既有库
// 转为 INCREMENTAL。已处于 INCREMENTAL 时直接返回 (false, nil)，不重复重建。
//
// 顺序：磁盘余量检查 → PRAGMA + VACUUM 重建 → checkpoint。VACUUM 会顺带压掉历史
// 删除留下的空洞，因此这一次迁移同时完成"开启增量回收"与"压缩文件"。
// 空间不足时返回 ErrInsufficientSpace 且不改动数据库，调用方应引导用户走离线压缩。
func (m *DBMaintenance) EnableIncrementalAutoVacuum() (bool, error) {
	var mode int
	if err := m.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return false, fmt.Errorf("error: %w", err)
	}
	if mode == AutoVacuumIncremental {
		return false, nil
	}
	if err := m.checkVacuumSpace(); err != nil {
		return false, err
	}
	if err := m.ApplyIncrementalAutoVacuum(context.Background()); err != nil {
		return false, err
	}
	// 重建已完成，此后的 checkpoint 失败不应让调用方以为"整个迁移没发生"，
	// 因此单独标注：模式已切换，只是 WAL 尚未截断。
	if _, err := m.Checkpoint(true); err != nil {
		return true, fmt.Errorf("重建已完成（auto_vacuum 已切换），但 checkpoint 失败：%w", err)
	}
	return true, nil
}

// ReclaimOutcome 描述「空间回收」这一步的最终结论。
// 前端据此区分"没有空闲页可回收""当前模式不支持增量回收""已迁移并压缩"等情形，
// 避免把"接口成功但回收 0 字节"显示成成功释放空间。
type ReclaimOutcome string

const (
	ReclaimDone             ReclaimOutcome = "reclaimed"            // 增量回收归还了空闲页
	ReclaimNoFreePages      ReclaimOutcome = "no_free_pages"        // 已是 INCREMENTAL，但没有空闲页
	ReclaimModeNotSupported ReclaimOutcome = "mode_not_incremental" // auto_vacuum 非 INCREMENTAL，增量回收无效
	ReclaimMigrated         ReclaimOutcome = "migrated"             // 一次性迁移到 INCREMENTAL 并重建（同时压缩）
	ReclaimSkipped          ReclaimOutcome = "skipped"              // 未请求回收
	ReclaimNoEffect         ReclaimOutcome = "no_effect"            // 有空闲页却一页未归还（模式未真正生效）
)

// MaintainOptions 是一键「清理并回收空间」的入参。
type MaintainOptions struct {
	// Days > 0 时删除三张日志表中早于 now-Days 天的记录；<=0 表示不动数据（纯回收空间）。
	Days int
	// InitIncremental 为 true 时，若库不是 INCREMENTAL 则执行一次性迁移（含 VACUUM）。
	InitIncremental bool
}

// StageResult 是一键操作中单个阶段的执行结果。
type StageResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MaintainResult 是一键操作的完整结果：分阶段结果 + 前后快照 + 实际回收口径。
//
// 回收口径：ReclaimedBytes 只统计**主库文件**的减少量，WAL/SHM 的变化在 Checkpoint
// 字段里单独上报——checkpoint 会把 WAL 并入主库并截断 WAL，把 WAL 缩小算作主库
// 缩小会虚报回收量。
type MaintainResult struct {
	OK                 bool             `json:"ok"`
	Message            string           `json:"message"`
	FailedStage        string           `json:"failed_stage,omitempty"`
	Stages             []StageResult    `json:"stages"`
	StatsBefore        DBStats          `json:"stats_before"`
	StatsAfter         DBStats          `json:"stats_after"`
	Deleted            map[string]int64 `json:"deleted"`
	DeletedTotal       int64            `json:"deleted_total"`
	ReclaimOutcome     ReclaimOutcome   `json:"reclaim_outcome"`
	ReclaimedBytes     int64            `json:"reclaimed_bytes"`      // 主库文件减少量（不含 WAL）
	FreelistFreedBytes int64            `json:"freelist_freed_bytes"` // 空闲页减少量
	Checkpoint         CheckpointResult `json:"checkpoint"`
	AutoVacuumBefore   int              `json:"auto_vacuum_before"`
	AutoVacuumAfter    int              `json:"auto_vacuum_after"`
	QuickCheck         string           `json:"quick_check"`
}

// Maintain 执行一键「清理并回收空间」：按保留期清理 → 空间回收 → checkpoint → 完整性校验，
// 并给出前后快照与真实回收量。
//
// 失败语义：任一步失败都返回 OK=false、FailedStage=失败阶段名、Message=原因，
// 已完成的阶段保留在 Stages 里（调用方据此回报"做到哪一步"）。不谎报全流程成功。
// 删除历史只在 Days>0 时发生，调用方必须先取得用户确认。
func (m *DBMaintenance) Maintain(opts MaintainOptions) MaintainResult {
	res := MaintainResult{Deleted: map[string]int64{}, Stages: make([]StageResult, 0, 6)}

	fail := func(stage string, err error) MaintainResult {
		res.OK = false
		res.FailedStage = stage
		res.Message = err.Error()
		res.Stages = append(res.Stages, StageResult{Name: stage, OK: false, Detail: err.Error()})
		return res
	}
	done := func(stage, detail string) {
		res.Stages = append(res.Stages, StageResult{Name: stage, OK: true, Detail: detail})
	}

	// ── 1. 回收前快照 ──
	before, err := m.Stats()
	if err != nil {
		return fail("stats_before", fmt.Errorf("读取回收前统计失败：%w", err))
	}
	res.StatsBefore = before
	res.AutoVacuumBefore = before.AutoVacuum
	done("stats_before", fmt.Sprintf("主库 %s，WAL %s，空闲页 %s",
		humanBytes(before.FileBytes), humanBytes(before.WALBytes), humanBytes(before.FreelistBytes)))

	// ── 2. 按保留期清理（Days<=0 时不动任何数据）──
	if opts.Days > 0 {
		cutoff := time.Now().Unix() - int64(opts.Days)*86400
		deleted, derr := m.PruneOlderThan(cutoff)
		if derr != nil {
			return fail("prune", fmt.Errorf("清理失败：%w", derr))
		}
		res.Deleted = deleted
		for _, n := range deleted {
			res.DeletedTotal += n
		}
		// 故障记录按固定 1 天保留期清理（评审要求），与本选项的天数无关：
		// 它只用于即时排查，不随"清理 N 天以前"的语义被误留或误删。
		if n, ferr := m.PruneFaultLog(time.Now().Unix()); ferr != nil {
			return fail("prune", fmt.Errorf("故障记录清理失败：%w", ferr))
		} else if n > 0 {
			res.Deleted["node_fault_log"] = n
			res.DeletedTotal += n
		}
		done("prune", fmt.Sprintf("删除 %d 行（%d 天以前，故障记录固定 %d 天）", res.DeletedTotal, opts.Days, FaultLogRetentionDays))
	} else {
		done("prune", "未请求清理历史（Days<=0），仅回收空间")
	}

	// ── 3. 空间回收 ──
	switch {
	case before.AutoVacuum == AutoVacuumIncremental:
		if _, verr := m.IncrementalVacuum(0); verr != nil {
			return fail("reclaim", fmt.Errorf("增量回收失败：%w", verr))
		}
		switch {
		case before.FreelistBytes > 0:
			res.ReclaimOutcome = ReclaimDone
			done("reclaim", fmt.Sprintf("增量回收 %s 空闲页", humanBytes(before.FreelistBytes)))
		case before.FreelistBytes == 0:
			res.ReclaimOutcome = ReclaimNoFreePages
			done("reclaim", "没有空闲页可回收（无需回收）")
		}
	case opts.InitIncremental:
		if _, verr := m.EnableIncrementalAutoVacuum(); verr != nil {
			return fail("reclaim", fmt.Errorf("一次性初始化失败：%w", verr))
		}
		res.ReclaimOutcome = ReclaimMigrated
		done("reclaim", "已迁移到 auto_vacuum=INCREMENTAL 并重建（同时压缩文件）")
	default:
		res.ReclaimOutcome = ReclaimModeNotSupported
		done("reclaim", fmt.Sprintf("当前 auto_vacuum=%s，增量回收无法归还空间；如需回收请勾选一次性初始化，或停服运行离线压缩",
			autoVacuumName(before.AutoVacuum)))
	}

	// ── 4. WAL checkpoint（失败/忙碌如实上报，不忽略）──
	cp, cerr := m.Checkpoint(true)
	if cerr != nil {
		return fail("checkpoint", fmt.Errorf("WAL checkpoint 失败：%w", cerr))
	}
	res.Checkpoint = cp
	switch {
	case !cp.Applicable:
		done("checkpoint", "非 WAL 模式，无需 checkpoint")
	case cp.Busy:
		done("checkpoint", fmt.Sprintf("checkpoint 受阻（有连接在读）：WAL 仍有 %s 未并入主库", humanBytes(cp.WALBytesAfter)))
	default:
		done("checkpoint", fmt.Sprintf("WAL %s → %s", humanBytes(cp.WALBytesBefore), humanBytes(cp.WALBytesAfter)))
	}

	// ── 5. 完整性校验 ──
	qc, qerr := m.QuickCheck()
	if qerr != nil {
		return fail("verify", fmt.Errorf("完整性检查失败：%w", qerr))
	}
	res.QuickCheck = qc
	if qc != "ok" {
		return fail("verify", fmt.Errorf("完整性检查未通过：%s", qc))
	}
	done("verify", "quick_check = ok")

	// ── 6. 回收后快照与口径计算 ──
	after, aerr := m.Stats()
	if aerr != nil {
		return fail("stats_after", fmt.Errorf("读取回收后统计失败：%w", aerr))
	}
	res.StatsAfter = after
	res.AutoVacuumAfter = after.AutoVacuum
	if d := before.FileBytes - after.FileBytes; d > 0 {
		res.ReclaimedBytes = d
	}
	if d := before.FreelistBytes - after.FreelistBytes; d > 0 {
		res.FreelistFreedBytes = d
	}
	// INCREMENTAL 且事前有空闲页，却既没归还空闲页也没缩小文件：模式未真正生效，
	// 如实降级为 no_effect，不报成功回收。
	if res.ReclaimOutcome == ReclaimDone && res.FreelistFreedBytes == 0 && res.ReclaimedBytes == 0 {
		res.ReclaimOutcome = ReclaimNoEffect
		res.Stages = append(res.Stages, StageResult{Name: "reclaim", OK: false,
			Detail: "增量回收未归还任何页（模式未真正生效），主库文件未缩小"})
	}
	done("stats_after", fmt.Sprintf("主库 %s，WAL %s", humanBytes(after.FileBytes), humanBytes(after.WALBytes)))

	res.OK = true
	res.Message = maintainMessage(res)
	return res
}

// maintainMessage 生成一句人类可读的结论，措辞严格对应实际回收结果。
func maintainMessage(res MaintainResult) string {
	var b strings.Builder
	if res.DeletedTotal > 0 {
		fmt.Fprintf(&b, "已删除 %d 行超期记录；", res.DeletedTotal)
	} else if res.StatsBefore.Tables != nil {
		b.WriteString("无超期记录可删；")
	}
	switch res.ReclaimOutcome {
	case ReclaimDone:
		fmt.Fprintf(&b, "回收 %s（主库 %s → %s）",
			humanBytes(res.ReclaimedBytes), humanBytes(res.StatsBefore.FileBytes), humanBytes(res.StatsAfter.FileBytes))
	case ReclaimMigrated:
		fmt.Fprintf(&b, "已开启增量回收并压缩：主库 %s → %s",
			humanBytes(res.StatsBefore.FileBytes), humanBytes(res.StatsAfter.FileBytes))
	case ReclaimNoFreePages:
		b.WriteString("没有空闲页需要回收")
	case ReclaimModeNotSupported:
		fmt.Fprintf(&b, "未回收空间：auto_vacuum=%s 不支持增量回收", autoVacuumName(res.AutoVacuumBefore))
	case ReclaimNoEffect:
		b.WriteString("未回收空间：增量回收未生效")
	case ReclaimSkipped:
		b.WriteString("未执行空间回收")
	}
	if res.Checkpoint.Busy {
		b.WriteString("；WAL checkpoint 受阻（有连接在读）")
	}
	return b.String()
}

// autoVacuumName 把 auto_vacuum 取值翻成可读名（与前端口径一致）。
func autoVacuumName(v int) string {
	switch v {
	case AutoVacuumNone:
		return "NONE"
	case AutoVacuumFull:
		return "FULL"
	case AutoVacuumIncremental:
		return "INCREMENTAL"
	default:
		return strconv.Itoa(v)
	}
}

// QuickCheck 执行 PRAGMA quick_check，返回 "ok" 或拼接后的错误明细。
func (m *DBMaintenance) QuickCheck() (string, error) {
	rows, err := m.db.Query("PRAGMA quick_check")
	if err != nil {
		return "", fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var parts []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("error: %w", err)
		}
		parts = append(parts, line)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("error: %w", err)
	}
	if len(parts) == 0 {
		return "ok", nil
	}
	return strings.Join(parts, "; "), nil
}

// FreelistBytes 返回当前空闲页占用字节数（供上层在回收前后对比）。
func (m *DBMaintenance) FreelistBytes() (int64, error) {
	pages, err := m.freelistPages()
	if err != nil {
		return 0, err
	}
	var pageSize int64
	if err := m.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return pages * pageSize, nil
}

func (m *DBMaintenance) freelistPages() (int64, error) {
	var n int64
	if err := m.db.QueryRow("PRAGMA freelist_count").Scan(&n); err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return n, nil
}

func isLogTable(table string) bool {
	for _, t := range logTables {
		if t == table {
			return true
		}
	}
	return false
}

func fileSizeOrZero(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// humanBytes 把字节数格式化为易读文本（仅用于面向用户的错误信息）。
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
