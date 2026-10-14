package exitpool

import (
	"testing"
	"time"
)

// TestSelectForParallelRelaxed_RespectsLongCooldown 验证宽松通道不再无限重打限流节点：
// 全员处于长冷却（剩余 > 5s）时严格与宽松通道均返回空；剩余冷却 ≤5s 的节点可被提前补位。
func TestSelectForParallelRelaxed_RespectsLongCooldown(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri-a", Name: "a"}
	n2 := Node{RawURI: "uri-b", Name: "b"}
	mgr.MergeNodes([]Node{n1, n2})

	now := time.Now().Unix()
	mgr.mu.Lock()
	mgr.healthMap["uri-a"] = &NodeHealth{LastSubHealthyAt: now - 100, CooldownUntil: now + 300} //nolint:exhaustruct
	mgr.healthMap["uri-b"] = &NodeHealth{LastSubHealthyAt: now - 50, CooldownUntil: now + 300}  //nolint:exhaustruct
	mgr.mu.Unlock()

	if strict := mgr.SelectForParallel(2, false); len(strict) != 0 {
		t.Errorf("Strict selection must return empty when all nodes cooling down, got %d", len(strict))
	}

	if relaxed := mgr.SelectForParallelRelaxed(2, false); len(relaxed) != 0 {
		t.Fatalf("Relaxed selection must skip nodes in long cooldown (>5s), got %d: %v", len(relaxed), relaxed)
	}
}

// TestSelectForParallelRelaxed_NearEndCooldownAllowed 验证剩余冷却 ≤5s 的节点
// 宽松通道允许提前补位（冷却临近结束不再机械排除）。
func TestSelectForParallelRelaxed_NearEndCooldownAllowed(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.MergeNodes([]Node{
		{RawURI: "uri-a", Name: "a"},
		{RawURI: "uri-b", Name: "b"},
	})

	now := time.Now().Unix()
	mgr.mu.Lock()
	mgr.healthMap["uri-a"] = &NodeHealth{LastSubHealthyAt: now - 100, CooldownUntil: now + 300} //nolint:exhaustruct
	mgr.healthMap["uri-b"] = &NodeHealth{LastSubHealthyAt: now - 50, CooldownUntil: now + 3}    //nolint:exhaustruct
	mgr.mu.Unlock()

	relaxed := mgr.SelectForParallelRelaxed(2, false)
	if len(relaxed) != 1 || relaxed[0].RawURI != "uri-b" {
		t.Fatalf("Expected only near-end-cooldown uri-b, got %v", relaxed)
	}
}

// TestSelectForParallelRelaxed_RespectsDisabled 验证宽松通道始终排除 Disabled 节点（手动禁用不做豁免）。
func TestSelectForParallelRelaxed_RespectsDisabled(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri-ok", Name: "ok"}
	n2 := Node{RawURI: "uri-dis", Name: "dis", Disabled: true}
	n3 := Node{RawURI: "uri-auto", Name: "auto"}
	mgr.MergeNodes([]Node{n1, n2, n3})

	now := time.Now().Unix()
	mgr.mu.Lock()
	mgr.healthMap["uri-ok"] = &NodeHealth{LastSubHealthyAt: now - 10}                            //nolint:exhaustruct
	mgr.healthMap["uri-auto"] = &NodeHealth{LastSubHealthyAt: now - 20, CooldownUntil: now + 60} //nolint:exhaustruct
	mgr.mu.Unlock()

	relaxed := mgr.SelectForParallelRelaxed(3, false)
	for _, s := range relaxed {
		if s.RawURI == "uri-dis" {
			t.Errorf("Disabled node uri-dis must not be selected by relaxed path")
		}
	}
	// uri-auto 处于长冷却（剩余 60s > 5s），宽松通道应跳过；仅返回 uri-ok。
	if len(relaxed) != 1 || relaxed[0].RawURI != "uri-ok" {
		t.Errorf("Expected only uri-ok (long-cooldown uri-auto skipped, disabled excluded), got %v", relaxed)
	}
}

// TestSelectForParallelRelaxed_KExceedsAvailable 验证 k 大于可用数时返回全部可用且不 panic。
func TestSelectForParallelRelaxed_KExceedsAvailable(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.MergeNodes([]Node{{RawURI: "uri-1", Name: "n1"}})

	relaxed := mgr.SelectForParallelRelaxed(5, false)
	if len(relaxed) != 1 || relaxed[0].RawURI != "uri-1" {
		t.Errorf("Expected exactly uri-1, got %v", relaxed)
	}
}

// TestSelectForParallelRelaxed_InFlightOrdering 验证宽松通道路径的排序语义与严格路径一致：
// Tier 1 内按 InFlight 升序优先（低负载节点先被选中）。
func TestSelectForParallelRelaxed_InFlightOrdering(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.MergeNodes([]Node{
		{RawURI: "uri-busy", Name: "busy"},
		{RawURI: "uri-idle", Name: "idle"},
	})

	now := time.Now().Unix()
	mgr.mu.Lock()
	mgr.healthMap["uri-busy"] = &NodeHealth{LastSubHealthyAt: now - 10, InFlight: 5} //nolint:exhaustruct
	mgr.healthMap["uri-idle"] = &NodeHealth{LastSubHealthyAt: now - 10}              //nolint:exhaustruct
	mgr.mu.Unlock()

	relaxed := mgr.SelectForParallelRelaxed(2, false)
	if len(relaxed) != 2 {
		t.Fatalf("Expected both nodes selected, got %d", len(relaxed))
	}
	if relaxed[0].RawURI != "uri-idle" {
		t.Errorf("Relaxed selection must order lower InFlight node first, got %s before %s",
			relaxed[0].RawURI, relaxed[1].RawURI)
	}
}

// TestSelectForParallel_StrictUnchangedAfterCoreExtract 回归锚点：核心方法抽取后严格路径冷却语义不变。
func TestSelectForParallel_StrictUnchangedAfterCoreExtract(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.MergeNodes([]Node{
		{RawURI: "uri-a", Name: "a"},
		{RawURI: "uri-b", Name: "b"},
	})

	now := time.Now().Unix()
	mgr.mu.Lock()
	mgr.healthMap["uri-a"] = &NodeHealth{LastSubHealthyAt: now - 100, CooldownUntil: now + 300} //nolint:exhaustruct
	mgr.healthMap["uri-b"] = &NodeHealth{LastSubHealthyAt: now - 50}                            //nolint:exhaustruct
	mgr.mu.Unlock()

	selected := mgr.SelectForParallel(3, false)
	if len(selected) != 1 {
		t.Fatalf("Expected only non-cooldown node in strict mode, got %d", len(selected))
	}
	if selected[0].RawURI != "uri-b" {
		t.Errorf("Expected uri-b (non-cooldown), got %s", selected[0].RawURI)
	}
}
