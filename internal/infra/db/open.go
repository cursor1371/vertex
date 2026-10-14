package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

// Open 打开指定路径的 SQLite 数据库，启用 WAL 模式与单连接池，并确保表结构就绪。
// 实例语义：不触碰任何包级状态；连接生命周期由调用方负责（defer database.Close()）。
//
// 新建（或空文件）库会在建表前启用 auto_vacuum=INCREMENTAL：此时库为空，PRAGMA
// 立即生效、无需额外 VACUUM，此后删除留下的空闲页可由增量回收归还文件系统。
// 已存在的非空库不会被本函数改动——SQLite 只能在空库上直接切换该模式，非空库
// 必须重建页面结构，因此那类库只能走显式维护流程（DBMaintenance.EnableIncrementalAutoVacuum
// 或离线压缩 vertex-proxy vacuum）。
func Open(dbPath string) (*sql.DB, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}

	fresh := isFreshDBFile(dbPath)

	// Use WAL mode for better concurrency
	//
	// auto_vacuum 必须排在 journal_mode(WAL) **之前**：切到 WAL 会写入库头部并把
	// 当时的 auto_vacuum 值固化下来，之后再设置就退化为"下次 VACUUM 才生效"
	// （实测：先 WAL 后 auto_vacuum 读回仍是 0）。仅对新库启用——既有非空库改动
	// 该模式需要 VACUUM 重建，属显式维护流程（DBMaintenance.EnableIncrementalAutoVacuum
	// 或离线压缩 vertex-proxy vacuum），不能在每次启动时悄悄做。
	pragmas := "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	if fresh {
		pragmas = "_pragma=auto_vacuum(INCREMENTAL)&" + pragmas
	}
	dsn := dbPath + "?" + pragmas
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}

	// Ensure DB is reachable
	if errPing := database.Ping(); errPing != nil { //nolint:govet
		_ = database.Close()
		return nil, fmt.Errorf("error: %w", errPing)
	}

	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(5 * time.Minute)

	// Create tables
	if err := createTables(database); err != nil {
		_ = database.Close()
		return nil, err
	}

	return database, nil
}

// isFreshDBFile 判断目标是否为"新建或空文件"的数据库。
// 必须在 sql.Open 之前调用（打开后 SQLite 可能立即写入头部，size 不再为 0）。
func isFreshDBFile(dbPath string) bool {
	fi, err := os.Stat(dbPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	return fi.Size() == 0
}

func createTables(database *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS nodes (
		raw_uri TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		name TEXT NOT NULL,
		disabled BOOLEAN NOT NULL DEFAULT 0,
		locked BOOLEAN NOT NULL DEFAULT 0,
		disabled_by_health BOOLEAN NOT NULL DEFAULT 0,
		disable_source TEXT NOT NULL DEFAULT '',
		disable_reason TEXT NOT NULL DEFAULT '',
		disabled_at INTEGER NOT NULL DEFAULT 0,
		disable_failure_class TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS node_health (
		raw_uri TEXT PRIMARY KEY,
		success_count INTEGER NOT NULL DEFAULT 0,
		fail_count INTEGER NOT NULL DEFAULT 0,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_test_ms REAL NOT NULL DEFAULT 0,
		last_test_error TEXT NOT NULL DEFAULT '',
		last_success_at INTEGER NOT NULL DEFAULT 0,
		last_fail_at INTEGER NOT NULL DEFAULT 0,
		cooldown_until INTEGER NOT NULL DEFAULT 0,
		last_test_at INTEGER NOT NULL DEFAULT 0,
		last_failure_class TEXT NOT NULL DEFAULT '',
		last_http_status INTEGER NOT NULL DEFAULT 0,
		auto_probe_fail_streak INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(raw_uri) REFERENCES nodes(raw_uri) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS entry_nodes (
		raw_uri TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		name TEXT NOT NULL,
		disabled BOOLEAN NOT NULL DEFAULT 0,
		locked BOOLEAN NOT NULL DEFAULT 0,
		disabled_by_health BOOLEAN NOT NULL DEFAULT 0,
		disable_source TEXT NOT NULL DEFAULT '',
		disable_reason TEXT NOT NULL DEFAULT '',
		disabled_at INTEGER NOT NULL DEFAULT 0,
		disable_failure_class TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS entry_node_health (
		raw_uri TEXT PRIMARY KEY,
		success_count INTEGER NOT NULL DEFAULT 0,
		fail_count INTEGER NOT NULL DEFAULT 0,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_test_ms REAL NOT NULL DEFAULT 0,
		last_test_error TEXT NOT NULL DEFAULT '',
		last_success_at INTEGER NOT NULL DEFAULT 0,
		last_fail_at INTEGER NOT NULL DEFAULT 0,
		cooldown_until INTEGER NOT NULL DEFAULT 0,
		last_test_at INTEGER NOT NULL DEFAULT 0,
		last_failure_class TEXT NOT NULL DEFAULT '',
		last_http_status INTEGER NOT NULL DEFAULT 0,
		auto_probe_fail_streak INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(raw_uri) REFERENCES entry_nodes(raw_uri) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS node_fault_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL DEFAULT 'exit',
		node_uri TEXT NOT NULL DEFAULT '',
		node_name TEXT NOT NULL DEFAULT '',
		failure_class TEXT NOT NULL DEFAULT '',
		http_status INTEGER NOT NULL DEFAULT 0,
		error_code TEXT NOT NULL DEFAULT '',
		error_sig TEXT NOT NULL DEFAULT '',
		raw_error TEXT NOT NULL DEFAULT '',
		target TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',
		first_seen_at INTEGER NOT NULL DEFAULT 0,
		last_seen_at INTEGER NOT NULL DEFAULT 0,
		count INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS fault_policies (
		failure_class TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		probe_fail_threshold INTEGER NOT NULL DEFAULT -1,
		updated_at INTEGER NOT NULL DEFAULT 0
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_node_fault_sig ON node_fault_log(scope, node_uri, failure_class, error_sig);
	CREATE INDEX IF NOT EXISTS idx_node_fault_last ON node_fault_log(last_seen_at);
	CREATE INDEX IF NOT EXISTS idx_node_fault_class ON node_fault_log(failure_class);

	CREATE TABLE IF NOT EXISTS usage_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		req_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		requested_model TEXT NOT NULL DEFAULT '',
		family TEXT NOT NULL DEFAULT '',
		api_key_name TEXT NOT NULL DEFAULT '',
		useid TEXT NOT NULL DEFAULT '',
		node_name TEXT NOT NULL DEFAULT '',
		node_uri TEXT NOT NULL DEFAULT '',
		streaming INTEGER NOT NULL DEFAULT 0,
		response_mode TEXT NOT NULL DEFAULT '',
		anti_truncate INTEGER NOT NULL DEFAULT 0,
		anti_truncate_transport TEXT NOT NULL DEFAULT '',
		integrity TEXT NOT NULL DEFAULT '',
		success INTEGER NOT NULL DEFAULT 0,
		status_code INTEGER NOT NULL DEFAULT 0,
		error_reason TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		candidates_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		total_ms INTEGER NOT NULL DEFAULT 0,
		ttfb_ms INTEGER NOT NULL DEFAULT 0,
		upstream_ttfb_ms INTEGER NOT NULL DEFAULT 0,
		output_ms INTEGER NOT NULL DEFAULT 0,
		finish_reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_usage_log_ts ON usage_log(ts);

	CREATE TABLE IF NOT EXISTS http_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL DEFAULT '',
		node_uri TEXT NOT NULL DEFAULT '',
		node_name TEXT NOT NULL DEFAULT '',
		peer_ip TEXT NOT NULL DEFAULT '',
		tcp_reused INTEGER NOT NULL DEFAULT 0,
		http_version TEXT NOT NULL DEFAULT '',
		final_url TEXT NOT NULL DEFAULT '',
		request_headers TEXT NOT NULL DEFAULT '',
		request_body TEXT NOT NULL DEFAULT '',
		response_headers TEXT NOT NULL DEFAULT '',
		response_body TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		success INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		response_code INTEGER NOT NULL DEFAULT 0,
		response_result TEXT NOT NULL DEFAULT '',
		response_error_type TEXT NOT NULL DEFAULT '',
		response_error_message TEXT NOT NULL DEFAULT '',
		elapsed_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_http_log_ts ON http_log(ts);
	CREATE INDEX IF NOT EXISTS idx_http_log_req_id ON http_log(request_id);

	CREATE TABLE IF NOT EXISTS race_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		req_id TEXT NOT NULL DEFAULT '',
		node_name TEXT NOT NULL DEFAULT '',
		node_uri TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		elapsed_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_race_log_ts ON race_log(ts);
	CREATE INDEX IF NOT EXISTS idx_race_log_node ON race_log(node_uri, node_name);
	`
	if _, err := database.Exec(schema); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	// 迁移：为已存在的 usage_log 表补齐新增列（老库无 ALTER，INSERT 前保证列存在）。
	if err := migrateUsageLog(database); err != nil {
		return fmt.Errorf("error %w", err)
	}
	// 迁移：为已存在的 http_log 表补齐新增列。
	if err := migrateHTTPLog(database); err != nil {
		return fmt.Errorf("error %w", err)
	}
	// 迁移：为已存在的节点表补齐 locked（防删）列。
	if err := migrateNodesLocked(database); err != nil {
		return fmt.Errorf("error %w", err)
	}
	// 迁移：为已存在的节点表补齐 disabled_by_health（健康禁用标记）列。
	if err := migrateNodesDisabledByHealth(database); err != nil {
		return fmt.Errorf("error %w", err)
	}
	// 迁移：为已存在的节点表补齐禁用审计列（来源/原因/时间/类别）。
	if err := migrateNodesDisableAudit(database); err != nil {
		return fmt.Errorf("error %w", err)
	}
	// 迁移：为已存在的健康表补齐检查时间与失败归因列。
	if err := migrateHealthAudit(database); err != nil {
		return fmt.Errorf("error %w", err)
	}
	// req_id 索引必须在迁移补列之后创建（老库此时才有该列）。
	if _, err := database.Exec("CREATE INDEX IF NOT EXISTS idx_usage_log_req_id ON usage_log(req_id)"); err != nil {
		return fmt.Errorf("error %w", err)
	}
	return nil

}

// migrateUsageLog 为已存在的 usage_log 表补齐历史版本缺失的列（幂等，忽略已存在列错误）。
func migrateUsageLog(database *sql.DB) error {
	alterCmds := []string{
		"ALTER TABLE usage_log ADD COLUMN req_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN useid TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN streaming INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE usage_log ADD COLUMN error_reason TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN node_uri TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN response_mode TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN anti_truncate INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE usage_log ADD COLUMN anti_truncate_transport TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN integrity TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_log ADD COLUMN upstream_ttfb_ms INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE usage_log ADD COLUMN finish_reason TEXT NOT NULL DEFAULT ''",
	}
	for _, cmd := range alterCmds {
		if _, err := database.Exec(cmd); err != nil {
			// 列已存在时报错可安全忽略（SQLite 无 IF NOT EXISTS for ADD COLUMN）。
			continue
		}
	}
	return nil

}

// migrateHTTPLog 为已存在的 http_log 表补齐历史版本缺失的列（幂等，忽略已存在列错误）。
func migrateHTTPLog(database *sql.DB) error {
	alterCmds := []string{
		"ALTER TABLE http_log ADD COLUMN error TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE http_log ADD COLUMN response_code INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE http_log ADD COLUMN response_result TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE http_log ADD COLUMN response_error_type TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE http_log ADD COLUMN response_error_message TEXT NOT NULL DEFAULT ''",
	}
	for _, cmd := range alterCmds {
		if _, err := database.Exec(cmd); err != nil {
			continue
		}
	}
	return nil

}

// migrateNodesLocked 为已存在的节点表补齐 locked（防删）列（幂等，忽略已存在列错误）。
// nodes / entry_nodes 共用同一列结构，故一次迁移覆盖两表。
func migrateNodesLocked(database *sql.DB) error {
	alterCmds := []string{
		"ALTER TABLE nodes ADD COLUMN locked BOOLEAN NOT NULL DEFAULT 0",
		"ALTER TABLE entry_nodes ADD COLUMN locked BOOLEAN NOT NULL DEFAULT 0",
	}
	for _, cmd := range alterCmds {
		if _, err := database.Exec(cmd); err != nil {
			continue
		}
	}
	return nil

}

// migrateNodesDisabledByHealth 为已存在的节点表补齐 disabled_by_health 列
// （幂等，忽略已存在列错误）。该列持久化"禁用由健康判定触发"的标记，
// 使重启后"成功测速自动解除禁用"的恢复路径仍然成立。
func migrateNodesDisabledByHealth(database *sql.DB) error {
	alterCmds := []string{
		"ALTER TABLE nodes ADD COLUMN disabled_by_health BOOLEAN NOT NULL DEFAULT 0",
		"ALTER TABLE entry_nodes ADD COLUMN disabled_by_health BOOLEAN NOT NULL DEFAULT 0",
	}
	for _, cmd := range alterCmds {
		if _, err := database.Exec(cmd); err != nil {
			continue
		}
	}
	return nil

}

// migrateNodesDisableAudit 为已存在的节点表补齐禁用审计列（幂等）。四列共同回答
// "这个节点为什么被禁用、谁禁的、什么时候禁的、当时是什么故障"：
// disable_source 取 manual(”)/health/permanent/admin_test/unsupported，
// disable_reason 是脱敏后的可读原因，disabled_at 是禁用时刻，disable_failure_class 是归因类别。
//
// 回填：老库只有 disabled_by_health 布尔，历史禁用的健康来源行按 health 回填，
// 否则这些节点在面板上会显示成"人工禁用"，来源审计失真。
func migrateNodesDisableAudit(database *sql.DB) error {
	alterCmds := []string{
		"ALTER TABLE nodes ADD COLUMN disable_source TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE nodes ADD COLUMN disable_reason TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE nodes ADD COLUMN disabled_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE nodes ADD COLUMN disable_failure_class TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE entry_nodes ADD COLUMN disable_source TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE entry_nodes ADD COLUMN disable_reason TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE entry_nodes ADD COLUMN disabled_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE entry_nodes ADD COLUMN disable_failure_class TEXT NOT NULL DEFAULT ''",
	}
	for _, cmd := range alterCmds {
		if _, err := database.Exec(cmd); err != nil {
			continue
		}
	}
	backfills := []string{
		"UPDATE nodes SET disable_source = 'health' WHERE disabled = 1 AND disabled_by_health = 1 AND disable_source = ''",
		"UPDATE entry_nodes SET disable_source = 'health' WHERE disabled = 1 AND disabled_by_health = 1 AND disable_source = ''",
	}
	for _, cmd := range backfills {
		if _, err := database.Exec(cmd); err != nil {
			return fmt.Errorf("error %w", err)
		}
	}
	return nil

}

// migrateHealthAudit 为已存在的健康表补齐审计列（幂等）：
// last_test_at 是最近一次检查时间（last_test_ms 是耗时，不能当时间用）、
// last_failure_class 与 last_http_status 是最近一次失败的归因与状态码、
// auto_probe_fail_streak 是自动探测连续失败次数（达阈值即转永久禁用）。
func migrateHealthAudit(database *sql.DB) error {
	alterCmds := []string{
		"ALTER TABLE node_health ADD COLUMN last_test_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE node_health ADD COLUMN last_failure_class TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE node_health ADD COLUMN last_http_status INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE node_health ADD COLUMN auto_probe_fail_streak INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE entry_node_health ADD COLUMN last_test_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE entry_node_health ADD COLUMN last_failure_class TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE entry_node_health ADD COLUMN last_http_status INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE entry_node_health ADD COLUMN auto_probe_fail_streak INTEGER NOT NULL DEFAULT 0",
	}
	for _, cmd := range alterCmds {
		if _, err := database.Exec(cmd); err != nil {
			continue
		}
	}
	return nil

}

// FaultLogRetentionDays 是故障记录的固定保留天数（评审要求：只保留 1 天）。
// 与 usage_log/http_log/race_log 的"可配置保留期"不同，故障记录用于即时排查，
// 不做成配置项以免误把窗口拉长后表体积失控。
const FaultLogRetentionDays = 1

// FaultLogCutoff 返回故障记录的清理截止时间戳：早于它的故障行应被删除。
func FaultLogCutoff(now int64) int64 {
	return now - int64(FaultLogRetentionDays)*86400
}

// PruneFaultLog 删除 last_seen_at < cutoff 的故障记录行，返回删除行数。
// 独立于 PruneOlderThan：故障表按 last_seen_at（而非 ts）判龄，且保留窗口固定 1 天。
func PruneFaultLog(database *sql.DB, cutoff int64) (int64, error) {
	if database == nil {
		return 0, nil
	}
	res, err := database.Exec("DELETE FROM node_fault_log WHERE last_seen_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
