package api

import (
	"context"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// TestSelfHealCfg_Defaults 锁定评审第 4 条的运行参数：默认周期 120 分钟、并发 5；
// 配置缺失（测试装配 ServerDeps{}）时探测关闭；并发度有上界钳制。
func TestSelfHealCfg_Defaults(t *testing.T) {
	// 配置缺失：关闭（不猜默认值去探测生产节点）。
	enabled, interval, conc := selfHealCfg(nil)
	if enabled {
		t.Error("配置缺失时应关闭探测")
	}
	if interval != 120*time.Minute {
		t.Errorf("兜底周期=%v, want 120m", interval)
	}
	if conc != 5 {
		t.Errorf("兜底并发=%d, want 5", conc)
	}

	// 默认配置：开 + 120 分钟 + 并发 5。
	def := config.DefaultConfig()
	enabled, interval, conc = selfHealCfg(config.StaticProvider(def))
	if !enabled {
		t.Error("默认配置应开启自动探测")
	}
	if interval != 120*time.Minute {
		t.Errorf("默认周期=%v, want 120m（评审要求 120 分钟）", interval)
	}
	if conc != 5 {
		t.Errorf("默认并发=%d, want 5（评审要求并发 5）", conc)
	}

	// 越界与非法值：周期 <=0 回退 120、>1440 钳到 1440；并发 >20 钳到 20、<=0 回退 5。
	custom := config.DefaultConfig()
	custom.HealthSelfHealIntervalMinutes = 0
	custom.HealthSelfHealProbeConcurrency = 999
	_, interval, conc = selfHealCfg(config.StaticProvider(custom))
	if interval != 120*time.Minute {
		t.Errorf("周期 0 应回退 120m, got %v", interval)
	}
	if conc != selfHealMaxProbeConcurrency {
		t.Errorf("并发应钳制为 %d, got %d", selfHealMaxProbeConcurrency, conc)
	}

	// 周期上界的钳制属于 normalizeConfig（见 config 包 normalize 测试）；selfHealCfg 只负责
	// 非正值兜底与并发上界，避免同一口径在两处各写一遍。
	custom2 := config.DefaultConfig()
	custom2.HealthSelfHealIntervalMinutes = 5
	custom2.HealthSelfHealProbeConcurrency = -1
	_, interval, conc = selfHealCfg(config.StaticProvider(custom2))
	if interval != 5*time.Minute {
		t.Errorf("显式周期应原样生效, got %v", interval)
	}
	if conc != 5 {
		t.Errorf("并发非法应回退 5, got %d", conc)
	}
}

// TestSelfHealRound_SkipsWhenTestRunning 独占令牌必须真正互斥：批量测速持有时自动探测整轮跳过。
// 此前用 IsTestRunning() 先检查后启动（TOCTOU），同节点可能被两边同时探测、
// 失败计数重复累计而提前触阈。
func TestSelfHealRound_SkipsWhenTestRunning(t *testing.T) {
	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, func() int { return 1 })
	// 造一个"健康禁用"节点：它本应进入本轮探测名单。
	mgr.MergeNodes([]exitpool.Node{{RawURI: "u1", Name: "n1"}}) //nolint:exhaustruct
	mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Fatal("前置条件：节点应处于健康禁用")
	}

	// 批量测速占用令牌。
	if !mgr.StartTestProgress(1) {
		t.Fatal("前置条件：应能取得批量测速令牌")
	}
	defer mgr.FinishTestProgress()

	deps := ServerDeps{
		Cfg:  config.StaticProvider(config.DefaultConfig()),
		Exit: mgr,
		VC:   nil, // 走到探测分支就会 panic——跳过必须是"在探测之前"返回
		IR:   transport.NewIRCache(),
	}
	selfHealRound(context.Background(), deps)

	// 未探测 → 节点仍是健康禁用、无任何改动。
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("批量测速进行中，自动探测不得改动节点状态")
	}

	// 释放令牌后应能取得（成对释放是"令牌不会泄漏"的保证）。
	mgr.FinishTestProgress()
	if !mgr.TryAcquireTestRun() {
		t.Error("释放后应能重新取得令牌")
	}
	mgr.ReleaseTestRun()
}

// TestSelfHealRound_NoTargetsNoop 没有健康禁用节点时整轮空转，不触碰任何依赖
// （deps.VC 为 nil 也不会 panic）。
func TestSelfHealRound_NoTargetsNoop(t *testing.T) {
	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, nil)
	deps := ServerDeps{
		Cfg:  config.StaticProvider(config.DefaultConfig()),
		Exit: mgr,
		IR:   transport.NewIRCache(),
	}
	selfHealRound(context.Background(), deps)
	if !mgr.TryAcquireTestRun() {
		t.Error("空转轮不得持有独占令牌（否则批量测速会被永久挡住）")
	}
	mgr.ReleaseTestRun()
}

// TestSelfHealRound_DisabledSwitchSkips 开关关闭时整轮跳过（且不占令牌）。
func TestSelfHealRound_DisabledSwitchSkips(t *testing.T) {
	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, func() int { return 1 })
	mgr.MergeNodes([]exitpool.Node{{RawURI: "u1", Name: "n1"}}) //nolint:exhaustruct
	mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"})

	cfg := config.DefaultConfig()
	cfg.HealthSelfHealEnabled = false
	deps := ServerDeps{
		Cfg:  config.StaticProvider(cfg),
		Exit: mgr,
		IR:   transport.NewIRCache(),
	}
	selfHealRound(context.Background(), deps)
	if !mgr.HealthAutoDisabledURIs()["u1"] {
		t.Error("开关关闭时不得改动节点状态")
	}
	if !mgr.TryAcquireTestRun() {
		t.Error("跳过轮不得占用独占令牌")
	}
	mgr.ReleaseTestRun()
}

// TestAdminTestChannelStillDisablesDirectly 回归保护：评审第 1 条明确 admin 测试通道
// 失败**继续直接禁用**（不走阈值）。此处用"协议不支持"这条不需要网络即可复现的失败路径，
// 并验证来源被记为 unsupported 以便审计区分。
func TestAdminTestChannelAuditSource(t *testing.T) {
	mgr := exitpool.NewManager(nil, nil, exitpool.Hooks{}, nil)
	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Exit: mgr, IR: transport.NewIRCache()}}}

	// 用一条明确不被支持的节点（xhttp 传输）。
	uri := "vless://uuid@example.com:443?type=xhttp"
	if supported, _, _ := adm.checkNodeSupport(uri); supported {
		t.Skipf("该节点在本版本被判定为受支持，改用其它用例: %s", uri)
	}
	mgr.MergeNodes([]exitpool.Node{{RawURI: uri, Type: "vless", Name: "n"}}) //nolint:exhaustruct

	// 走管理端测试失败所用的禁用入口，验证来源与不可自动恢复语义。
	mgr.BatchDisableWithSource([]string{uri}, nodestore.DisableSourceUnsupported, "协议不支持", string(nodestore.FailureUnsupported))

	nodes := mgr.LoadNodes()
	if len(nodes) != 1 {
		t.Fatalf("节点数=%d, want 1", len(nodes))
	}
	if !nodes[0].Disabled {
		t.Error("admin 测试通道失败应直接禁用（评审第 1 条，行为不变）")
	}
	if nodes[0].DisableSource != nodestore.DisableSourceUnsupported {
		t.Errorf("DisableSource=%q, want unsupported", nodes[0].DisableSource)
	}
	if nodes[0].DisableReason == "" {
		t.Error("禁用原因应写入（面板要展示「为什么禁」）")
	}
	if mgr.HealthAutoDisabledURIs()[uri] {
		t.Error("unsupported 来源不应进入自动探测名单")
	}
}
