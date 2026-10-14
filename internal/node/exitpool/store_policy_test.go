package exitpool

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// policySetWith 构造"仅指定类别自定义"的策略集（全局默认阈值 global）。
func policySetWith(t *testing.T, global int, rows ...nodestore.FaultPolicy) *nodestore.PolicySet {
	t.Helper()
	set := nodestore.NewPolicySet(func() int { return global })
	set.Replace(rows)
	return set
}

func probeFail(t *testing.T, mgr *Manager) {
	t.Helper()
	mgr.ReportProbe("u1", ProbeResult{
		OK: false, ErrStr: "dial tcp: connection refused",
		Source: nodestore.ProbeSourceAutoProbe,
	})
}

// TestPolicy_RecordOnlyTakesEffectImmediately 把节点网络故障配成"不禁用"后，
// 该类失败立即不再摘出（策略即时生效，无需重启）；失败照常记账与进故障记录。
func TestPolicy_RecordOnlyTakesEffectImmediately(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.SetPolicySet(policySetWith(t, 2, nodestore.FaultPolicy{
		Class: nodestore.FailureNodeNetwork, Action: nodestore.PolicyRecordOnly,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}))
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	for i := 0; i < 3; i++ {
		if mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"}) {
			t.Fatal("策略为不禁用时不得摘出节点")
		}
	}
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatal("节点应保持可用")
	}
	if h := mgr.LoadHealth()["u1"]; h.ConsecutiveFailures != 3 || h.LastFailureClass != "node_network" {
		t.Errorf("失败仍应照常记账: %+v", h)
	}
	if mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("不禁用的类别不得进入自动探测名单")
	}
}

// TestPolicy_SingleShotNoRecovery 单次禁用：摘出、不进探测名单、来源 single_shot，
// 且成功探测也不得自动放开（只能人工启用）。
func TestPolicy_SingleShotNoRecovery(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.SetPolicySet(policySetWith(t, 2, nodestore.FaultPolicy{
		Class: nodestore.FailureNodeNetwork, Action: nodestore.PolicySingleShot,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}))
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	if !mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"}) {
		t.Fatal("单次禁用策略下首次失败应摘出")
	}
	n := mgr.LoadNodes()[0]
	if n.DisableSource != nodestore.DisableSourceSingleShot {
		t.Errorf("来源=%q, want %q", n.DisableSource, nodestore.DisableSourceSingleShot)
	}
	if mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("单次禁用不得进入自动探测名单（否则会绕一圈被恢复）")
	}
	mgr.ReportProbe("u1", ProbeResult{OK: true, Ms: 50, Source: nodestore.ProbeSourceAutoProbe})
	if mgr.LoadNodes()[0].Disabled == false {
		t.Error("单次禁用的节点不得被成功探测自动放开")
	}
}

// TestPolicy_ProbeThresholdZeroMeansUnlimited N=0 = 无限探测：探测连败再多也不转永久，
// 但探测成功仍照常恢复。
func TestPolicy_ProbeThresholdZeroMeansUnlimited(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.SetPolicySet(policySetWith(t, 0))                   // 全局默认 0 → node_network 跟随全局 = 无限
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	if n := mgr.LoadNodes()[0]; n.DisableSource != nodestore.DisableSourceHealth {
		t.Fatalf("首次失败应摘出并进探测, got %q", n.DisableSource)
	}
	for i := 0; i < 6; i++ {
		probeFail(t, mgr)
	}
	n := mgr.LoadNodes()[0]
	if n.DisableSource != nodestore.DisableSourceHealth {
		t.Fatalf("N=0（无限探测）不得转永久, got %q", n.DisableSource)
	}
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("无限探测的节点必须仍在探测名单里")
	}
	// 探测成功仍恢复。
	mgr.ReportProbe("u1", ProbeResult{OK: true, Ms: 40, Source: nodestore.ProbeSourceAutoProbe})
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Error("无限探测下探测成功仍应恢复")
	}
}

// TestPolicy_ProbeThresholdBoundary N 次阈值边界：第 N-1 次不转永久，第 N 次转。
func TestPolicy_ProbeThresholdBoundary(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 3 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	for i := 0; i < 2; i++ { // 第 1、2 次探测失败：尚未达阈值 3
		probeFail(t, mgr)
		if src := mgr.LoadNodes()[0].DisableSource; src != nodestore.DisableSourceHealth {
			t.Fatalf("第 %d 次探测失败不应转永久, got %q", i+1, src)
		}
	}
	probeFail(t, mgr) // 第 3 次：达阈值
	n := mgr.LoadNodes()[0]
	if n.DisableSource != nodestore.DisableSourcePermanent {
		t.Fatalf("第 3 次探测失败应转永久, got %q", n.DisableSource)
	}
	if mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("永久禁用应退出探测名单")
	}
}

// TestPolicy_NoRetroactiveChange 策略变更**不回溯**存量节点（评审定稿的"不隐式恢复"）：
//   - health 禁用 + 类别改为不禁用 → 仍保持禁用（不因改配置被立即放开）；
//   - permanent 禁用 + 类别改为不禁用 → 同样不动；
//   - 来源与禁用时间都不被改写。
func TestPolicy_NoRetroactiveChange(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	set := nodestore.NewPolicySet(func() int { return 2 })
	mgr.SetPolicySet(set)
	mgr.seedUnsafe([]Node{
		{RawURI: "u1", Name: "n1"}, //nolint:exhaustruct
		{RawURI: "u2", Name: "n2"}, //nolint:exhaustruct
	}, nil)

	// u1：health 禁用；u2：探测连败升为 permanent。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	mgr.ReportProbe("u2", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	for i := 0; i < 2; i++ {
		mgr.ReportProbe("u2", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused", Source: nodestore.ProbeSourceAutoProbe})
	}
	before := map[string]struct {
		src string
		at  int64
	}{}
	for _, n := range mgr.LoadNodes() {
		before[n.RawURI] = struct {
			src string
			at  int64
		}{n.DisableSource, n.DisabledAt}
	}
	if before["u2"].src != nodestore.DisableSourcePermanent {
		t.Fatalf("前置条件：u2 应为 permanent, got %q", before["u2"].src)
	}

	// 把该类改成"不禁用"。
	set.Replace([]nodestore.FaultPolicy{{
		Class: nodestore.FailureNodeNetwork, Action: nodestore.PolicyRecordOnly,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}})

	// 再来一次请求路径失败：两者都必须保持原状。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	mgr.ReportProbe("u2", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})

	for _, n := range mgr.LoadNodes() {
		want := before[n.RawURI]
		if !n.Disabled {
			t.Errorf("%s 不应因策略变更被放开（不回溯）", n.RawURI)
		}
		if n.DisableSource != want.src {
			t.Errorf("%s 来源被改写: %q → %q", n.RawURI, want.src, n.DisableSource)
		}
		if n.DisabledAt != want.at {
			t.Errorf("%s 禁用时间被改写: %d → %d", n.RawURI, want.at, n.DisabledAt)
		}
	}
	// health 节点仍在探测名单（其唯一自动恢复通道是探测成功）。
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("health 节点应仍在探测名单（恢复只走探测成功）")
	}
}

// TestGlobalFaultGuard_OnlyPreventsDoesNotRecover 你确认的第 1 条锁死语义：
// 全局故障保护命中**只阻止后续节点被摘除**，不恢复任何已禁用节点。
// 本用例构造"先有节点被摘出 → 再触发保护命中"，断言既有禁用状态与禁用时间完全不变。
func TestGlobalFaultGuard_OnlyPreventsDoesNotRecover(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
	mgr.SetGlobalFaultGuard(nodestore.NewGlobalFaultGuard(func() (bool, int, int) { return true, 120, 3 }))
	mgr.seedUnsafe([]Node{
		{RawURI: "u1", Name: "n1"}, //nolint:exhaustruct
		{RawURI: "u2", Name: "n2"}, //nolint:exhaustruct
		{RawURI: "u3", Name: "n3"}, //nolint:exhaustruct
	}, nil)

	// u1 先被摘出（保护尚未命中）。
	if !mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"}) {
		t.Fatal("保护命中前应正常摘出 u1")
	}
	snapU1 := mgr.LoadNodes()[0]
	if !snapU1.Disabled || snapU1.DisabledAt == 0 {
		t.Fatalf("前置条件：u1 应已禁用: %+v", snapU1)
	}

	// u2、u3 相继失败 → 第 3 个不同节点触发保护命中。
	mgr.ReportProbe("u2", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	mgr.ReportProbe("u3", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"}) // 命中
	if n := mgr.LoadNodes()[2]; n.Disabled {
		t.Fatal("保护命中后不得再摘出节点")
	}
	if len(mgr.GlobalFaultActive()) == 0 {
		t.Fatal("前置条件：保护应处于活跃状态")
	}

	// 关键断言：已禁用的 u1 不被保护命中"回滚"，状态与时间都不变。
	after := mgr.LoadNodes()[0]
	if !after.Disabled {
		t.Error("保护命中不得恢复已禁用节点（只防不治）")
	}
	if after.DisableSource != snapU1.DisableSource || after.DisabledAt != snapU1.DisabledAt {
		t.Errorf("已禁用节点的来源/时间不得被保护命中改写: %+v → %+v", snapU1, after)
	}
	// 它仍按自身来源的既有规则处理：health → 在探测名单里，靠探测成功恢复。
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("health 节点应仍在探测名单（恢复走既有探测路径，与保护器无关）")
	}
}

// TestPolicy_401DefaultClassFromPolicy auth 类别的动作同样由策略表决定：
// 默认"禁用后恢复探测"，改成单次禁用后来源随之变为 single_shot。
func TestPolicy_401DefaultClassFromPolicy(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	// 显式类别（请求路径从 VertexError.Kind 得知）走默认策略 → health。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "opaque", Class: nodestore.FailureAuth})
	if src := mgr.LoadNodes()[0].DisableSource; src != nodestore.DisableSourceHealth {
		t.Fatalf("auth 默认策略应摘出并进探测, got %q", src)
	}

	// 改成单次禁用后，同一路径的来源变为 single_shot（策略统一生效，不在代码里分叉）。
	mgr2 := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr2.SetPolicySet(policySetWith(t, 2, nodestore.FaultPolicy{
		Class: nodestore.FailureAuth, Action: nodestore.PolicySingleShot,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}))
	mgr2.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct
	mgr2.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "opaque", Class: nodestore.FailureAuth})
	if src := mgr2.LoadNodes()[0].DisableSource; src != nodestore.DisableSourceSingleShot {
		t.Fatalf("auth 配成单次禁用后来源应为 single_shot, got %q", src)
	}
}

// TestPolicy_DisableByHealthFollowsPolicy DisableByHealth 是策略化入口：
// 类别被配成不禁用时，它不应再摘出节点（避免留下"绕过策略"的旁路）。
func TestPolicy_DisableByHealthFollowsPolicy(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.SetPolicySet(policySetWith(t, 2, nodestore.FaultPolicy{
		Class: nodestore.FailureAuth, Action: nodestore.PolicyRecordOnly,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}))
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	mgr.DisableByHealth([]string{"u1"}, nodestore.FailureAuth)
	if mgr.LoadNodes()[0].Disabled {
		t.Error("类别策略为不禁用时，DisableByHealth 不得摘出节点（否则策略可被旁路）")
	}
}
