package stats

import (
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
)

// TestHTTPLogSwitch 验证全局开关默认关闭、可切换、保留天数默认与配置。
func TestHTTPLogSwitch(t *testing.T) {
	s, err := db.NewMemoryHTTPLogStore()
	if err != nil {
		t.Fatalf("NewMemoryHTTPLogStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	InitHTTPLog(s)
	if HTTPLogEnabled() {
		t.Fatal("http_log 开关应默认关闭")
	}
	SetHTTPLogEnabled(true)
	if !HTTPLogEnabled() {
		t.Fatal("SetHTTPLogEnabled(true) 未生效")
	}
	SetHTTPLogEnabled(false)
	if HTTPLogEnabled() {
		t.Fatal("SetHTTPLogEnabled(false) 未生效")
	}

	if HTTPLogRetentionDays() != 30 {
		t.Fatalf("默认保留天数应为 30, got %d", HTTPLogRetentionDays())
	}
	SetHTTPLogRetentionDays(14)
	if HTTPLogRetentionDays() != 14 {
		t.Fatalf("SetHTTPLogRetentionDays(14) 未生效, got %d", HTTPLogRetentionDays())
	}
	SetHTTPLogRetentionDays(0)
	if HTTPLogRetentionDays() != 14 {
		t.Fatalf("SetHTTPLogRetentionDays(0) 应被忽略, got %d", HTTPLogRetentionDays())
	}

	// body 捕获上限：默认 0（不截断），可配置。
	if got := HTTPLogRecorder().BodyCapBytes(); got != 0 {
		t.Fatalf("默认 body cap 应为 0, got %d", got)
	}
	SetHTTPLogBodyCapBytes(65536)
	if got := HTTPLogRecorder().BodyCapBytes(); got != 65536 {
		t.Fatalf("SetHTTPLogBodyCapBytes(65536) 未生效, got %d", got)
	}
	SetHTTPLogBodyCapBytes(-1)
	if got := HTTPLogRecorder().BodyCapBytes(); got != 65536 {
		t.Fatalf("SetHTTPLogBodyCapBytes(-1) 应被忽略, got %d", got)
	}

	CloseHTTPLog()
}

// TestHTTPLogRecorderDisabled 验证开关关闭时 Record 不落库。
func TestHTTPLogRecorderDisabled(t *testing.T) {
	s, err := db.NewMemoryHTTPLogStore()
	if err != nil {
		t.Fatalf("NewMemoryHTTPLogStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	store := s
	InitHTTPLog(store)
	SetHTTPLogEnabled(false)
	defer CloseHTTPLog()

	rec := HTTPLogRecorder()
	if rec.Enabled() {
		t.Fatal("开关关闭时 recorder 不应 enabled")
	}
	rec.Record(transport.HTTPLogRecord{TS: time.Now().Unix(), RequestID: "x", Success: true})

	CloseHTTPLog()

	pg, err := store.History(1, 20, db.HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if pg.Total != 0 {
		t.Fatalf("开关关闭时不应有记录, got %d", pg.Total)
	}
}

// TestHTTPLogRecorderEnabled 验证开关开启时 Record 批量落库、节点名解析生效。
func TestHTTPLogRecorderEnabled(t *testing.T) {
	s, err := db.NewMemoryHTTPLogStore()
	if err != nil {
		t.Fatalf("NewMemoryHTTPLogStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	store := s
	InitHTTPLog(store)
	SetHTTPLogEnabled(true)
	SetHTTPLogNodeNamer(func(uri string) string {
		if uri == "socks5://n" {
			return "node-a"
		}
		return uri
	})
	defer CloseHTTPLog()

	rec := HTTPLogRecorder()
	if !rec.Enabled() {
		t.Fatal("开关开启时 recorder 应 enabled")
	}
	rec.Record(transport.HTTPLogRecord{
		TS: time.Now().Unix(), RequestID: "ok-1", Scope: "batchgraphql",
		NodeURI: "socks5://n", Success: true, StatusCode: 200, HTTPVersion: "HTTP/2",
		TCPReused: true, PeerIP: "1.2.3.4",
	})
	rec.Record(transport.HTTPLogRecord{
		TS: time.Now().Unix(), RequestID: "fail-1", Scope: "batchgraphql",
		NodeURI: "socks5://n", Success: false, StatusCode: 500,
	})

	// 关闭收集器会同步排空缓冲，避免依赖 3s flush 周期。
	CloseHTTPLog()

	pg, err := store.History(1, 20, db.HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if pg.Total != 2 {
		t.Fatalf("开关开启时应有 2 条记录, got %d", pg.Total)
	}
	// 时间倒序：fail-1 后写应在前
	if pg.Items[0].RequestID != "fail-1" || pg.Items[0].NodeName != "node-a" || pg.Items[0].NodeURI != "socks5://n" {
		t.Fatalf("node 名解析/顺序不符: %+v", pg.Items[0])
	}
	// 验证成功记录字段
	okRec, found, err := store.ByID(pg.Items[1].ID)
	if err != nil || !found {
		t.Fatalf("ByID: %v %v", found, err)
	}
	if !okRec.TCPReused || okRec.PeerIP != "1.2.3.4" || okRec.HTTPVersion != "HTTP/2" {
		t.Fatalf("网络元数据未保留: %+v", okRec)
	}
}
