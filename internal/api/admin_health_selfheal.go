package api

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
	"github.com/bsfdsagfadg/vertex/internal/node/importer"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// selfHealDefaults 是配置缺失（deps.Cfg == nil，常见于测试装配 ServerDeps{}）时的
// 兜底口径：探测关闭。
const (
	selfHealDefaultIntervalMin    = 120
	selfHealDefaultConcurrency    = 5
	selfHealMaxProbeConcurrency   = 20
	selfHealSingleProbeTimeoutSec = 15
)

// selfHealCfg 聚合一轮探测需要的运行参数：开关 + 周期分钟数 + 并发度。
// 每轮开始时重新读取，面板改值后下一轮即生效，无需重启。
func selfHealCfg(cfg config.ConfigProvider) (enabled bool, interval time.Duration, concurrency int) {
	if cfg == nil {
		return false, selfHealDefaultIntervalMin * time.Minute, selfHealDefaultConcurrency
	}
	minutes := cfg.HealthSelfHealIntervalMinutes()
	if minutes <= 0 {
		minutes = selfHealDefaultIntervalMin
	}
	conc := cfg.HealthSelfHealProbeConcurrency()
	if conc <= 0 {
		conc = selfHealDefaultConcurrency
	}
	if conc > selfHealMaxProbeConcurrency {
		conc = selfHealMaxProbeConcurrency
	}
	return cfg.HealthSelfHealEnabled(), time.Duration(minutes) * time.Minute, conc
}

// StartHealthSelfHeal 启动"健康触发禁用"节点的后台自动探测协程，返回 stop 函数。
//
// 背景：健康禁用节点被 SelectForParallel 第一行排除，请求路径与普通 test-all 都触达
// 不了它们——唯一恢复途径是自动探测（或管理员手动「批量测速」/重新启用）。本协程周期性
// 对 HealthAutoDisabledURIs() 探测（与 test-all 同口径：CreateSession + FetchTokenWithSession），
// 成功即经 ReportProbe 的既有恢复路径自动解除禁用。
//
// 升级语义：同一节点被自动探测**连续失败达 health_disable_threshold** 次后转永久禁用
// （nodestore.DisableSourcePermanent），此后不再被探测、只能人工启用——避免
// "探测失败 → 保持禁用 → 再探测"的无限循环，也让"持续不可恢复"有明确的终点。
//
// 周期、开关与并发度来自运行配置（health_selfheal_enabled / health_selfheal_interval_minutes /
// health_selfheal_probe_concurrency），每轮重新读取：等待期用可变 timer 而非固定 ticker，
// 改间隔后下一轮即生效；开关关闭只跳过轮次、协程保持存活，重新打开后自动恢复探测。
func StartHealthSelfHeal(deps ServerDeps) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// 先等一个周期再探测：进程刚启动时禁用集合刚从库回填，多为上次会话残留。
		_, interval, _ := selfHealCfg(deps.Cfg)
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				selfHealRound(ctx, deps)
				// 本轮结束后按当前配置重新等待：间隔/开关的修改从这里开始生效。
				_, interval, _ = selfHealCfg(deps.Cfg)
				timer.Reset(interval)
			}
		}
	}()
	return cancel
}

// selfHealRound 执行一轮自动探测：并发度来自配置（默认 5）。
//
// 与批量测速互斥由独占令牌（TryAcquireTestRun）保证，而非此前的"先检查 IsTestRunning
// 再启动"——后者在检查与启动之间存在窗口，同一节点可能被两边同时探测。
func selfHealRound(ctx context.Context, deps ServerDeps) {
	enabled, _, concurrency := selfHealCfg(deps.Cfg)
	if !enabled {
		return
	}
	if deps.Exit == nil || deps.VC == nil || deps.Tokens == nil || deps.IR == nil {
		return
	}
	autoDisabled := deps.Exit.HealthAutoDisabledURIs()
	if len(autoDisabled) == 0 {
		return
	}
	if !deps.Exit.TryAcquireTestRun() {
		log.Printf("[Health] [AutoProbe] 批量测速进行中，本轮跳过（待探测 %d 个）", len(autoDisabled))
		return
	}
	defer deps.Exit.ReleaseTestRun()

	uris := make([]string, 0, len(autoDisabled))
	for uri := range autoDisabled {
		uris = append(uris, uri)
	}
	log.Printf("[Health] [AutoProbe] 开始探测 %d 个健康触发禁用的节点（并发 %d）", len(uris), concurrency)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		sem       = make(chan struct{}, concurrency)
		recovered int
		promoted  int
	)
	for _, uri := range uris {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(uri string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			recoveredNow, promotedNow := selfHealProbeOne(ctx, deps, uri)
			mu.Lock()
			if recoveredNow {
				recovered++
			}
			if promotedNow {
				promoted++
			}
			mu.Unlock()
		}(uri)
	}
	wg.Wait()
	if recovered > 0 || promoted > 0 {
		log.Printf("[Health] [AutoProbe] 本轮结果：恢复 %d 个、转永久禁用 %d 个（共探测 %d 个）",
			recovered, promoted, len(uris))
	}
}

// selfHealProbeOne 探测单个节点，返回（本次是否恢复、本次是否转永久禁用）。
func selfHealProbeOne(ctx context.Context, deps ServerDeps, uri string) (recovered, promoted bool) {
	// capability 早检查（对齐 test-all）：协议不支持/解析失败的节点保持禁用并记录真实原因。
	// 这类失败不参与"永久禁用"升级——问题在解析器不在节点，判它不可恢复没有依据。
	if pn, perr := deps.IR.GetOrParse(uri); perr != nil || pn == nil || !pn.Supported {
		reason := "parse failed"
		if perr != nil {
			reason = "parse failed: " + perr.Error()
		} else if pn != nil {
			reason = "unsupported: " + pn.UnsupportedReason
		}
		deps.Exit.ReportProbe(uri, exitpool.ProbeResult{
			OK: false, ErrStr: nodestore.MaskSecrets(reason),
			Class: nodestore.FailureUnsupported, Source: nodestore.ProbeSourceUnsupported,
		})
		return false, false
	}

	start := time.Now()
	sess, err := deps.VC.Net().CreateSession(selfHealSingleProbeTimeoutSec, uri, "health-autoprobe")
	var testErr error
	if err == nil {
		_, testErr = deps.Tokens.FetchTokenWithSession(ctx, sess)
		sess.Close()
	} else {
		testErr = err
	}
	duration := float64(time.Since(start).Milliseconds())

	before := deps.Exit.DisableSourceOf(uri)
	deps.Exit.ReportProbe(uri, exitpool.ProbeResult{
		OK:     testErr == nil,
		Ms:     duration,
		ErrStr: importer.ErrToStr(testErr),
		Source: nodestore.ProbeSourceAutoProbe,
	})
	after := deps.Exit.DisableSourceOf(uri)

	switch {
	case testErr == nil && before != "" && after == "":
		log.Printf("[Health] [AutoProbe] 节点 %s 探测成功（%.0fms），已自动解除禁用", uri, duration)
		recovered = true
	case before == nodestore.DisableSourceHealth && after == nodestore.DisableSourcePermanent:
		log.Printf("[Health] [AutoProbe] 节点 %s 探测连续失败达阈值，已转永久禁用", uri)
		promoted = true
	}
	return recovered, promoted
}
