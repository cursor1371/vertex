package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

func TestOpenCreatesTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	// Verify nodes table exists and starts empty
	var count int
	err = database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count)
	if err != nil || count != 0 {
		t.Errorf("Expected 0 nodes in fresh database, got %d, error: %v", count, err)
	}

	// Verify node_health table exists and starts empty
	var healthCount int
	err = database.QueryRow("SELECT COUNT(*) FROM node_health").Scan(&healthCount)
	if err != nil || healthCount != 0 {
		t.Errorf("Expected 0 health records in fresh database, got %d, error: %v", healthCount, err)
	}
}

func TestOpen_ConnectionPoolLimits(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pool.db")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	stats := database.Stats()

	if stats.MaxOpenConnections <= 0 {
		t.Fatalf("MaxOpenConnections=%d, want > 0 (connection pool limit not set)", stats.MaxOpenConnections)
	}
	if stats.MaxOpenConnections > 1 {
		t.Fatalf("MaxOpenConnections=%d, want <= 1", stats.MaxOpenConnections)
	}

	if stats.OpenConnections <= 0 {
		t.Errorf("OpenConnections=%d, want > 0 (connection should be established)", stats.OpenConnections)
	}

	if stats.Idle <= 0 {
		t.Errorf("Idle=%d, want > 0 (idle connection should be present)", stats.Idle)
	}
}

// TestOpen_MigratesOldUsageLog 模拟老版本数据库（usage_log 缺 req_id/useid/streaming 列）：
// 再次 Open 时应通过迁移补齐列并正常建索引，修复"no such column: req_id"启动失败。
func TestOpen_MigratesOldUsageLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")

	// 先建一个"老库"：仅有旧版 usage_log（无新增列）。
	old, err := sqlOpenOld(dbPath)
	if err != nil {
		t.Fatalf("create old db: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	// 用正式 Open 打开，应完成迁移。
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed (migration bug): %v", err)
	}
	defer func() { _ = database.Close() }()

	// 验证新列已补齐：老库经迁移后，新增的三列必须可写可读。
	store := NewUsageStore(database)
	rec := UsageRecord{
		TS: time.Now().Unix(), ReqID: "mig-1", Model: "m", UseID: "u", Streaming: true, Success: true,
		RequestedModel: "抗截断-m", ResponseMode: "增量", AntiTruncate: true,
		TTFBMs: 900, UpstreamTTFBMs: 300,
	}
	if err := store.Insert(rec); err != nil {
		t.Fatalf("Insert after migration failed: %v", err)
	}
	pg, err := store.History(1, 10)
	if err != nil {
		t.Fatalf("History after migration failed: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	if pg.Items[0].RequestedModel != "抗截断-m" {
		t.Errorf("RequestedModel=%q, 迁移后新列应可写可读", pg.Items[0].RequestedModel)
	}
	if pg.Items[0].ResponseMode != "增量" {
		t.Errorf("ResponseMode=%q, 迁移后新列应可写可读", pg.Items[0].ResponseMode)
	}
	if !pg.Items[0].AntiTruncate {
		t.Error("AntiTruncate 应为 true，迁移后新列应可写可读")
	}
	if pg.Items[0].UpstreamTTFBMs != 300 {
		t.Errorf("UpstreamTTFBMs=%d, 迁移后新列应可写可读", pg.Items[0].UpstreamTTFBMs)
	}
	if pg.Items[0].TTFBMs != 900 {
		t.Errorf("TTFBMs=%d, 应与上游首字分开存储", pg.Items[0].TTFBMs)
	}
}

// TestOpen_MigratesOldHTTPLog 模拟老版本数据库（http_log 缺 error 列）：
// 再次 Open 时应通过迁移补齐列。
func TestOpen_MigratesOldHTTPLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old-httplog.db")

	// 先建一个"老库"：旧版 usage_log + 旧版 http_log（无 error 列）。
	old, err := sqlOpenOld(dbPath)
	if err != nil {
		t.Fatalf("create old db: %v", err)
	}
	if _, err := old.Exec(`
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
			elapsed_ms INTEGER NOT NULL DEFAULT 0
		);
	`); err != nil {
		_ = old.Close()
		t.Fatalf("create old http_log: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	// 用正式 Open 打开，应完成迁移。
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed (migration bug): %v", err)
	}
	defer func() { _ = database.Close() }()

	store := NewHTTPLogStore(database)
	if err := store.InsertBatch([]HTTPLogRecord{{
		TS: time.Now().Unix(), RequestID: "mig-http-1", Scope: "batchgraphql",
		StatusCode: 500, Success: false, Error: "dial tcp: timeout",
		ResponseCode: 8, ResponseResult: "DATA_FETCHING_EXCEPTION",
		ResponseErrorType: "DATA_FETCHING_EXCEPTION", ResponseErrorMessage: "quota",
	}}); err != nil {
		t.Fatalf("InsertBatch after migration failed: %v", err)
	}
	pg, err := store.History(1, 10, HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History after migration failed: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item after migration, got %d", len(pg.Items))
	}
	got := pg.Items[0]
	if got.Error != "dial tcp: timeout" {
		t.Fatalf("error column not migrated: %+v", got)
	}
	if got.ResponseCode != 8 || got.ResponseResult != "DATA_FETCHING_EXCEPTION" ||
		got.ResponseErrorType != "DATA_FETCHING_EXCEPTION" || got.ResponseErrorMessage != "quota" {
		t.Fatalf("response columns not migrated: %+v", got)
	}
}

// sqlOpenOld 构造一个老版本 schema 的 SQLite 库：usage_log 缺 req_id/useid/streaming 列，
// 用于模拟历史库升级场景。
func sqlOpenOld(dbPath string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	oldSchema := `
	CREATE TABLE IF NOT EXISTS usage_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		family TEXT NOT NULL DEFAULT '',
		api_key_name TEXT NOT NULL DEFAULT '',
		node_name TEXT NOT NULL DEFAULT '',
		success INTEGER NOT NULL DEFAULT 0,
		status_code INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		candidates_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		total_ms INTEGER NOT NULL DEFAULT 0,
		ttfb_ms INTEGER NOT NULL DEFAULT 0,
		output_ms INTEGER NOT NULL DEFAULT 0
	);
	`
	if _, err := database.Exec(oldSchema); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

// TestOpen_MigratesOldUsageLogFinishReason 老库迁移后 finish_reason 列可写可读。
func TestOpen_MigratesOldUsageLogFinishReason(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old-finish.db")

	old, err := sqlOpenOld(dbPath)
	if err != nil {
		t.Fatalf("create old db: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed (migration bug): %v", err)
	}
	defer func() { _ = database.Close() }()

	store := NewUsageStore(database)
	if err := store.Insert(UsageRecord{
		TS: time.Now().Unix(), ReqID: "mig-fr", Model: "m", Success: true,
		FinishReason: "MAX_TOKENS",
	}); err != nil {
		t.Fatalf("Insert after migration failed: %v", err)
	}
	pg, err := store.History(1, 10)
	if err != nil {
		t.Fatalf("History after migration failed: %v", err)
	}
	if len(pg.Items) != 1 || pg.Items[0].FinishReason != "MAX_TOKENS" {
		t.Fatalf("迁移后 finish_reason 应可写可读, got %+v", pg.Items)
	}
}
