package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/diskfree"
)

// insertBloat 插入 n 条带长载荷的 race_log 并随后删除，制造空闲页（模拟历史删除留下的空洞）。
func insertBloat(t *testing.T, m *DBMaintenance, ts int64, n int) {
	t.Helper()
	payload := strings.Repeat("x", 400)
	for i := 0; i < n; i++ {
		if _, err := m.db.Exec(
			"INSERT INTO race_log (ts, req_id, node_uri, outcome) VALUES (?,?,?,?)",
			ts, "bloat", payload, "win",
		); err != nil {
			t.Fatalf("insert bloat: %v", err)
		}
	}
	if _, err := m.db.Exec("DELETE FROM race_log WHERE req_id = 'bloat'"); err != nil {
		t.Fatalf("delete bloat: %v", err)
	}
}

// countRace 统计 race_log 行数。
func countRace(t *testing.T, m *DBMaintenance) int64 {
	t.Helper()
	var n int64
	if err := m.db.QueryRow("SELECT COUNT(*) FROM race_log").Scan(&n); err != nil {
		t.Fatalf("count race_log: %v", err)
	}
	return n
}

// TestOpenFreshDBEnablesIncremental 验证新建库在 schema 初始化前即启用增量回收，
// 使此后删除留下的空闲页可由增量回收归还。
func TestOpenFreshDBEnablesIncremental(t *testing.T) {
	m, _ := newTempDB(t)
	if got := autoVacuumMode(t, m); got != AutoVacuumIncremental {
		t.Fatalf("新建库 auto_vacuum = %d, want %d(INCREMENTAL)", got, AutoVacuumIncremental)
	}
}

// TestOpenExistingLegacyDBKeepsNoneMode 验证 Open 不会擅自改动既有库：
// 非空库切换 auto_vacuum 必须靠 VACUUM 重建，属于显式维护流程，不能每次启动都做。
func TestOpenExistingLegacyDBKeepsNoneMode(t *testing.T) {
	legacy, path := newLegacyTempDB(t)
	insertRace(t, legacy, 1000, 3)
	if err := legacy.db.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	m := NewDBMaintenance(reopened, path)
	if got := autoVacuumMode(t, m); got != AutoVacuumNone {
		t.Fatalf("既有库被 Open 改动了 auto_vacuum = %d, want 0(NONE)", got)
	}
	if n := countRace(t, m); n != 3 {
		t.Fatalf("既有数据应保留，race_log 行数 = %d, want 3", n)
	}
}

// TestEnableIncrementalAutoVacuumMigratesLegacyDB 验证旧库一次性迁移：
// 模式切到 INCREMENTAL，并顺带把历史删除留下的空洞压掉。
func TestEnableIncrementalAutoVacuumMigratesLegacyDB(t *testing.T) {
	m, path := newLegacyTempDB(t)
	insertBloat(t, m, 1000, 3000) // 灌入再删除 → 大量空闲页

	before, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if before.FreelistBytes <= 0 {
		t.Fatalf("前置条件：应产生空闲页, got %d", before.FreelistBytes)
	}

	changed, err := m.EnableIncrementalAutoVacuum()
	if err != nil {
		t.Fatalf("EnableIncrementalAutoVacuum: %v", err)
	}
	if !changed {
		t.Error("旧库迁移应报告 changed=true")
	}
	if got := autoVacuumMode(t, m); got != AutoVacuumIncremental {
		t.Fatalf("迁移后 auto_vacuum = %d, want 2(INCREMENTAL)", got)
	}

	after, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if after.FreelistBytes != 0 {
		t.Errorf("迁移后空闲页应清零, got %d", after.FreelistBytes)
	}
	if after.FileBytes >= before.FileBytes {
		t.Errorf("迁移应压掉空洞: before=%d after=%d", before.FileBytes, after.FileBytes)
	}
	if fileSizeOrZero(path) != after.FileBytes {
		t.Errorf("文件实际大小与统计不一致: stat=%d stats=%d", fileSizeOrZero(path), after.FileBytes)
	}
	if res, err := m.QuickCheck(); err != nil || res != "ok" {
		t.Errorf("迁移后 quick_check = %q err=%v, want ok", res, err)
	}
}

// TestEnableIncrementalAutoVacuumIsIdempotent 验证已处于 INCREMENTAL 时不重复重建。
func TestEnableIncrementalAutoVacuumIsIdempotent(t *testing.T) {
	m, _ := newTempDB(t) // 新建库已是 INCREMENTAL
	changed, err := m.EnableIncrementalAutoVacuum()
	if err != nil {
		t.Fatalf("EnableIncrementalAutoVacuum: %v", err)
	}
	if changed {
		t.Error("已是 INCREMENTAL 时不应报告 changed=true（不应重复重建）")
	}
}

// TestEnableIncrementalAutoVacuumRefusesWithoutSpace 验证磁盘不足时拒绝迁移，
// 且不改动数据库（模式与数据都保持原样）。
func TestEnableIncrementalAutoVacuumRefusesWithoutSpace(t *testing.T) {
	m, _ := newLegacyTempDB(t)
	insertRace(t, m, 1000, 5)

	orig := diskFreeFn
	diskFreeFn = func(string) (diskfree.Space, error) { return diskfree.Space{Free: 1, Total: 100}, nil }
	t.Cleanup(func() { diskFreeFn = orig })

	changed, err := m.EnableIncrementalAutoVacuum()
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("应返回 ErrInsufficientSpace, got %v", err)
	}
	if changed {
		t.Error("拒绝时不应报告 changed=true")
	}
	if got := autoVacuumMode(t, m); got != AutoVacuumNone {
		t.Errorf("拒绝后模式应保持 NONE, got %d", got)
	}
	if n := countRace(t, m); n != 5 {
		t.Errorf("拒绝后数据应完好, race_log 行数 = %d, want 5", n)
	}
}

// TestIncrementalVacuumReclaimsOnIncrementalDB 验证 INCREMENTAL 库上增量回收
// 确实归还空闲页，并在 checkpoint 后真正缩小主库文件（成功缩小的正向用例）。
func TestIncrementalVacuumReclaimsOnIncrementalDB(t *testing.T) {
	m, path := newTempDB(t)
	insertBloat(t, m, 1000, 3000)

	before := fileSizeOrZero(path)
	freed, err := m.IncrementalVacuum(0)
	if err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("INCREMENTAL 库上应归还字节, got %d", freed)
	}
	if pages, err := m.freelistPages(); err != nil || pages != 0 {
		t.Fatalf("回收后空闲页应清零, got %d err=%v", pages, err)
	}
	// 回收只改页结构，主库文件的实际截断发生在 checkpoint 之后：这正是
	// "WAL 体积变化不等于主库体积变化"的口径来源，也是一键操作把 checkpoint
	// 列为独立阶段并上报其结果的原因。
	if _, err := m.Checkpoint(true); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got := fileSizeOrZero(path); got >= before {
		t.Errorf("checkpoint 后主库文件应缩小: before=%d after=%d", before, got)
	}
}

// TestMaintainReclaimsAndPreservesRetentionWindow 验证一键操作：
// 只删保留期外的记录、保留期内的不动，并真实回收空间。
func TestMaintainReclaimsAndPreservesRetentionWindow(t *testing.T) {
	m, _ := newTempDB(t)
	now := time.Now().Unix()
	insertRace(t, m, now-86400, 5)        // 1 天前：保留期内
	insertRace(t, m, now-30*86400, 50)    // 30 天前：超期
	insertBloat(t, m, now-40*86400, 3000) // 空洞

	res := m.Maintain(MaintainOptions{Days: 7})
	if !res.OK {
		t.Fatalf("Maintain 失败于阶段 %q: %s", res.FailedStage, res.Message)
	}
	if got := res.Deleted["race_log"]; got != 50 {
		t.Errorf("应删除 50 行超期记录, got %d（deleted=%v）", got, res.Deleted)
	}
	if res.DeletedTotal != 50 {
		t.Errorf("DeletedTotal = %d, want 50", res.DeletedTotal)
	}
	if n := countRace(t, m); n != 5 {
		t.Errorf("保留期内记录不应被删, 剩余 = %d, want 5", n)
	}
	if res.ReclaimOutcome != ReclaimDone {
		t.Errorf("ReclaimOutcome = %q, want %q", res.ReclaimOutcome, ReclaimDone)
	}
	if res.ReclaimedBytes <= 0 {
		t.Errorf("应报告实际回收字节, got %d", res.ReclaimedBytes)
	}
	if res.StatsAfter.FileBytes >= res.StatsBefore.FileBytes {
		t.Errorf("回收后主库文件应缩小: before=%d after=%d",
			res.StatsBefore.FileBytes, res.StatsAfter.FileBytes)
	}
	if res.QuickCheck != "ok" {
		t.Errorf("QuickCheck = %q, want ok", res.QuickCheck)
	}
	if res.AutoVacuumAfter != AutoVacuumIncremental {
		t.Errorf("AutoVacuumAfter = %d, want 2", res.AutoVacuumAfter)
	}
	// 阶段齐全（含 checkpoint 与前后快照），供前端展示进度。
	want := []string{"stats_before", "prune", "reclaim", "checkpoint", "verify", "stats_after"}
	if len(res.Stages) != len(want) {
		t.Fatalf("阶段数 = %d, want %d: %+v", len(res.Stages), len(want), res.Stages)
	}
	for i, name := range want {
		if res.Stages[i].Name != name {
			t.Errorf("阶段[%d] = %q, want %q", i, res.Stages[i].Name, name)
		}
	}
}

// TestMaintainWithoutDaysDoesNotDelete 验证 Days=0 时纯回收空间、不删任何数据。
func TestMaintainWithoutDaysDoesNotDelete(t *testing.T) {
	m, _ := newTempDB(t)
	now := time.Now().Unix()
	insertRace(t, m, now-365*86400, 20) // 一年前，若按保留期本应被删
	insertBloat(t, m, now-400*86400, 1000)

	res := m.Maintain(MaintainOptions{Days: 0})
	if !res.OK {
		t.Fatalf("Maintain 失败: %s", res.Message)
	}
	if res.DeletedTotal != 0 {
		t.Errorf("Days=0 不应删除任何记录, got %d", res.DeletedTotal)
	}
	if n := countRace(t, m); n != 20 {
		t.Errorf("数据应原样保留, got %d want 20", n)
	}
}

// TestMaintainOnNonIncrementalDoesNotClaimReclaim 验证旧库（NONE）上的一键操作
// 不会把"回收 0 字节"报成成功释放空间，而是明确给出模式不支持的结论。
func TestMaintainOnNonIncrementalDoesNotClaimReclaim(t *testing.T) {
	m, path := newLegacyTempDB(t)
	now := time.Now().Unix()
	insertRace(t, m, now-30*86400, 10)
	insertBloat(t, m, now-40*86400, 2000)

	before := fileSizeOrZero(path)

	res := m.Maintain(MaintainOptions{Days: 7})
	if !res.OK {
		t.Fatalf("Maintain 失败: %s", res.Message)
	}
	if res.ReclaimOutcome != ReclaimModeNotSupported {
		t.Errorf("ReclaimOutcome = %q, want %q", res.ReclaimOutcome, ReclaimModeNotSupported)
	}
	if res.ReclaimedBytes != 0 {
		t.Errorf("NONE 库不应报告回收字节, got %d", res.ReclaimedBytes)
	}
	// 文件大小会因 prune/checkpoint 正常波动（WAL 并入主库），故只断言"没有回收"。
	if before == 0 {
		t.Fatal("前置条件：文件应有体积")
	}
	if !strings.Contains(res.Message, "未回收") {
		t.Errorf("结论应明确说明未回收空间, got %q", res.Message)
	}
	// 清理本身仍应生效。
	if res.DeletedTotal != 10 {
		t.Errorf("超期记录仍应被删, got %d", res.DeletedTotal)
	}
	if res.AutoVacuumAfter != AutoVacuumNone {
		t.Errorf("AutoVacuumAfter = %d, want 0", res.AutoVacuumAfter)
	}
}

// TestMaintainWithInitMigratesAndCompacts 验证一键操作勾选一次性初始化后：
// 旧库迁移到 INCREMENTAL 且同一次操作内完成压缩。
func TestMaintainWithInitMigratesAndCompacts(t *testing.T) {
	m, path := newLegacyTempDB(t)
	now := time.Now().Unix()
	insertRace(t, m, now-30*86400, 10)
	insertBloat(t, m, now-40*86400, 3000)

	before := fileSizeOrZero(path)

	res := m.Maintain(MaintainOptions{Days: 7, InitIncremental: true})
	if !res.OK {
		t.Fatalf("Maintain 失败于 %q: %s", res.FailedStage, res.Message)
	}
	if res.ReclaimOutcome != ReclaimMigrated {
		t.Errorf("ReclaimOutcome = %q, want %q", res.ReclaimOutcome, ReclaimMigrated)
	}
	if res.AutoVacuumBefore != AutoVacuumNone || res.AutoVacuumAfter != AutoVacuumIncremental {
		t.Errorf("模式应 NONE→INCREMENTAL, got %d→%d", res.AutoVacuumBefore, res.AutoVacuumAfter)
	}
	if res.ReclaimedBytes <= 0 {
		t.Errorf("迁移应同时压缩并报告回收字节, got %d", res.ReclaimedBytes)
	}
	if got := fileSizeOrZero(path); got >= before {
		t.Errorf("文件应缩小: before=%d after=%d", before, got)
	}
	if n := countRace(t, m); n != 0 {
		t.Errorf("超期记录应被删净, 剩余 %d", n)
	}
}

// TestMaintainReportsFailedStage 验证失败时如实上报失败阶段与已完成阶段，不谎报成功。
func TestMaintainReportsFailedStage(t *testing.T) {
	m, _ := newLegacyTempDB(t)
	now := time.Now().Unix()
	insertRace(t, m, now-30*86400, 7)

	orig := diskFreeFn
	diskFreeFn = func(string) (diskfree.Space, error) { return diskfree.Space{Free: 1, Total: 100}, nil }
	t.Cleanup(func() { diskFreeFn = orig })

	res := m.Maintain(MaintainOptions{Days: 7, InitIncremental: true})
	if res.OK {
		t.Fatal("初始化因空间不足失败时不应报告 ok=true")
	}
	if res.FailedStage != "reclaim" {
		t.Errorf("FailedStage = %q, want reclaim", res.FailedStage)
	}
	if res.Message == "" {
		t.Error("Message 不应为空")
	}
	if !strings.Contains(res.Message, "需要约") {
		t.Errorf("失败原因应说明空间不足, got %q", res.Message)
	}
	// 已完成阶段必须保留：stats_before 与 prune 都应在，且 prune 已生效。
	var names []string
	for _, s := range res.Stages {
		names = append(names, s.Name)
	}
	if len(names) < 2 || names[0] != "stats_before" || names[1] != "prune" {
		t.Errorf("已完成阶段应保留, got %v", names)
	}
	if n := countRace(t, m); n != 0 {
		t.Errorf("prune 阶段已完成，超期记录应已删除, 剩余 %d", n)
	}
}

// TestCheckpointOnWALAndNonWAL 验证 checkpoint 结果解析正确，且非 WAL 库不报错。
func TestCheckpointOnWALAndNonWAL(t *testing.T) {
	m, _ := newTempDB(t)
	insertRace(t, m, 1000, 10)

	cp, err := m.Checkpoint(true)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !cp.Applicable {
		t.Fatal("WAL 库应报告 applicable=true")
	}
	if cp.Busy {
		t.Error("无并发读者时不应报告 busy")
	}
	if cp.WALBytesAfter > cp.WALBytesBefore {
		t.Errorf("TRUNCATE checkpoint 后 WAL 不应变大: %d → %d", cp.WALBytesBefore, cp.WALBytesAfter)
	}

	// 非 WAL 库：应返回 applicable=false 且不报错。
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.db")
	plain, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	plain.SetMaxOpenConns(1)
	if err := createTables(plain); err != nil {
		t.Fatal(err)
	}
	cp2, err := NewDBMaintenance(plain, plainPath).Checkpoint(true)
	if err != nil {
		t.Fatalf("非 WAL 库不应报错: %v", err)
	}
	if cp2.Applicable {
		t.Error("非 WAL 库应报告 applicable=false")
	}
}

// TestCheckpointBusyReportsBusy 验证存在并发读者时 checkpoint 如实上报 busy，
// 而不是静默当作成功（否则会把"WAL 未并入主库"误报成已回收）。
func TestCheckpointBusyReportsBusy(t *testing.T) {
	m, path := newTempDB(t)
	insertRace(t, m, 1000, 200)

	// 第二个连接持读事务，阻止 WAL 被重置。
	other, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	other.SetMaxOpenConns(1)
	tx, err := other.Begin()
	if err != nil {
		t.Fatalf("begin reader: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM race_log").Scan(&n); err != nil {
		t.Fatalf("reader query: %v", err)
	}

	cp, err := m.Checkpoint(true)
	if err != nil {
		t.Fatalf("Checkpoint 不应报错（应通过 busy 字段上报）: %v", err)
	}
	if !cp.Busy {
		t.Error("存在并发读者时应报告 busy=true")
	}
}
