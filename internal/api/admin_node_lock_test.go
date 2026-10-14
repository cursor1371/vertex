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

// TestAdminLockHealthyNodesRequiresRequestSuccess 验证一键防删只锁定"确实成功过"的节点：
// 仅测试通过、但从未有请求级成功记录的节点不应被锁定。
func TestAdminLockHealthyNodesRequiresRequestSuccess(t *testing.T) {
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().Unix()
	if err := store.InsertBatch([]db.UsageRecord{
		{TS: now - 100, NodeName: "n-ok", NodeURI: "uri-ok", Success: true},
		{TS: now - 90, NodeName: "n-fail", NodeURI: "uri-fail", Success: false},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, nil)
	mgr.MergeNodes([]exitpool.Node{
		{RawURI: "uri-ok", Type: "vless", Name: "n-ok"},
		{RawURI: "uri-fail", Type: "vless", Name: "n-fail"},
		{RawURI: "uri-untested", Type: "vless", Name: "n-untested"},
	})
	// 三个节点都"测试通过且无连续失败"，仅凭测试口径会被全部锁定。
	for _, u := range []string{"uri-ok", "uri-fail", "uri-untested"} {
		mgr.RecordTest(u, true, 10, "")
	}

	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Stats: store, Exit: mgr, IR: transport.NewIRCache()}}}

	rec := httptest.NewRecorder()
	adm.adminLockHealthyNodes(rec, httptest.NewRequest(http.MethodPost, "/api/admin/nodes/lock-healthy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("只应锁定 1 个成功过的节点, got count=%d", resp.Count)
	}
	locked := map[string]bool{}
	for _, n := range mgr.LoadNodes() {
		locked[n.RawURI] = n.Locked
	}
	if !locked["uri-ok"] {
		t.Error("有请求级成功记录的节点应被锁定")
	}
	if locked["uri-fail"] || locked["uri-untested"] {
		t.Errorf("无成功记录的节点不应被锁定: %+v", locked)
	}
}

// TestAdminUnlockAllNodes 验证一键解除防删会清空全部出口节点的锁定，且不影响未锁定节点。
func TestAdminUnlockAllNodes(t *testing.T) {
	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, nil)
	mgr.MergeNodes([]exitpool.Node{
		{RawURI: "uri1", Type: "vless", Name: "n1", Locked: true},
		{RawURI: "uri2", Type: "vless", Name: "n2", Locked: true},
		{RawURI: "uri3", Type: "vless", Name: "n3"},
	})

	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Exit: mgr, IR: transport.NewIRCache()}}}

	rec := httptest.NewRecorder()
	adm.adminUnlockAllNodes(rec, httptest.NewRequest(http.MethodPost, "/api/admin/nodes/unlock-all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 2 {
		t.Errorf("应解除 2 个锁定, got count=%d", resp.Count)
	}
	for _, n := range mgr.LoadNodes() {
		if n.Locked {
			t.Errorf("解除后不应再有锁定节点: %s", n.RawURI)
		}
	}
}
