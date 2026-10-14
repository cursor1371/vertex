package nodestore

import "testing"

// TestPolicySet_DefaultsResolve 无任何自定义行时，解析结果必须等于内置默认表。
func TestPolicySet_DefaultsResolve(t *testing.T) {
	set := NewPolicySet(func() int { return 3 })
	for _, c := range KnownFailureClasses() {
		pol := set.For(c)
		if pol.Action != BuiltinPolicyAction(c) {
			t.Errorf("%s 动作=%q, want 内置默认 %q", c, pol.Action, BuiltinPolicyAction(c))
		}
		if !pol.ActionIsDefault {
			t.Errorf("%s ActionIsDefault 应为 true", c)
		}
		if c == FailureNodeNetwork || c == FailurePermission || c == FailureAuth {
			if pol.ProbeFailThreshold != 3 {
				t.Errorf("%s 阈值应跟随全局 3, got %d", c, pol.ProbeFailThreshold)
			}
			if !pol.ThresholdFollowsGlobal {
				t.Errorf("%s ThresholdFollowsGlobal 应为 true", c)
			}
		}
	}
}

// TestPolicySet_CustomActionOverrides 自定义动作覆盖默认；阈值按"自定义 > 全局"解析。
func TestPolicySet_CustomActionOverrides(t *testing.T) {
	set := NewPolicySet(func() int { return 2 })
	set.Replace([]FaultPolicy{
		{Class: FailureNodeNetwork, Action: PolicyRecordOnly, ProbeFailThreshold: ProbeThresholdFollowGlobal},
		{Class: FailurePermission, Action: PolicySingleShot, ProbeFailThreshold: ProbeThresholdFollowGlobal},
		{Class: FailureAuth, Action: PolicyDisableThenProbe, ProbeFailThreshold: 7},
		{Class: FailureUnknown, Action: PolicyDisableThenProbe, ProbeFailThreshold: ProbeThresholdUnlimited},
	})

	nn := set.For(FailureNodeNetwork)
	if nn.Action != PolicyRecordOnly || nn.DisablesOnFirstFailure() || nn.ProbesForRecovery() {
		t.Errorf("node_network 被改为不禁用后不应摘出/进探测: %+v", nn)
	}
	if nn.ActionIsDefault {
		t.Error("node_network 已被自定义，ActionIsDefault 应为 false")
	}

	perm := set.For(FailurePermission)
	if perm.Action != PolicySingleShot || !perm.DisablesOnFirstFailure() || perm.ProbesForRecovery() {
		t.Errorf("permission 单次禁用语义错误: %+v", perm)
	}

	auth := set.For(FailureAuth)
	if auth.ProbeFailThreshold != 7 || auth.ThresholdFollowsGlobal {
		t.Errorf("auth 阈值应为自定义 7 且非跟随全局: %+v", auth)
	}

	// 0 = 无限探测（自定义阈值优先于全局 2）。
	other := set.For(FailureUnknown)
	if other.ProbeFailThreshold != ProbeThresholdUnlimited || other.ThresholdFollowsGlobal {
		t.Errorf("other 阈值应为自定义 0（无限）: %+v", other)
	}
}

// TestPolicySet_InvalidRowsFallBack 非法行一律丢弃并回落默认，不静默按可疑值执行。
func TestPolicySet_InvalidRowsFallBack(t *testing.T) {
	set := NewPolicySet(nil)
	set.Replace([]FaultPolicy{
		{Class: FailureNodeNetwork, Action: PolicyAction("disable_everything"), ProbeFailThreshold: 1},
		{Class: FailurePermission, Action: PolicyRecordOnly, ProbeFailThreshold: -99},
	})
	if got := set.For(FailureNodeNetwork).Action; got != PolicyDisableThenProbe {
		t.Errorf("非法动作应回落默认, got %q", got)
	}
	if got := set.For(FailurePermission).Action; got != PolicyDisableThenProbe {
		t.Errorf("非法阈值行应被整行丢弃并回落默认, got %q", got)
	}
}

// TestPolicySet_UnknownClassSafe 未知类别（未来新增分类）必须安全：不异常、
// 且默认不禁用——新增分类绝不能凭缺失配置就获得自动禁用能力。
func TestPolicySet_UnknownClassSafe(t *testing.T) {
	set := NewPolicySet(func() int { return 2 })
	future := FailureClass("some_future_class")
	pol := set.For(future)
	if pol.Action != PolicyRecordOnly {
		t.Errorf("未知类别应回落 record_only, got %q", pol.Action)
	}
	if pol.DisablesOnFirstFailure() {
		t.Error("未知类别不得自动摘出节点")
	}
	// 未知类别也必须出现在 All() 之外（All 只列内置类别），但不 panic。
	if len(set.All()) != len(KnownFailureClasses()) {
		t.Errorf("All() 应列出全部内置类别, got %d", len(set.All()))
	}
}

// TestPolicySet_GlobalThresholdZero 全局阈值 0 = 无限探测：未自定义的禁用类目全部继承。
func TestPolicySet_GlobalThresholdZero(t *testing.T) {
	set := NewPolicySet(func() int { return ProbeThresholdUnlimited })
	pol := set.For(FailureNodeNetwork)
	if pol.ProbeFailThreshold != ProbeThresholdUnlimited {
		t.Errorf("全局 0 应解析为无限探测, got %d", pol.ProbeFailThreshold)
	}
	if !pol.DisablesOnFirstFailure() {
		t.Error("无限探测仍应「首次即摘出」，只是不会转永久")
	}
}

// TestPolicySet_NilThresholdFuncFallback 阈值取值函数缺失时回落内置默认，不 panic。
func TestPolicySet_NilThresholdFuncFallback(t *testing.T) {
	set := NewPolicySet(nil)
	if got := set.For(FailureNodeNetwork).ProbeFailThreshold; got != DefaultProbeFailThreshold {
		t.Errorf("阈值缺失应回落 %d, got %d", DefaultProbeFailThreshold, got)
	}
	// 负值（配置异常）同样回落默认。
	set2 := NewPolicySet(func() int { return -5 })
	if got := set2.For(FailureNodeNetwork).ProbeFailThreshold; got != DefaultProbeFailThreshold {
		t.Errorf("负阈值应回落 %d, got %d", DefaultProbeFailThreshold, got)
	}
}

// TestPolicySet_RecordOnlyHidesThreshold 不禁用类别的探测阈值无意义，
// 解析结果统一为 0 并标记为跟随全局，避免面板出现"不禁用却显示阈值 7"。
func TestPolicySet_RecordOnlyHidesThreshold(t *testing.T) {
	set := NewPolicySet(func() int { return 5 })
	set.Replace([]FaultPolicy{{Class: FailureDNS, Action: PolicyRecordOnly, ProbeFailThreshold: 9}})
	pol := set.For(FailureDNS)
	if pol.ProbeFailThreshold != 0 || !pol.ThresholdFollowsGlobal {
		t.Errorf("不禁用类别的阈值应归一为 0/跟随全局: %+v", pol)
	}
}

// TestPolicySet_CustomizationsRoundTrip 自定义行可原样取回（管理接口回显用）。
func TestPolicySet_CustomizationsRoundTrip(t *testing.T) {
	set := NewPolicySet(nil)
	set.Replace([]FaultPolicy{{Class: FailurePermission, Action: PolicySingleShot, ProbeFailThreshold: ProbeThresholdFollowGlobal}})
	got := set.Customizations()
	c, ok := got[FailurePermission]
	if !ok {
		t.Fatalf("自定义行应可取回, got %v", got)
	}
	if c.Action != PolicySingleShot || !c.ThresholdFollowsGlobal {
		t.Errorf("回显不一致: %+v", c)
	}
	// Replace(nil) 等价重置。
	set.Replace(nil)
	if len(set.Customizations()) != 0 {
		t.Error("重置后不应残留自定义行")
	}
	if set.For(FailurePermission).Action != PolicyDisableThenProbe {
		t.Error("重置后应回落默认动作")
	}
}

// TestPolicyActionValid 动作枚举校验（PUT 与 DB 行合法性共用）。
func TestPolicyActionValid(t *testing.T) {
	for _, a := range []PolicyAction{PolicyRecordOnly, PolicySingleShot, PolicyDisableThenProbe} {
		if !PolicyActionValid(string(a)) {
			t.Errorf("%q 应为合法动作", a)
		}
	}
	for _, bad := range []string{"", "disable", "record_only ", "DISABLE"} {
		if PolicyActionValid(bad) {
			t.Errorf("%q 不应为合法动作", bad)
		}
	}
	// 字符串规整：大小写与空白容错。
	if a, ok := PolicyActionFromString(" Single_Shot "); !ok || a != PolicySingleShot {
		t.Errorf("规整解析失败: %q %v", a, ok)
	}
	if _, ok := PolicyActionFromString("nope"); ok {
		t.Error("未知动作不应通过")
	}
}
