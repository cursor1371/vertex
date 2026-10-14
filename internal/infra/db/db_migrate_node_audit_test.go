package db

import (
	"path/filepath"
	"testing"
)

// TestOpen_MigratesNodeAuditAndFaultLog 模拟 Issue #5 之前的旧库：
//   - nodes / entry_nodes 只有 disabled_by_health 布尔，没有禁用审计四列；
//   - node_health / entry_node_health 没有 last_test_at / last_failure_class /
//     last_http_status / auto_probe_fail_streak；
//   - 完全没有 node_fault_log 表。
//
// 再次 Open 时应补齐新列、创建故障表，并把历史"健康禁用"行回填成
// disable_source='health'——否则这些节点在面板上会显示成"人工禁用"，且永久失去
// 自动恢复资格（HealthAutoDisabledURIs 按来源过滤）。
func TestOpen_MigratesNodeAuditAndFaultLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old-node-audit.db")

	old, err := sqlOpenOld(dbPath)
	if err != nil {
		t.Fatalf("create old db: %v", err)
	}
	// 旧版节点表与健康表（不含本次新增列）。
	legacy := `
	CREATE TABLE IF NOT EXISTS nodes (
		raw_uri TEXT PRIMARY KEY, type TEXT NOT NULL, name TEXT NOT NULL,
		disabled BOOLEAN NOT NULL DEFAULT 0, locked BOOLEAN NOT NULL DEFAULT 0,
		disabled_by_health BOOLEAN NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS node_health (
		raw_uri TEXT PRIMARY KEY, success_count INTEGER NOT NULL DEFAULT 0,
		fail_count INTEGER NOT NULL DEFAULT 0, consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_test_ms REAL NOT NULL DEFAULT 0, last_test_error TEXT NOT NULL DEFAULT '',
		last_success_at INTEGER NOT NULL DEFAULT 0, last_fail_at INTEGER NOT NULL DEFAULT 0,
		cooldown_until INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS entry_nodes (
		raw_uri TEXT PRIMARY KEY, type TEXT NOT NULL, name TEXT NOT NULL,
		disabled BOOLEAN NOT NULL DEFAULT 0, locked BOOLEAN NOT NULL DEFAULT 0,
		disabled_by_health BOOLEAN NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS entry_node_health (
		raw_uri TEXT PRIMARY KEY, success_count INTEGER NOT NULL DEFAULT 0,
		fail_count INTEGER NOT NULL DEFAULT 0, consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_test_ms REAL NOT NULL DEFAULT 0, last_test_error TEXT NOT NULL DEFAULT '',
		last_success_at INTEGER NOT NULL DEFAULT 0, last_fail_at INTEGER NOT NULL DEFAULT 0,
		cooldown_until INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO nodes (raw_uri, type, name, disabled, locked, disabled_by_health)
		VALUES ('uri-health', 'vless', 'health-disabled', 1, 0, 1),
		       ('uri-manual', 'vless', 'manual-disabled', 1, 0, 0),
		       ('uri-live', 'vless', 'live', 0, 0, 0);
	INSERT INTO entry_nodes (raw_uri, type, name, disabled, locked, disabled_by_health)
		VALUES ('entry-health', 'socks5', 'entry-health-disabled', 1, 0, 1);
	INSERT INTO node_health (raw_uri, success_count, fail_count, consecutive_failures)
		VALUES ('uri-health', 1, 2, 2);
	INSERT INTO entry_node_health (raw_uri, success_count, fail_count, consecutive_failures)
		VALUES ('entry-health', 0, 1, 1);
	`
	if _, err := old.Exec(legacy); err != nil {
		_ = old.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed (migration bug): %v", err)
	}
	defer func() { _ = database.Close() }()

	// 1) 节点审计列已补齐，且历史健康禁用行回填为 health 来源。
	assertSource := func(table, uri, want string) {
		t.Helper()
		var got string
		if err := database.QueryRow("SELECT disable_source FROM "+table+" WHERE raw_uri = ?", uri).Scan(&got); err != nil {
			t.Fatalf("读取 %s.disable_source(%s): %v", table, uri, err)
		}
		if got != want {
			t.Errorf("%s[%s].disable_source=%q, want %q", table, uri, got, want)
		}
	}
	assertSource("nodes", "uri-health", "health")
	assertSource("nodes", "uri-manual", "")
	assertSource("nodes", "uri-live", "")
	assertSource("entry_nodes", "entry-health", "health")

	// 新增的审计列必须可读写（回填只碰 disable_source，其余保持默认）。
	if _, err := database.Exec("UPDATE nodes SET disable_reason = ?, disabled_at = ?, disable_failure_class = ? WHERE raw_uri = ?",
		"连续节点级连通性失败达阈值（阈值 2）", 1700000000, "node_network", "uri-health"); err != nil {
		t.Fatalf("写入审计列失败（迁移未补齐列）: %v", err)
	}
	var reason, class string
	var at int64
	if err := database.QueryRow("SELECT disable_reason, disabled_at, disable_failure_class FROM nodes WHERE raw_uri = 'uri-health'").
		Scan(&reason, &at, &class); err != nil {
		t.Fatalf("读回审计列: %v", err)
	}
	if reason == "" || at != 1700000000 || class != "node_network" {
		t.Errorf("审计列往返失败: reason=%q at=%d class=%q", reason, at, class)
	}

	// 2) 健康表新增列已补齐（写读往返验证，而不是只看表结构）。
	if _, err := database.Exec(`UPDATE node_health SET last_test_at = ?, last_failure_class = ?, last_http_status = ?, auto_probe_fail_streak = ? WHERE raw_uri = ?`,
		1700000001, "dns", 403, 2, "uri-health"); err != nil {
		t.Fatalf("写入健康审计列失败（迁移未补齐列）: %v", err)
	}
	var lta int64
	var lfc string
	var lhs, streak int
	if err := database.QueryRow("SELECT last_test_at, last_failure_class, last_http_status, auto_probe_fail_streak FROM node_health WHERE raw_uri = 'uri-health'").
		Scan(&lta, &lfc, &lhs, &streak); err != nil {
		t.Fatalf("读回健康审计列: %v", err)
	}
	if lta != 1700000001 || lfc != "dns" || lhs != 403 || streak != 2 {
		t.Errorf("健康审计列往返失败: %d %q %d %d", lta, lfc, lhs, streak)
	}
	if _, err := database.Exec(`UPDATE entry_node_health SET last_test_at = ? WHERE raw_uri = ?`, 1700000002, "entry-health"); err != nil {
		t.Fatalf("entry_node_health 未补齐健康审计列: %v", err)
	}

	// 3) 故障记录表已创建且可写。
	if _, err := database.Exec(`INSERT INTO node_fault_log
		(scope, node_uri, node_name, failure_class, http_status, error_code, error_sig, raw_error, target, source, first_seen_at, last_seen_at, count)
		VALUES ('exit', 'uri-health', 'health-disabled', 'node_network', 0, '', 'sig-1', 'dial tcp: refused', '', 'request', 1, 1, 1)`); err != nil {
		t.Fatalf("node_fault_log 未创建: %v", err)
	}
	var cnt int64
	if err := database.QueryRow("SELECT COUNT(*) FROM node_fault_log").Scan(&cnt); err != nil {
		t.Fatalf("统计故障表: %v", err)
	}
	if cnt != 1 {
		t.Errorf("故障表应有 1 行, got %d", cnt)
	}

	// 4) 故障处理策略表已创建且可写（策略可配置化：稀疏覆盖，空表 = 全默认）。
	if _, err := database.Exec(`INSERT INTO fault_policies (failure_class, action, probe_fail_threshold, updated_at)
		VALUES ('node_network', 'disable_then_probe', 3, 1700000000)`); err != nil {
		t.Fatalf("fault_policies 未创建或列不匹配: %v", err)
	}
	var action string
	var threshold int
	if err := database.QueryRow("SELECT action, probe_fail_threshold FROM fault_policies WHERE failure_class = 'node_network'").
		Scan(&action, &threshold); err != nil {
		t.Fatalf("读取策略行: %v", err)
	}
	if action != "disable_then_probe" || threshold != 3 {
		t.Errorf("策略行往返不一致: %q %d", action, threshold)
	}
	// 稀疏覆盖：表里只有显式写入的类别（其余类别靠内置默认，无需行）。
	var policyRows int
	if err := database.QueryRow("SELECT COUNT(*) FROM fault_policies").Scan(&policyRows); err != nil {
		t.Fatalf("统计策略行: %v", err)
	}
	if policyRows != 1 {
		t.Errorf("稀疏覆盖应只有 1 行, got %d", policyRows)
	}

	// 5) 幂等：重复 Open 不应报错（回填与建表都必须是可重复执行的）。
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	again, err := Open(dbPath)
	if err != nil {
		t.Fatalf("重复 Open 失败（迁移不幂等）: %v", err)
	}
	defer func() { _ = again.Close() }()
	// 二次迁移不得把人工禁用误标成 health。
	var manualSrc string
	if err := again.QueryRow("SELECT disable_source FROM nodes WHERE raw_uri = 'uri-manual'").Scan(&manualSrc); err != nil {
		t.Fatalf("二次 Open 后读取: %v", err)
	}
	if manualSrc != "" {
		t.Errorf("人工禁用的节点来源不应被回填改成 %q", manualSrc)
	}
}

// TestFaultLogRetentionConstants 锁定"故障记录只保留 1 天"这一硬性口径。
func TestFaultLogRetentionConstants(t *testing.T) {
	if FaultLogRetentionDays != 1 {
		t.Fatalf("FaultLogRetentionDays=%d, want 1（评审要求固定 1 天）", FaultLogRetentionDays)
	}
	now := int64(1_700_000_000)
	if got, want := FaultLogCutoff(now), now-86400; got != want {
		t.Errorf("FaultLogCutoff=%d, want %d", got, want)
	}
}
