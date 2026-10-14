package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

// TestAdminGetNodeStatsWindowAndLock 验证 /api/admin/stats/nodes：
// days 时间窗口生效，且按节点聚合行带回防删锁状态（locked）。
func TestAdminGetNodeStatsWindowAndLock(t *testing.T) {
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().Unix()
	if err := store.InsertBatch([]db.UsageRecord{
		{TS: now - 3600, NodeName: "n-locked", NodeURI: "uri-locked", Success: true, TotalTokens: 10},
		{TS: now - 40*86400, NodeName: "n-old", NodeURI: "uri-old", Success: true, TotalTokens: 20},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// 独立节点池：uri-locked 带防删锁；nil 库 = 纯内存模式。
	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, nil)
	mgr.MergeNodes([]exitpool.Node{{RawURI: "uri-locked", Type: "vless", Name: "n-locked", Locked: true}})

	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Stats: store, Exit: mgr, IR: transport.NewIRCache()}}}

	type nodeRow struct {
		Name   string `json:"name"`
		RawURI string `json:"raw_uri"`
		Locked bool   `json:"locked"`
	}
	type respBody struct {
		Days   int64     `json:"days"`
		ByNode []nodeRow `json:"by_node"`
	}

	// days=3：只应含近期节点，且 locked=true
	rec := httptest.NewRecorder()
	adm.adminGetNodeStats(rec, httptest.NewRequest(http.MethodGet, "/api/admin/stats/nodes?days=3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got respBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Days != 3 {
		t.Errorf("days=%d want 3", got.Days)
	}
	if len(got.ByNode) != 1 || got.ByNode[0].RawURI != "uri-locked" {
		t.Fatalf("days=3 应只含近期节点, got %+v", got.ByNode)
	}
	if !got.ByNode[0].Locked {
		t.Error("locked 节点应回传 locked=true")
	}

	// days=0（全部）：两个节点都在默认 90 天保留期内
	rec2 := httptest.NewRecorder()
	adm.adminGetNodeStats(rec2, httptest.NewRequest(http.MethodGet, "/api/admin/stats/nodes?days=0", nil))
	var got2 respBody
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if len(got2.ByNode) != 2 {
		t.Fatalf("days=0 应含全部节点, got %d: %+v", len(got2.ByNode), got2.ByNode)
	}
}

// TestAdminGetNodesStatsByURIMergesUsageAndRacing 验证 /api/admin/nodes/stats：
// 请求级统计与竞速统计按节点 URI 合并到同一条记录。
func TestAdminGetNodesStatsByURIMergesUsageAndRacing(t *testing.T) {
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().Unix()
	if err := store.InsertBatch([]db.UsageRecord{
		{TS: now - 100, NodeName: "n1", NodeURI: "uri1", Success: true},
		{TS: now - 90, NodeName: "n1", NodeURI: "uri1", Success: false},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if err := store.InsertRaceBatch([]db.RaceRecord{
		{TS: now - 80, NodeName: "n1", NodeURI: "uri1", Outcome: "win", ElapsedMs: 50},
		{TS: now - 70, NodeName: "n1", NodeURI: "uri1", Outcome: "error", ElapsedMs: 60},
	}); err != nil {
		t.Fatalf("InsertRaceBatch: %v", err)
	}

	//nolint:exhaustruct // 仅注入被测路径消费的依赖（无节点池时按 URI 直接分组）
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Stats: store}}}

	rec := httptest.NewRecorder()
	adm.adminGetNodesStatsByURI(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Stats map[string]struct {
			Requests int `json:"requests"`
			Success  int `json:"success"`
			Attempts int `json:"attempts"`
			Wins     int `json:"wins"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	st, ok := resp.Stats["uri1"]
	if !ok {
		t.Fatalf("stats 应含 uri1, got %+v", resp.Stats)
	}
	if st.Requests != 2 || st.Success != 1 || st.Attempts != 2 || st.Wins != 1 {
		t.Fatalf("请求级与竞速统计应合并到同一 URI: %+v", st)
	}
}
