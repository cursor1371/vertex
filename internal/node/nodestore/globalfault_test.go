package nodestore

import "testing"

// TestGlobalFaultGuardTripsOnMultipleNodes 锁定保护器的核心判据：窗口内**不同节点**同因失败
// 达到阈值才判定为疑似全局故障；单个节点反复失败不是全局故障（那是它自己的问题）。
func TestGlobalFaultGuardTripsOnMultipleNodes(t *testing.T) {
	guard := NewGlobalFaultGuard(func() (bool, int, int) { return true, 120, 3 })
	now := int64(1000)

	if guard.Observe(FailureNodeNetwork, "node-1", now) {
		t.Fatal("1 个节点失败不应判定为全局故障")
	}
	if guard.Observe(FailureNodeNetwork, "node-1", now+1) {
		t.Fatal("同一节点反复失败不应判定为全局故障（那是节点自身故障）")
	}
	if guard.Observe(FailureNodeNetwork, "node-2", now+2) {
		t.Fatal("2 个节点未达阈值 3，不应判定")
	}
	if !guard.Observe(FailureNodeNetwork, "node-3", now+3) {
		t.Fatal("3 个不同节点同因失败应判定为疑似全局故障")
	}
}

// TestGlobalFaultGuardWindowExpiry 窗口内的旧事件必须失效：窗口滑过且无新同类失败后，
// 保护自动解除（否则一次抖动会永久冻结自动禁用）。
func TestGlobalFaultGuardWindowExpiry(t *testing.T) {
	guard := NewGlobalFaultGuard(func() (bool, int, int) { return true, 60, 3 })
	now := int64(5000)
	for _, u := range []string{"a", "b", "c"} {
		guard.Observe(FailureNodeNetwork, u, now)
	}
	if len(guard.Active(now+1)) == 0 {
		t.Fatal("窗口内应处于活跃状态")
	}
	// 窗口过后：新事件只带一个节点，旧事件全部过期，不应再判定。
	if guard.Observe(FailureNodeNetwork, "d", now+61) {
		t.Fatal("窗口过期后不应继续判定为全局故障")
	}
	if len(guard.Active(now+61)) != 0 {
		t.Errorf("窗口过期后 Active 应为空, got %v", guard.Active(now+61))
	}
}

// TestGlobalFaultGuardClassesAreIndependent 不同类别的失败互不累加：
// DNS 与网络类各自统计，避免"两个类别各 2 个节点"被凑成"4 个节点"。
func TestGlobalFaultGuardClassesAreIndependent(t *testing.T) {
	guard := NewGlobalFaultGuard(func() (bool, int, int) { return true, 120, 3 })
	now := int64(100)
	for _, u := range []string{"a", "b"} {
		guard.Observe(FailureNodeNetwork, u, now)
		guard.Observe(FailureDNS, u, now)
	}
	if guard.Observe(FailureNodeNetwork, "c", now) != true {
		t.Fatal("网络类 3 个节点应判定")
	}
	// 独立性判据用 Active 而不是继续 Observe：DNS 此时只有 a/b 两个节点，
	// 若它与网络类共享计数，Active 会把 DNS 也算成活跃。
	active := guard.Active(now)
	if _, ok := active[string(FailureNodeNetwork)]; !ok {
		t.Errorf("网络类应处于活跃状态, got %v", active)
	}
	if _, ok := active[string(FailureDNS)]; ok {
		t.Errorf("DNS 类只有 2 个节点，不应活跃（类别计数必须相互独立）, got %v", active)
	}
}

// TestGlobalFaultGuardDisabled 开关关闭时永不判定（等价取消该保护）。
func TestGlobalFaultGuardDisabled(t *testing.T) {
	guard := NewGlobalFaultGuard(func() (bool, int, int) { return false, 120, 2 })
	for _, u := range []string{"a", "b", "c", "d"} {
		if guard.Observe(FailureNodeNetwork, u, 1) {
			t.Fatal("保护关闭时不应判定为全局故障")
		}
	}
	if len(guard.Active(1)) != 0 {
		t.Error("保护关闭时 Active 应为空")
	}
}

// TestGlobalFaultGuardNilSafe 无配置/未注入时必须是安全的空操作（测试装配常传 nil）。
func TestGlobalFaultGuardNilSafe(t *testing.T) {
	var g *GlobalFaultGuard
	if g.Observe(FailureNodeNetwork, "a", 1) {
		t.Error("nil 保护器不应判定")
	}
	if len(g.Active(1)) != 0 {
		t.Error("nil 保护器 Active 应为空")
	}
	noCfg := NewGlobalFaultGuard(nil)
	if noCfg.Observe(FailureNodeNetwork, "a", 1) {
		t.Error("无配置保护器不应判定")
	}
	// 空 uri 不参与统计（直连/前置无归属节点的场景）。
	guard := NewGlobalFaultGuard(func() (bool, int, int) { return true, 120, 2 })
	if guard.Observe(FailureNodeNetwork, "", 1) {
		t.Error("空 uri 不应参与统计")
	}
}

// TestFailureClassGuardScope 全局保护的类别范围：node_network + permission + auth。
// 后两者纳入，正是"仅当能够明确判断为全局账号 / API Key / 认证配置问题时才不归因到单节点"
// 的落地判据；DNS 不纳入——它本就不禁用，无需为它引入一套额外逻辑。
func TestFailureClassGuardScope(t *testing.T) {
	for _, c := range []FailureClass{FailureNodeNetwork, FailurePermission, FailureAuth} {
		if !c.GuardsAgainstGlobalFault() {
			t.Errorf("%s 应受全局保护约束", c)
		}
	}
	for _, c := range []FailureClass{FailureDNS, FailureRateLimit, FailureTimeout, FailureUpstream5xx, FailureProbeFlow, FailureUnsupported, FailureUnknown} {
		if c.GuardsAgainstGlobalFault() {
			t.Errorf("%s 不应受全局保护约束（DNS 本就不禁用，无需保护）", c)
		}
	}
}
