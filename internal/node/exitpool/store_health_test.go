package exitpool

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
	"time"
)

func TestUpdateNodeTestResult(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	// Setup: one enabled node
	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	mgr.MergeNodes([]Node{n1})

	// Test: fail the node
	mgr.RecordTest("uri1", false, 100, "timeout")
	health := mgr.LoadHealth()
	h1, ok := health["uri1"]
	if !ok || h1.ConsecutiveFailures != 1 {
		t.Errorf("Expected 1 consecutive failure")
	}
	nodes := mgr.LoadNodes()
	if len(nodes) != 1 || nodes[0].Disabled {
		t.Errorf("Expected node1 to NOT be disabled after soft failure (sub-healthy replaces disable)")
	}
	if !ok || h1.LastSubHealthyAt == 0 {
		t.Errorf("Expected LastSubHealthyAt to be set after failed test")
	}

	// Test: succeed the node
	mgr.RecordTest("uri1", true, 50, "")
	health = mgr.LoadHealth()
	h2, ok := health["uri1"]
	if !ok || h2.SuccessCount != 1 {
		t.Errorf("Expected 1 success")
	}
	if !ok || h2.LastSubHealthyAt != 0 {
		t.Errorf("Expected LastSubHealthyAt to be cleared after success")
	}
	nodes = mgr.LoadNodes()
	if len(nodes) == 0 || nodes[0].Disabled {
		t.Errorf("Expected node1 to be enabled after success")
	}
}

func TestEnableNode(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "node1", Disabled: true} //nolint:exhaustruct
	mgr.MergeNodes([]Node{n1})

	// Also set cooldown
	mgr.RecordTest("uri1", false, 0, "timeout")

	ok := mgr.EnableNode("uri1")
	if !ok {
		t.Errorf("Expected EnableNode to return true")
	}
	nodes := mgr.LoadNodes()
	if len(nodes) != 1 || nodes[0].Disabled {
		t.Errorf("Expected node1 to be enabled")
	}
	health := mgr.LoadHealth()
	if h, hOK := health["uri1"]; hOK && h.CooldownUntil != 0 {
		t.Errorf("Expected cooldown to be cleared")
	}

	// Test enabling non-existent node
	ok = mgr.EnableNode("nonexistent")
	if ok {
		t.Errorf("Expected EnableNode to return false for nonexistent node")
	}
}

func TestSelectForParallel_SubHealthyFallback(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "node1"}
	n2 := Node{RawURI: "uri2", Name: "node2"}
	n3 := Node{RawURI: "uri3", Name: "node3"}
	mgr.MergeNodes([]Node{n1, n2, n3})

	// Put n1 and n2 in sub-healthy via soft failure, leave n3 healthy
	mgr.RecordTest("uri1", false, 0, "timeout")
	mgr.RecordTest("uri2", false, 0, "timeout")

	// Request 3 nodes: should get n3 (Tier 1) + n1+n2 (Tier 2 fallback)
	selected := mgr.SelectForParallel(3, false)
	if len(selected) != 3 {
		t.Errorf("Expected 3 selected (1 Tier1 + 2 Tier2), got %d", len(selected))
	}
}

func TestGetNodeTier(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	// Tier 3: disabled node
	nDisabled := Node{RawURI: "uri-dis", Name: "dis", Disabled: true}
	if tier := getNodeTier(nDisabled, nil, 0); tier != 3 {
		t.Errorf("Expected disabled node → tier 3, got %d", tier)
	}

	// Tier 1: no health entry at all
	nNoHealth := Node{RawURI: "uri-nohealth", Name: "nohealth"}
	if tier := getNodeTier(nNoHealth, nil, 0); tier != 1 {
		t.Errorf("Expected node without health → tier 1, got %d", tier)
	}

	// Tier 1: healthy node with LastSubHealthyAt == 0
	mgr.MergeNodes([]Node{{RawURI: "uri-h1", Name: "h1"}})
	mgr.RecordTest("uri-h1", true, 50, "")
	h1, ok := mgr.LoadHealth()["uri-h1"]
	if tier := getNodeTier(Node{RawURI: "uri-h1", Name: "h1"}, &h1, time.Now().Unix()); !ok || tier != 1 {
		t.Errorf("Expected healthy node → tier 1, got %d", tier)
	}

	// Tier 2: sub-healthy (LastSubHealthyAt set within 5s)
	mgr.RecordTest("uri-h1", false, 0, "timeout")
	h2, ok := mgr.LoadHealth()["uri-h1"]
	if !ok || h2.LastSubHealthyAt == 0 {
		t.Fatal("Expected LastSubHealthyAt to be set")
	}
	if tier := getNodeTier(Node{RawURI: "uri-h1", Name: "h1"}, &h2, time.Now().Unix()); tier != 2 {
		t.Errorf("Expected sub-healthy node → tier 2, got %d", tier)
	}

	// Tier 2 persists regardless of time: set LastSubHealthyAt to 10s ago
	// （LoadHealth 返回值拷贝：本地改副本后喂给 getNodeTier，与原语义一致）
	h2.LastSubHealthyAt = time.Now().Unix() - 10
	if tier := getNodeTier(Node{RawURI: "uri-h1", Name: "h1"}, &h2, time.Now().Unix()); tier != 2 {
		t.Errorf("Expected sub-healthy node (10s ago) → tier 2, got %d", tier)
	}
}

func TestSelectForParallel_RoundRobin_InFlight(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "a"}
	n2 := Node{RawURI: "uri2", Name: "b"}
	n3 := Node{RawURI: "uri3", Name: "c"}
	mgr.MergeNodes([]Node{n1, n2, n3})

	// All Tier 1, all InFlight=0: round-robin across calls
	rrIndexStore(mgr, 0)

	got := make(map[string]int)
	for call := 0; call < 6; call++ {
		sel := mgr.SelectForParallel(1, false)
		if len(sel) != 1 {
			t.Fatalf("call %d: expected 1 selected, got %d", call, len(sel))
		}
		got[sel[0].RawURI]++
	}

	// Over 6 calls with 3 nodes, each should be picked exactly 2 times
	for _, uri := range []string{"uri1", "uri2", "uri3"} {
		if got[uri] != 2 {
			t.Errorf("Expected %s to be picked 2 times, got %d", uri, got[uri])
		}
	}

	// Node with higher InFlight should be deprioritized
	mgr.Reset()
	mgr.MergeNodes([]Node{n1, n2, n3})
	// Simulate uri1 having InFlight=2, others InFlight=0
	mgr.mu.Lock()
	mgr.healthMap["uri1"] = &NodeHealth{InFlight: 2} //nolint:exhaustruct
	mgr.healthMap["uri2"] = &NodeHealth{InFlight: 0} //nolint:exhaustruct
	mgr.healthMap["uri3"] = &NodeHealth{InFlight: 0} //nolint:exhaustruct
	mgr.mu.Unlock()

	sel2 := mgr.SelectForParallel(1, false)
	if len(sel2) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(sel2))
	}
	if sel2[0].RawURI == "uri1" {
		t.Errorf("Expected lower InFlight node to be preferred, but got uri1 (InFlight=2)")
	}
}

func TestSelectForParallel_SubHealthy5sRecovery(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "a"}
	n2 := Node{RawURI: "uri2", Name: "b"}
	mgr.MergeNodes([]Node{n1, n2})

	// n1 soft-failed → Tier 2; n2 healthy → Tier 1
	mgr.RecordTest("uri1", false, 0, "timeout")
	mgr.RecordTest("uri2", true, 50, "")

	// Request 2: should get n2 (Tier 1) + n1 (Tier 2 fallback)
	sel := mgr.SelectForParallel(2, false)
	if len(sel) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(sel))
	}

	// Verify n1 stays Tier 2 even after 10s (no automatic recovery)
	// LoadHealth 返回值拷贝、不再写穿：直接改内存态模拟"标记已老化 10s"。
	mgr.mu.Lock()
	if h := mgr.healthMap["uri1"]; h != nil {
		h.LastSubHealthyAt = time.Now().Unix() - 10
	}
	mgr.mu.Unlock()

	// n1 should still be Tier 2 (LastSubHealthyAt > 0)
	if h, ok := mgr.LoadHealth()["uri1"]; ok {
		tier := getNodeTier(n1, &h, time.Now().Unix())
		if tier != 2 {
			t.Errorf("Expected n1 to stay Tier 2, got Tier %d", tier)
		}
	}
	// SelectForParallel still works: n2 (Tier 1) + n1 (Tier 2 fallback)
	sel2 := mgr.SelectForParallel(2, false)
	if len(sel2) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(sel2))
	}
}

func TestSelectForParallel_LastSelectedAtSort(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	// 3 Tier 1 nodes, all InFlight=0, different LastSelectedAt
	n1 := Node{RawURI: "uri-a", Name: "a"}
	n2 := Node{RawURI: "uri-b", Name: "b"}
	n3 := Node{RawURI: "uri-c", Name: "c"}
	mgr.MergeNodes([]Node{n1, n2, n3})

	// Create health entries and set LastSelectedAt
	now := time.Now().Unix()
	mgr.mu.Lock()
	mgr.healthMap["uri-a"] = &NodeHealth{LastSelectedAt: now - 100, InFlight: 0} //nolint:exhaustruct
	mgr.healthMap["uri-b"] = &NodeHealth{LastSelectedAt: now - 10, InFlight: 0}  //nolint:exhaustruct
	mgr.healthMap["uri-c"] = &NodeHealth{LastSelectedAt: 0, InFlight: 0}         //nolint:exhaustruct
	mgr.mu.Unlock()

	// Control round-robin so sorted[0] is picked
	// Sorted order: [uri-c(0), uri-a(now-100), uri-b(now-10)]
	// With rrIndex=2, offset=(2+1)%3=0 → picks sorted[0]=uri-c
	rrIndexStore(mgr, 2)
	sel := mgr.SelectForParallel(1, false)
	if len(sel) != 1 {
		t.Fatalf("expected 1 selected, got %d", len(sel))
	}
	if sel[0].RawURI != "uri-c" {
		t.Errorf("Expected uri-c (LastSelectedAt=0) to be selected first, got %s", sel[0].RawURI)
	}
}

func TestSelectForParallel_Phase3Protection(t *testing.T) {
	// Scenario A: fresh node available to replace recently used node
	t.Run("fresh replacement available", func(t *testing.T) {
		mgr := newMemMgr(nil, Hooks{})
		n1 := Node{RawURI: "uri-a", Name: "a"}
		n2 := Node{RawURI: "uri-b", Name: "b"}
		n3 := Node{RawURI: "uri-c", Name: "c"}
		n4 := Node{RawURI: "uri-d", Name: "d"}
		n5 := Node{RawURI: "uri-e", Name: "e"}
		mgr.MergeNodes([]Node{n1, n2, n3, n4, n5})

		now := time.Now().Unix()
		mgr.mu.Lock()
		// Two fresh (never selected), three stale (recently selected)
		mgr.healthMap["uri-a"] = &NodeHealth{LastSelectedAt: 0, InFlight: 0}       //nolint:exhaustruct
		mgr.healthMap["uri-b"] = &NodeHealth{LastSelectedAt: 0, InFlight: 0}       //nolint:exhaustruct
		mgr.healthMap["uri-c"] = &NodeHealth{LastSelectedAt: now - 1, InFlight: 0} //nolint:exhaustruct
		mgr.healthMap["uri-d"] = &NodeHealth{LastSelectedAt: now - 1, InFlight: 0} //nolint:exhaustruct
		mgr.healthMap["uri-e"] = &NodeHealth{LastSelectedAt: now - 1, InFlight: 0} //nolint:exhaustruct
		mgr.mu.Unlock()

		// Sorted by inFlight→LastSelectedAt→URI: [uri-a(0), uri-b(0), uri-c(1), uri-d(1), uri-e(1)]
		// Set offset to skip fresh nodes: need offset=2 → rrIndex such that (idx+1)%5=2
		rrIndexStore(mgr, 1)
		sel := mgr.SelectForParallel(3, false)
		if len(sel) != 3 {
			t.Fatalf("expected 3 selected, got %d", len(sel))
		}
		// Phase 3 should replace uri-c and uri-d with uri-a and uri-b
		// Final selected: [uri-a, uri-b, uri-e]
		if sel[0].RawURI != "uri-a" {
			t.Errorf("Expected sel[0]=uri-a (fresh), got %s", sel[0].RawURI)
		}
		if sel[1].RawURI != "uri-b" {
			t.Errorf("Expected sel[1]=uri-b (fresh), got %s", sel[1].RawURI)
		}
		if sel[2].RawURI != "uri-e" {
			t.Errorf("Expected sel[2]=uri-e (stale, no more fresh), got %s", sel[2].RawURI)
		}
	})

	// Scenario B: all nodes within 5s, no fresh replacement → best-effort, no change
	t.Run("no fresh replacement", func(t *testing.T) {
		mgr := newMemMgr(nil, Hooks{})
		n1 := Node{RawURI: "uri-a", Name: "a"}
		n2 := Node{RawURI: "uri-b", Name: "b"}
		n3 := Node{RawURI: "uri-c", Name: "c"}
		mgr.MergeNodes([]Node{n1, n2, n3})

		now := time.Now().Unix()
		mgr.mu.Lock()
		mgr.healthMap["uri-a"] = &NodeHealth{LastSelectedAt: now - 1, InFlight: 0} //nolint:exhaustruct
		mgr.healthMap["uri-b"] = &NodeHealth{LastSelectedAt: now - 1, InFlight: 0} //nolint:exhaustruct
		mgr.healthMap["uri-c"] = &NodeHealth{LastSelectedAt: now - 1, InFlight: 0} //nolint:exhaustruct
		mgr.mu.Unlock()

		sel := mgr.SelectForParallel(3, false)
		if len(sel) != 3 {
			t.Fatalf("expected 3 selected, got %d", len(sel))
		}
		// All 3 should still be present (best-effort, no replacement possible)
		got := make(map[string]bool)
		for _, s := range sel {
			got[s.RawURI] = true
		}
		for _, uri := range []string{"uri-a", "uri-b", "uri-c"} {
			if !got[uri] {
				t.Errorf("Expected %s to be in selected set, but it was replaced", uri)
			}
		}
	})

	// Scenario C: protection expired (LastSelectedAt >= 5s ago)
	t.Run("protection expired", func(t *testing.T) {
		mgr := newMemMgr(nil, Hooks{})
		n1 := Node{RawURI: "uri-a", Name: "a"}
		n2 := Node{RawURI: "uri-b", Name: "b"}
		n3 := Node{RawURI: "uri-c", Name: "c"}
		mgr.MergeNodes([]Node{n1, n2, n3})

		now := time.Now().Unix()
		mgr.mu.Lock()
		mgr.healthMap["uri-a"] = &NodeHealth{LastSelectedAt: now - 10, InFlight: 0} //nolint:exhaustruct
		mgr.healthMap["uri-b"] = &NodeHealth{LastSelectedAt: now - 10, InFlight: 0} //nolint:exhaustruct
		mgr.healthMap["uri-c"] = &NodeHealth{LastSelectedAt: now - 10, InFlight: 0} //nolint:exhaustruct
		mgr.mu.Unlock()

		sel := mgr.SelectForParallel(3, false)
		if len(sel) != 3 {
			t.Fatalf("expected 3 selected, got %d", len(sel))
		}
		// All nodes should be returned unchanged (protection expired)
		got := make(map[string]bool)
		for _, s := range sel {
			got[s.RawURI] = true
		}
		for _, uri := range []string{"uri-a", "uri-b", "uri-c"} {
			if !got[uri] {
				t.Errorf("Expected %s to be in selected set, but it was replaced", uri)
			}
		}
	})
}

func TestRecordRateLimit_CooldownUntil(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.RecordRateLimit("uri1", 30)
	h, ok := mgr.LoadHealth()["uri1"]
	if !ok {
		t.Fatal("Expected health entry for uri1")
	}
	now := time.Now().Unix()
	if h.CooldownUntil <= now {
		t.Errorf("Expected CooldownUntil > now, got %d (now=%d)", h.CooldownUntil, now)
	}
	if h.CooldownUntil < now+28 || h.CooldownUntil > now+32 {
		t.Errorf("Expected CooldownUntil ~ now+30, got %d (now=%d)", h.CooldownUntil, now)
	}
}

func TestRecordTest_CooldownNotClearedOnFailure(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "node1"}
	mgr.MergeNodes([]Node{n1})

	mgr.RecordRateLimit("uri1", 30)
	h, ok := mgr.LoadHealth()["uri1"]
	if !ok {
		t.Fatal("Expected health entry")
	}
	cooldown := h.CooldownUntil
	now := time.Now().Unix()
	if cooldown <= now {
		t.Fatalf("Expected CooldownUntil in the future after RecordRateLimit, got %d (now=%d)", cooldown, now)
	}

	mgr.RecordTest("uri1", false, 0, "timeout")

	h2, ok := mgr.LoadHealth()["uri1"]
	if !ok {
		t.Fatal("Expected health entry after RecordTest")
	}
	if h2.CooldownUntil != cooldown {
		t.Errorf("RecordTest failure should NOT clear CooldownUntil: got %d, want %d", h2.CooldownUntil, cooldown)
	}
}

func TestSelectForParallel_Tier2RoundRobin(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri-a", Name: "a"}
	n2 := Node{RawURI: "uri-b", Name: "b"}
	n3 := Node{RawURI: "uri-c", Name: "c"}
	mgr.MergeNodes([]Node{n1, n2, n3})

	now := time.Now().Unix()
	mgr.mu.Lock()
	// All Tier 2: LastSubHealthyAt with different values → deterministic sort.
	// Sort order (InFlight=0 → LastSelectedAt=0 → LastSubHealthyAt ASC): a(100), b(50), c(10)
	mgr.healthMap["uri-a"] = &NodeHealth{LastSubHealthyAt: now - 100} //nolint:exhaustruct
	mgr.healthMap["uri-b"] = &NodeHealth{LastSubHealthyAt: now - 50}  //nolint:exhaustruct
	mgr.healthMap["uri-c"] = &NodeHealth{LastSubHealthyAt: now - 10}  //nolint:exhaustruct
	mgr.mu.Unlock()

	// sorted = [uri-a, uri-b, uri-c]
	// Starting from rrIndex=0:
	//   offset = (0+1)%3 = 1 → picks sorted[1] = uri-b
	//   offset = (1+1)%3 = 2 → picks sorted[2] = uri-c
	//   offset = (2+1)%3 = 0 → picks sorted[0] = uri-a
	// Note: actual distribution may not be perfectly even because
	// SelectForParallel updates LastSelectedAt on selected nodes,
	// which shifts the sort order in subsequent calls.
	rrIndexStore(mgr, 0)

	got := make(map[string]int)
	for call := 0; call < 9; call++ {
		sel := mgr.SelectForParallel(1, false)
		if len(sel) != 1 {
			t.Fatalf("call %d: expected 1 selected, got %d", call, len(sel))
		}
		got[sel[0].RawURI]++
	}

	for _, uri := range []string{"uri-a", "uri-b", "uri-c"} {
		if got[uri] == 0 {
			t.Errorf("%s was never selected — round-robin not working", uri)
		}
	}
	for _, uri := range []string{"uri-a", "uri-b", "uri-c"} {
		if got[uri] > 6 {
			t.Errorf("%s selected %d/9 times — likely no round-robin", uri, got[uri])
		}
	}
}

func TestSelectForParallel_Tier2SkipsCooldown(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri-a", Name: "a"}
	n2 := Node{RawURI: "uri-b", Name: "b"}
	n3 := Node{RawURI: "uri-c", Name: "c"}
	mgr.MergeNodes([]Node{n1, n2, n3})

	now := time.Now().Unix()
	mgr.mu.Lock()
	// All Tier 2. uri-a is in cooldown.
	// Different LastSubHealthyAt to ensure deterministic sort: [uri-a, uri-b, uri-c]
	mgr.healthMap["uri-a"] = &NodeHealth{LastSubHealthyAt: now - 100, CooldownUntil: now + 300} //nolint:exhaustruct
	mgr.healthMap["uri-b"] = &NodeHealth{LastSubHealthyAt: now - 50}                            //nolint:exhaustruct
	mgr.healthMap["uri-c"] = &NodeHealth{LastSubHealthyAt: now - 10}                            //nolint:exhaustruct
	mgr.mu.Unlock()

	rrIndexStore(mgr, 0)
	// offset=(0+1)%3=1 → skips iterating [uri-b, uri-c]; uri-a in group but CooldownUntil>now
	selected := mgr.SelectForParallel(2, false)
	if len(selected) != 2 {
		t.Errorf("Expected 2 nodes (skip cooldown), got %d", len(selected))
	}
	for _, s := range selected {
		if s.RawURI == "uri-a" {
			t.Errorf("uri-a should be skipped due to CooldownUntil > now, but it was selected")
		}
	}

	// Requesting 3 with only 2 not in cooldown → get only 2
	selected2 := mgr.SelectForParallel(3, false)
	if len(selected2) != 2 {
		t.Errorf("Expected 2 nodes even when k=3 (uri-a in cooldown), got %d", len(selected2))
	}
}

func TestRecordTest_AutoDisableSyncsMemory(t *testing.T) {
	// 注入探测阈值 3：它只管探测阶段——首次移出不受阈值门控（由策略驱动）。
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 3 })

	n1 := Node{RawURI: "uri1", Name: "node1"}
	n2 := Node{RawURI: "uri2", Name: "node2"}
	mgr.MergeNodes([]Node{n1, n2})

	// 首次可归因失败即摘出（默认策略「禁用后恢复探测」），并同步内存禁用状态
	mgr.RecordTest("uri1", false, 0, "dial tcp timeout")

	// LoadNodes 读取内存节点列表，须实时反映禁用状态
	nodes := mgr.LoadNodes()
	found := false
	for _, n := range nodes {
		if n.RawURI == "uri1" {
			found = true
			if !n.Disabled {
				t.Errorf("首次可归因失败后 uri1 应 Disabled=true")
			}
			if n.DisableSource != nodestore.DisableSourceHealth {
				t.Errorf("来源=%q, want %q", n.DisableSource, nodestore.DisableSourceHealth)
			}
		}
	}
	if !found {
		t.Fatalf("Expected uri1 in node list")
	}

	// SelectForParallel 不得再选中被自动禁用的节点
	selected := mgr.SelectForParallel(2, false)
	for _, s := range selected {
		if s.RawURI == "uri1" {
			t.Errorf("Disabled node uri1 must not be selected by SelectForParallel")
		}
	}

	// 自动恢复：健康触发的禁用，成功测速后应解除；人工禁用则不会（另行覆盖）。
	mgr.RecordTest("uri1", true, 123, "")
	for _, n := range mgr.LoadNodes() {
		if n.RawURI == "uri1" && n.Disabled {
			t.Fatalf("健康触发的禁用应在成功测速后自动解除")
		}
	}

	// 人工禁用的节点不得被成功测速自动放开
	mgr.RecordTest("uri1", true, 123, "") // 归零连败
	for i := 0; i < 3; i++ {
		mgr.RecordTest("uri1", false, 0, "dial tcp timeout")
	}
	if !mgr.EnableNode("uri1") {
		t.Fatalf("EnableNode 应能手动恢复")
	}
	// 此时节点为人工启用态（Disabled=false、无 DisabledByHealth 标记），
	// 再次连败禁用后人工 BatchUpdateNodesDisabled 清标记，成功测速不得自动放开。
	for i := 0; i < 3; i++ {
		mgr.RecordTest("uri1", false, 0, "dial tcp timeout")
	}
	mgr.BatchUpdateNodesDisabled([]string{"uri1"}, true)
	mgr.RecordTest("uri1", true, 123, "")
	for _, n := range mgr.LoadNodes() {
		if n.RawURI == "uri1" && !n.Disabled {
			t.Fatalf("人工禁用的节点不得被成功测速自动放开")
		}
	}
}

// TestRecordRateLimit_NotMarkedAsFailure 守卫：429 是上游正常竞争限流，
// 只写冷却与独立计次，不写任何失败/降权字段（此前会写 LastSubHealthyAt/
// LastTestError/LastFailAt，惩罚性降权且与"不递增连败"自相矛盾）。
func TestRecordRateLimit_NotMarkedAsFailure(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-429", Name: "n"}}) //nolint:exhaustruct

	mgr.RecordRateLimit("uri-429", 30)
	h, ok := mgr.LoadHealth()["uri-429"]
	if !ok {
		t.Fatal("应有健康条目")
	}
	if h.LastSubHealthyAt != 0 {
		t.Errorf("429 不得写 LastSubHealthyAt（惩罚性降权）, got %d", h.LastSubHealthyAt)
	}
	if h.LastTestError != "" || h.LastFailAt != 0 {
		t.Errorf("429 不得写失败记账: err=%q failAt=%d", h.LastTestError, h.LastFailAt)
	}
	if h.Last429At == 0 || h.RateLimitCount != 1 {
		t.Errorf("429 应写独立计次: last429=%d count=%d", h.Last429At, h.RateLimitCount)
	}
	now := time.Now().Unix()
	if h.CooldownUntil <= now {
		t.Errorf("应设置冷却: %d", h.CooldownUntil)
	}
	// 冷却内 Tier 2（选点跳过）；冷却过期自动回 Tier 1（无冷却外持续惩罚）。
	if tier := getNodeTier(Node{RawURI: "uri-429", Name: "n"}, &h, now); tier != 2 {
		t.Errorf("冷却内应 Tier 2, got %d", tier)
	}
	if tier := getNodeTier(Node{RawURI: "uri-429", Name: "n"}, &h, now+31); tier != 1 {
		t.Errorf("冷却过期应回 Tier 1, got %d", tier)
	}
}

// TestRecordSlowAvoid_MarksSubHealthy 反向用例：流式空闲超时（慢/僵死）是节点
// 劣化信号，除冷却外还必须写亚健康降权与失败记账——与 429 语义分离。
func TestRecordSlowAvoid_MarksSubHealthy(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-slow", Name: "s"}}) //nolint:exhaustruct

	mgr.RecordSlowAvoid("uri-slow", 15)
	h, ok := mgr.LoadHealth()["uri-slow"]
	if !ok {
		t.Fatal("应有健康条目")
	}
	if h.LastSubHealthyAt == 0 {
		t.Error("idle 应写 LastSubHealthyAt（Tier 2 降权）")
	}
	if h.LastTestError != "stream idle timeout" || h.LastFailAt == 0 {
		t.Errorf("idle 应写失败记账: err=%q failAt=%d", h.LastTestError, h.LastFailAt)
	}
}
