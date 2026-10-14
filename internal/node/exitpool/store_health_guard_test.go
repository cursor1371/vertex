package exitpool

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// ---- 策略驱动的自动禁用（Issue #5 验收标准 + 策略可配置化基准）----

// TestExitProbe_RecordOnlyClassesNeverDisable 默认策略为「不禁用」的类别，即便连败远超
// 任何阈值也不得摘出节点：它们要么只记录、要么走冷却/降权。
// 其中 DNS 是评审第 3 条的硬约束（只记录、不进探测、不永久）；
// other 是"无法归因"的兜底，同样保守处置。
func TestExitProbe_RecordOnlyClassesNeverDisable(t *testing.T) {
	cases := []struct {
		name    string
		errStr  string
		wantCls nodestore.FailureClass
	}{
		{name: "DNS_NXDOMAIN只记录", errStr: "lookup ct.cloudflare.byoip.top: (exchange4: NXDOMAIN | exchange6: NXDOMAIN)", wantCls: nodestore.FailureDNS},
		{name: "DNS_no_such_host只记录", errStr: "dial tcp: lookup example.com: no such host", wantCls: nodestore.FailureDNS},
		{name: "429限流", errStr: "unexpected HTTP response status: 429", wantCls: nodestore.FailureRateLimit},
		{name: "503上游", errStr: "status: 503 upstream busy", wantCls: nodestore.FailureUpstream5xx},
		{name: "业务超时只降权", errStr: "context deadline exceeded", wantCls: nodestore.FailureTimeout},
		{name: "协议不支持", errStr: "unsupported: 未知协议", wantCls: nodestore.FailureUnsupported},
		{name: "签名缺失", errStr: "QUERY_SIGNATURE_NOT_FOUND", wantCls: nodestore.FailureProbeFlow},
		{name: "无关文本归其他", errStr: "some entirely unrelated message", wantCls: nodestore.FailureUnknown},
		{name: "其他兜底不禁用", errStr: "eof", wantCls: nodestore.FailureUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
			mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

			for i := 0; i < 5; i++ {
				if disabled := mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: tc.errStr}); disabled {
					t.Fatalf("%s 不得摘出节点（类别 %s）", tc.name, tc.wantCls)
				}
			}
			if n := mgr.LoadNodes()[0]; n.Disabled {
				t.Fatalf("%s 不得触发禁用（类别 %s）", tc.name, tc.wantCls)
			}
			h := mgr.LoadHealth()["u1"]
			if h.LastFailureClass != string(tc.wantCls) {
				t.Errorf("LastFailureClass=%q, want %q", h.LastFailureClass, tc.wantCls)
			}
			if h.ConsecutiveFailures != 5 {
				t.Errorf("连败计数应照常累加（只记录不摘出）, got %d", h.ConsecutiveFailures)
			}
			if mgr.HealthAutoDisabledURIs()["u1"] {
				t.Error("不禁用的类别不得进入自动探测名单")
			}
		})
	}
}

// TestExitProbe_DisableThenProbeRemovesOnFirstFailure 默认策略为「禁用后恢复探测」的类别：
// **首次**匹配失败即摘出可用池、来源 health、进入自动探测名单、且不再被选点。
//
// 这是评审明确要求先从"累计 N 次才首次移出"改回"首次即移出"的行为，属防回退断言；
// N 只作用于探测阶段（见 TestExitProbe_AutoProbeEscalatesToPermanent）。
func TestExitProbe_DisableThenProbeRemovesOnFirstFailure(t *testing.T) {
	cases := []struct {
		name     string
		errStr   string
		explicit nodestore.FailureClass
		wantCls  nodestore.FailureClass
	}{
		{name: "节点网络_拨号被拒", errStr: "dial tcp: connect: connection refused", wantCls: nodestore.FailureNodeNetwork},
		{name: "节点网络_wsarecv", errStr: "wsarecv: A connection attempt failed because the connected party did not properly respond", wantCls: nodestore.FailureNodeNetwork},
		{name: "节点网络_dial timeout", errStr: "dial timeout after 15s", wantCls: nodestore.FailureNodeNetwork},
		{name: "403权限拒绝", errStr: "unexpected HTTP response status: 403", wantCls: nodestore.FailurePermission},
		{name: "403_枚举形式", errStr: "status: PERMISSION_DENIED", wantCls: nodestore.FailurePermission},
		{name: "401认证失败", errStr: "unexpected HTTP response status: 401", wantCls: nodestore.FailureAuth},
		{name: "显式permission类别", errStr: "opaque error", explicit: nodestore.FailurePermission, wantCls: nodestore.FailurePermission},
		{name: "显式auth类别", errStr: "opaque error", explicit: nodestore.FailureAuth, wantCls: nodestore.FailureAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 用最"苛刻"的阈值（3）：首次移出不受阈值影响，仍应立刻摘出。
			mgr := NewManager(nil, nil, Hooks{}, func() int { return 3 })
			mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

			disabled := mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: tc.errStr, Class: tc.explicit})
			if !disabled {
				t.Fatalf("%s：首次失败即应摘出（阈值只管探测阶段）", tc.name)
			}
			n := mgr.LoadNodes()[0]
			if !n.Disabled {
				t.Fatal("节点应被移出可用池")
			}
			if n.DisableSource != nodestore.DisableSourceHealth {
				t.Errorf("来源=%q, want %q（该类策略为禁用后恢复探测）", n.DisableSource, nodestore.DisableSourceHealth)
			}
			if n.DisableReason == "" || n.DisabledAt == 0 {
				t.Error("禁用审计（原因/时间）应写入")
			}
			if n.DisableFailureClass != string(tc.wantCls) {
				t.Errorf("DisableFailureClass=%q, want %q", n.DisableFailureClass, tc.wantCls)
			}
			if !mgr.HealthAutoDisabledURIs()["u1"] {
				t.Error("应进入自动探测名单（可被探测成功恢复）")
			}
			if got := mgr.SelectForParallel(1, false); len(got) != 0 {
				t.Errorf("已摘出的节点不应再被选点, got %d", len(got))
			}
			if h := mgr.LoadHealth()["u1"]; h.LastFailureClass != string(tc.wantCls) {
				t.Errorf("失败类别记录=%q, want %q", h.LastFailureClass, tc.wantCls)
			}
		})
	}
}

// TestExitProbe_NetworkFailureKeepsAuditFields 摘出后审计字段完整，且原因写明策略与探测阈值。
func TestExitProbe_NetworkFailureKeepsAuditFields(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connect: connection refused"})

	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatal("首次网络类失败应摘出")
	}
	if n.DisableSource != nodestore.DisableSourceHealth {
		t.Errorf("DisableSource=%q, want %q", n.DisableSource, nodestore.DisableSourceHealth)
	}
	if n.DisableReason == "" {
		t.Error("禁用原因不应为空（面板要展示「为什么禁」）")
	}
	if !strings.Contains(n.DisableReason, nodestore.PolicyActionLabel(nodestore.PolicyDisableThenProbe)) {
		t.Errorf("原因应写明策略: %q", n.DisableReason)
	}
	if n.DisabledAt == 0 {
		t.Error("禁用时间不应为 0")
	}
	if n.DisableFailureClass != string(nodestore.FailureNodeNetwork) {
		t.Errorf("DisableFailureClass=%q, want %q", n.DisableFailureClass, nodestore.FailureNodeNetwork)
	}
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("健康禁用的节点应进入自动探测名单")
	}
	if !mgr.LoadHealth()["u1"].DisabledByHealth {
		t.Error("内存视图 DisabledByHealth 应与来源同步为 true")
	}
}

// TestExitProbe_AutoProbeEscalatesToPermanent 自动探测连续失败达阈值 → 转永久禁用，
// 此后不再进入自动探测名单，且后续失败不会改写来源（评审第 4 条）。
func TestExitProbe_AutoProbeEscalatesToPermanent(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 2 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	// 先由请求路径达成健康禁用。
	for i := 0; i < 2; i++ {
		mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout"})
	}
	if n := mgr.LoadNodes()[0]; n.DisableSource != nodestore.DisableSourceHealth {
		t.Fatalf("前置条件：应为健康禁用, got %q", n.DisableSource)
	}

	// 自动探测第 1 次失败：仍未达阈值。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout", Source: nodestore.ProbeSourceAutoProbe})
	if n := mgr.LoadNodes()[0]; n.DisableSource != nodestore.DisableSourceHealth {
		t.Fatalf("自动探测第 1 次失败不应转永久, got %q", n.DisableSource)
	}
	// 自动探测第 2 次失败：达阈值 → 永久禁用。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout", Source: nodestore.ProbeSourceAutoProbe})
	if n := mgr.LoadNodes()[0]; n.DisableSource != nodestore.DisableSourcePermanent {
		t.Fatalf("自动探测连续失败达阈值应转永久禁用, got %q", n.DisableSource)
	}
	if mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("永久禁用节点不应再进入自动探测名单（否则形成无限探测循环）")
	}
	if mgr.DisableSourceOf("u1") != nodestore.DisableSourcePermanent {
		t.Errorf("DisableSourceOf=%q, want permanent", mgr.DisableSourceOf("u1"))
	}

	// 后续失败（含请求路径）不得把永久降级回健康可恢复。
	for i := 0; i < 3; i++ {
		mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout"})
	}
	if n := mgr.LoadNodes()[0]; n.DisableSource != nodestore.DisableSourcePermanent {
		t.Fatalf("永久禁用不得被自动路径改写, got %q", n.DisableSource)
	}
}

// TestExitProbe_AutoProbeSuccessResetsStreak 自动探测成功必须清零连败并恢复节点，
// 否则"达阈值转永久"将没有复位途径。
func TestExitProbe_AutoProbeSuccessResetsStreak(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 3 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	for i := 0; i < 3; i++ {
		mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout"})
	}
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout", Source: nodestore.ProbeSourceAutoProbe})
	if h := mgr.LoadHealth()["u1"]; h.AutoProbeFailStreak != 1 {
		t.Fatalf("自动探测连败应为 1, got %d", h.AutoProbeFailStreak)
	}
	mgr.ReportProbe("u1", ProbeResult{OK: true, Ms: 120, Source: nodestore.ProbeSourceAutoProbe})

	n := mgr.LoadNodes()[0]
	if n.Disabled {
		t.Error("探测成功应自动解除健康触发禁用")
	}
	if n.DisableSource != nodestore.DisableSourceManual {
		t.Errorf("恢复后应清空禁用来源, got %q", n.DisableSource)
	}
	if n.DisableReason != "" || n.DisabledAt != 0 {
		t.Error("恢复后应清空禁用审计（残留的旧原因比没有更误导）")
	}
	h := mgr.LoadHealth()["u1"]
	if h.AutoProbeFailStreak != 0 || h.ConsecutiveFailures != 0 || h.DisabledByHealth {
		t.Errorf("恢复后应清零连败与标记, got %+v", h)
	}
}

// TestExitProbe_GlobalFaultGuardBlocksDisable 全局故障保护命中时暂停自动禁用：
// 这是"一次本地 DNS/链路故障不要把整池节点逐个禁掉"的兜底。
func TestExitProbe_GlobalFaultGuardBlocksDisable(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
	// 阈值 1（最激进）+ 保护开启（3 个不同节点 / 120 秒窗口）。
	mgr.SetGlobalFaultGuard(nodestore.NewGlobalFaultGuard(func() (bool, int, int) { return true, 120, 3 }))
	mgr.seedUnsafe([]Node{
		{RawURI: "u1", Name: "n1"}, //nolint:exhaustruct
		{RawURI: "u2", Name: "n2"}, //nolint:exhaustruct
		{RawURI: "u3", Name: "n3"}, //nolint:exhaustruct
		{RawURI: "u4", Name: "n4"}, //nolint:exhaustruct
	}, nil)

	// 前 2 个节点：还未触发全局故障判定，按阈值正常禁用。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	mgr.ReportProbe("u2", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	if !mgr.LoadNodes()[0].Disabled || !mgr.LoadNodes()[1].Disabled {
		t.Fatal("保护未触发前按阈值正常禁用")
	}

	// 第 3 个节点触发全局故障判定：本次起暂停禁用。
	mgr.ReportProbe("u3", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	if mgr.LoadNodes()[2].Disabled {
		t.Fatal("全局故障命中时不得禁用节点（整池同因失败通常是公共依赖故障）")
	}
	mgr.ReportProbe("u4", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	if mgr.LoadNodes()[3].Disabled {
		t.Fatal("全局故障持续期间不得禁用后续节点")
	}

	// 面板可见：命中期间要告诉管理员"这批失败可能不是节点的问题"。
	active := mgr.GlobalFaultActive()
	if _, ok := active[string(nodestore.FailureNodeNetwork)]; !ok {
		t.Errorf("GlobalFaultActive 应报告网络类活跃, got %v", active)
	}
}

// TestExitProbe_DisableIsIdempotent 幂等：同因反复失败不产生额外状态变更（禁用时间不变），
// 而失败原因变化时才刷新审计。
func TestExitProbe_DisableIsIdempotent(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	first := mgr.LoadNodes()[0]
	if !first.Disabled || first.DisabledAt == 0 {
		t.Fatalf("阈值 1 应在首次失败即禁用: %+v", first)
	}

	for i := 0; i < 5; i++ {
		mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	}
	again := mgr.LoadNodes()[0]
	if again.DisabledAt != first.DisabledAt {
		t.Errorf("同因反复失败不应重写禁用审计（幂等），DisabledAt %d → %d", first.DisabledAt, again.DisabledAt)
	}

	// 原因变化（错误文本不同）时允许刷新，保证面板看到的是最近一次的真实原因。
	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: i/o timeout"})
	changed := mgr.LoadNodes()[0]
	if changed.DisableReason == first.DisableReason {
		t.Error("失败原因变化时应刷新审计原因")
	}
}

// TestExitProbe_ManualAndTestSourcesNotAutoRecovered 人工禁用 / admin 测试禁用 /
// 协议不支持的禁用都不得被成功探测自动放开（自动策略不覆盖人工与测试设置）。
func TestExitProbe_ManualAndTestSourcesNotAutoRecovered(t *testing.T) {
	sources := []string{
		nodestore.DisableSourceAdminTest,
		nodestore.DisableSourceUnsupported,
		nodestore.DisableSourceManual,
	}
	for _, src := range sources {
		t.Run("source="+src, func(t *testing.T) {
			mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
			mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

			if src == nodestore.DisableSourceManual {
				mgr.BatchUpdateNodesDisabled([]string{"u1"}, true)
			} else {
				mgr.BatchDisableWithSource([]string{"u1"}, src, "测试原因", string(nodestore.FailureNodeNetwork))
			}
			n := mgr.LoadNodes()[0]
			if !n.Disabled || n.DisableSource != src {
				t.Fatalf("禁用与来源应写入: %+v", n)
			}
			// 人工禁用没有自动判定的原因，其余来源必须写明"为什么禁"。
			if src != nodestore.DisableSourceManual && n.DisableReason == "" {
				t.Errorf("来源 %q 应记录禁用原因", src)
			}
			if mgr.HealthAutoDisabledURIs()["u1"] {
				t.Error("非健康来源不应进入自动探测名单")
			}
			mgr.ReportProbe("u1", ProbeResult{OK: true, Ms: 80, Source: nodestore.ProbeSourceAutoProbe})
			if !mgr.LoadNodes()[0].Disabled {
				t.Errorf("来源 %q 的禁用不得被成功探测自动放开", src)
			}
		})
	}
}

// TestExitProbe_ReasonOf 禁用原因必须可读、稳定（幂等的前提）且反映当前策略。
func TestExitProbe_ReasonOf(t *testing.T) {
	info := nodestore.ClassifyFailure("dial tcp: connect: connection refused")
	r := ProbeResult{ErrStr: info.Detail, Target: "example.com"}
	cls := info.Class

	probePol := nodestore.FaultPolicy{Class: cls, Action: nodestore.PolicyDisableThenProbe, ProbeFailThreshold: 3}
	reason := disableReasonOf(probePol, info, r)
	if reason == "" {
		t.Fatal("原因不应为空")
	}
	// 原因必须稳定（不含实时连败次数），否则反复失败会反复重写库、幂等失效。
	if again := disableReasonOf(probePol, info, r); reason != again {
		t.Errorf("同策略同因的原因必须稳定:\n a=%q\n b=%q", reason, again)
	}
	// 策略与阈值必须体现在原因里，面板要能看出"为什么禁、什么时候会转永久"。
	if !strings.Contains(reason, nodestore.PolicyActionLabel(probePol.Action)) {
		t.Errorf("原因应写明策略: %q", reason)
	}
	if !strings.Contains(reason, strconv.Itoa(probePol.ProbeFailThreshold)) {
		t.Errorf("原因应写明探测失败阈值: %q", reason)
	}

	// 无限探测（N=0）必须写明"不因探测失败转永久"，否则管理员会以为迟早会转。
	unlimited := nodestore.FaultPolicy{Class: cls, Action: nodestore.PolicyDisableThenProbe, ProbeFailThreshold: 0}
	if got := disableReasonOf(unlimited, info, r); !strings.Contains(got, "无限探测") {
		t.Errorf("N=0 应写明无限探测: %q", got)
	}

	// 单次禁用不应出现任何探测阈值措辞（它压根不进探测）。
	shot := nodestore.FaultPolicy{Class: cls, Action: nodestore.PolicySingleShot}
	if got := disableReasonOf(shot, info, r); strings.Contains(got, "探测失败") {
		t.Errorf("单次禁用不应提及探测阈值: %q", got)
	}
}

// TestConcurrentReportProbe_SingleDisable 并发上报同一节点（模拟批量测速 + 请求路径 +
// 自动探测同时命中）：只应产生一次禁用状态变更，且不会 panic / 数据错乱。
// 本地用 -race 验证（CI 的 -race 因 Issue #3 仍非阻断）。
func TestConcurrentReportProbe_SingleDisable(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 3 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.ReportProbe("u1", ProbeResult{
				OK: false, ErrStr: "dial tcp: connection refused",
				Source: nodestore.ProbeSourceAdminBatch,
			})
		}()
	}
	wg.Wait()

	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatal("并发失败达阈值后应禁用")
	}
	if n.DisableSource != nodestore.DisableSourceHealth {
		t.Errorf("来源应为 health, got %q", n.DisableSource)
	}
	if n.DisabledAt == 0 {
		t.Error("禁用时间应写入")
	}
	h := mgr.LoadHealth()["u1"]
	if h.ConsecutiveFailures != 32 {
		t.Errorf("并发失败应全部记账（无丢失更新）, got %d", h.ConsecutiveFailures)
	}
}

// TestGlobalFaultGuard_WiredIntoSelection 保护器注入后，选点仍正常工作
// （守卫只在禁用决策上生效，不得影响选点）。
func TestGlobalFaultGuard_WiredIntoSelection(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
	mgr.SetGlobalFaultGuard(nodestore.NewGlobalFaultGuard(func() (bool, int, int) { return false, 120, 3 }))
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	mgr.ReportProbe("u1", ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	if !mgr.LoadNodes()[0].Disabled {
		t.Fatal("保护关闭时应按阈值禁用")
	}
	if len(mgr.GlobalFaultActive()) != 0 {
		t.Error("保护关闭时 GlobalFaultActive 应为空")
	}
	if got := mgr.SelectForParallel(1, false); len(got) != 0 {
		t.Errorf("禁用节点不应被选中, got %d", len(got))
	}
}

// TestReportProbe_MasksPersistedError 落库与面板展示的错误文本必须脱敏：
// LastTestError 会经 /api/admin/nodes 的 health 字段原样下发到面板，上游错误常带完整 URL
// （query 里可能有凭据），不脱敏等于把凭据写进库再展示。
func TestReportProbe_MasksPersistedError(t *testing.T) {
	mgr := NewManager(nil, nil, Hooks{}, func() int { return 1 })
	mgr.seedUnsafe([]Node{{RawURI: "u1", Name: "n1"}}, nil) //nolint:exhaustruct

	const secret = "SECRET_TOKEN_ABC"
	mgr.ReportProbe("u1", ProbeResult{
		OK:     false,
		ErrStr: `GET "https://example.com/anchor?key=` + secret + `": dial tcp: connection refused`,
	})

	h := mgr.LoadHealth()["u1"]
	if strings.Contains(h.LastTestError, secret) {
		t.Errorf("LastTestError 泄露凭据: %q", h.LastTestError)
	}
	if !strings.Contains(h.LastTestError, "<redacted>") {
		t.Errorf("凭据应被替换为 <redacted>: %q", h.LastTestError)
	}
	if !strings.Contains(h.LastTestError, "connection refused") {
		t.Errorf("错误类型应保留以便排查: %q", h.LastTestError)
	}
	// 禁用原因走同一条脱敏路径（分类器的 Detail）。
	n := mgr.LoadNodes()[0]
	if strings.Contains(n.DisableReason, secret) {
		t.Errorf("DisableReason 泄露凭据: %q", n.DisableReason)
	}
}
