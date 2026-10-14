package stats

import (
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// TestRecordAndSnapshot 验证 Record 更新实时计数器、Init 幂等、Close 后取节点返回空。
func TestRecordAndSnapshot(t *testing.T) {
	s, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	Init(s)
	Init(s) // 幂等

	Record(db.UsageRecord{Success: true, PromptTokens: 10, CandidatesTokens: 5, TotalTokens: 15})
	Record(db.UsageRecord{Success: false, TotalTokens: 0})

	snap := Snapshot()
	if snap["requests"] != int64(2) || snap["success"] != int64(1) || snap["errors"] != int64(1) {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap["prompt_tokens"] != int64(10) || snap["total_tokens"] != int64(15) {
		t.Fatalf("unexpected token counters: %+v", snap)
	}

	RecordNode("req-1", "node-a", "uri-a")
	if name, uri := TakeNodeInfo("req-1"); name != "node-a" || uri != "uri-a" {
		t.Fatalf("TakeNodeInfo got (%q,%q), want (node-a,uri-a)", name, uri)
	}
	if name, _ := TakeNodeInfo("req-1"); name != "" {
		t.Fatalf("second TakeNodeInfo should be empty, got %q", name)
	}

	Close()
	// Close 后再取节点应返回空
	if name, _ := TakeNodeInfo("req-2"); name != "" {
		t.Fatalf("TakeNodeInfo after Close should be empty, got %q", name)
	}
}

// TestNodeTTLCleanup 验证超过 TTL 的节点条目被惰性清理。
func TestNodeTTLCleanup(t *testing.T) {
	s, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	Init(s)

	// 手工注入一条过期条目
	nodeMu.Lock()
	nodes["old-req"] = nodeEntry{name: "node-x", written: time.Now().Add(-nodeTTL - time.Minute)}
	nodeMu.Unlock()

	// 触发惰性清理
	RecordNode("fresh", "node-y", "uri-y")

	nodeMu.Lock()
	_, old := nodes["old-req"]
	_, fresh := nodes["fresh"]
	nodeMu.Unlock()
	if old {
		t.Fatal("stale node entry should have been cleaned up")
	}
	if !fresh {
		t.Fatal("fresh node entry should exist")
	}

	Close()
}

// TestActiveRequests 验证在途请求注册表的登记/更新/移除与快照。
func TestActiveRequests(t *testing.T) {
	s, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	Init(s)

	StartReq("req-1", "model-a", "alice", "alice", true)
	StartReq("req-2", "model-b", "", "bob", false)
	UpdateReq("req-2", func(a *ActiveRequest) {
		a.UseID = "bob-2"
		a.TotalTok = 42
		a.TTFBMs = 300
	})

	active := ActiveRequests()
	if len(active) != 2 {
		t.Fatalf("expected 2 active requests, got %d", len(active))
	}
	// 按开始时间倒序：req-2 后开始应排在前
	if active[0].ReqID != "req-2" {
		t.Fatalf("expected req-2 first (newest), got %+v", active[0])
	}
	if active[0].UseID != "bob-2" || active[0].TotalTok != 42 || active[0].TTFBMs != 300 {
		t.Fatalf("UpdateReq 未生效: %+v", active[0])
	}
	if active[1].Streaming != true || active[1].UseID != "alice" {
		t.Fatalf("StartReq 字段未生效: %+v", active[1])
	}

	StopReq("req-1")
	active = ActiveRequests()
	if len(active) != 1 || active[0].ReqID != "req-2" {
		t.Fatalf("StopReq 未移除: %+v", active)
	}

	Close()
}

// TestSetRetentionDays 验证保留天数可配置且默认 90。
func TestSetRetentionDays(t *testing.T) {
	s, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	Init(s)

	if RetentionDays() != 90 {
		t.Fatalf("default retention should be 90, got %d", RetentionDays())
	}
	SetRetentionDays(30)
	if RetentionDays() != 30 {
		t.Fatalf("SetRetentionDays(30) failed, got %d", RetentionDays())
	}
	SetRetentionDays(0)
	if RetentionDays() != 30 {
		t.Fatalf("SetRetentionDays(0) should be ignored, got %d", RetentionDays())
	}

	Close()
}

// TestInitCloseCycleRebindable 验证 Init/Close 可成对重复调用并重新绑定仓储。
// 回归：旧实现用包级 sync.Once 保护 Close，导致首个 Close 之后的 Close 恒为 no-op，
// 后续 Init 起的 flushLoop 永不停机、started 也置不回 false，单例会一直绑在旧 store 上。
func TestInitCloseCycleRebindable(t *testing.T) {
	first, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = first.Close() }()

	Init(first)
	Record(db.UsageRecord{Success: true, TotalTokens: 1})
	Close()

	// 第二次 Init 应能重新绑定到新 store。
	second, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = second.Close() }()

	Init(second)
	Record(db.UsageRecord{Success: true, TotalTokens: 7})
	Close() // 必须真正停机，否则第二次 Init 的 flushLoop 会泄漏

	// Close 后应回到未启用态（否则后续 Init 不生效、flushLoop 泄漏）。
	if Enabled() {
		t.Error("Close 后 Enabled 应为 false")
	}

	// 第一条记录只应落在 first，第二条只应落在 second。
	n1, err := first.Aggregate(0, time.Now().Unix()+1)
	if err != nil {
		t.Fatalf("Aggregate(first): %v", err)
	}
	n2, err := second.Aggregate(0, time.Now().Unix()+1)
	if err != nil {
		t.Fatalf("Aggregate(second): %v", err)
	}
	if n1.TotalTokens != 1 || n1.Requests != 1 {
		t.Errorf("first store 应收 1 条, got requests=%d tokens=%d", n1.Requests, n1.TotalTokens)
	}
	if n2.TotalTokens != 7 || n2.Requests != 1 {
		t.Errorf("second store 应收 1 条, got requests=%d tokens=%d", n2.Requests, n2.TotalTokens)
	}
}
