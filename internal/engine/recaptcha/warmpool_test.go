package recaptcha

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWarmCfg 是 warmPoolConfig 的最小假体，避免为单测实现整个 ConfigProvider。
// adaptive 零值为 false：既有用例构造时都不显式设置，等价于"自停关闭"的旧行为，
// 新行为由显式 adaptive: true 的用例覆盖。
type fakeWarmCfg struct {
	enabled   bool
	depth     int
	ttlSec    int
	budgetSec int
	adaptive  bool
	debug     bool
}

func (f fakeWarmCfg) RecaptchaTokenPrefetch() bool     { return f.enabled }
func (f fakeWarmCfg) RecaptchaPrefetchDepth() int      { return f.depth }
func (f fakeWarmCfg) RecaptchaPrefetchTTLSeconds() int { return f.ttlSec }
func (f fakeWarmCfg) RecaptchaPrefetchAdaptive() bool  { return f.adaptive }
func (f fakeWarmCfg) RecaptchaPrefetchBudgetSeconds() int {
	return f.budgetSec
}
func (f fakeWarmCfg) DebugMode() bool { return f.debug }

// countingFetch 是可计数、可注入错误的抓取桩。
type countingFetch struct {
	calls atomic.Int64
	err   error
}

func (c *countingFetch) fn(_ context.Context) (string, error) {
	n := c.calls.Add(1)
	if c.err != nil {
		return "", c.err
	}
	return fmt.Sprintf("tok-%d", n), nil
}

// gatedFetch 在放行前阻塞，用于在"无补货干扰"的前提下断言池的取出语义。
type gatedFetch struct {
	calls atomic.Int64
	gate  chan struct{}
}

func (g *gatedFetch) fn(ctx context.Context) (string, error) {
	n := g.calls.Add(1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return fmt.Sprintf("tok-%d", n), nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// TestWarmPool_DisabledNoBackgroundTraffic 未启用时既不发放 token 也不产生任何后台抓取。
func TestWarmPool_DisabledNoBackgroundTraffic(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: false, depth: 3, ttlSec: 90}, fetch.fn)
	defer p.Close()

	if _, ok := p.pop(); ok {
		t.Fatal("未启用时不应命中")
	}
	time.Sleep(80 * time.Millisecond)
	if n := fetch.calls.Load(); n != 0 {
		t.Fatalf("未启用时不应抓取, calls=%d", n)
	}
}

// TestWarmPool_IdleNoPrefetch 启用但无消费时不产生后台流量（消费驱动，而非定时预取）。
func TestWarmPool_IdleNoPrefetch(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90}, fetch.fn)
	defer p.Close()

	time.Sleep(80 * time.Millisecond)
	if n := fetch.calls.Load(); n != 0 {
		t.Fatalf("空闲时不应抓取, calls=%d", n)
	}
}

// TestWarmPool_ConsumptionDrivenRefillAndHit 首次未命中触发补货，补满后命中。
func TestWarmPool_ConsumptionDrivenRefillAndHit(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90}, fetch.fn)
	defer p.Close()

	if _, ok := p.pop(); ok {
		t.Fatal("空池首次 pop 不应命中")
	}
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "池应被补满 3")
	tok, ok := p.pop()
	if !ok || tok == "" {
		t.Fatalf("补满后应命中, tok=%q ok=%v", tok, ok)
	}
	if st := p.stats(); st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("计数异常: hits=%d misses=%d, want 1/1", st.Hits, st.Misses)
	}
}

// TestWarmPool_SingleUseNoDuplicate 同一 token 绝不重复发放，取尽后未命中。
func TestWarmPool_SingleUseNoDuplicate(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 4, ttlSec: 90}, g.fn)
	defer p.Close()
	// 直接播种并闸住补货，确保断言期间池内不会新增 token。
	for i := 0; i < 4; i++ {
		p.put(fmt.Sprintf("seed-%d", i))
	}

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		tok, ok := p.pop()
		if !ok {
			t.Fatalf("第 %d 次 pop 应命中", i+1)
		}
		if seen[tok] {
			t.Fatalf("token 重复发放: %q", tok)
		}
		seen[tok] = true
	}
	if _, ok := p.pop(); ok {
		t.Fatal("池取尽后不应命中")
	}
}

// TestWarmPool_ConcurrentPopDistinct 并发 pop 绝不重复发放同一 token。
func TestWarmPool_ConcurrentPopDistinct(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 8, ttlSec: 90}, g.fn)
	defer p.Close()
	for i := 0; i < 8; i++ {
		p.put(fmt.Sprintf("seed-%d", i))
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, ok := p.pop(); ok {
				mu.Lock()
				seen[tok]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != 8 {
		t.Fatalf("并发取出的不同 token 数 = %d, want 8 (seen=%v)", len(seen), seen)
	}
	for tok, n := range seen {
		if n != 1 {
			t.Fatalf("token %q 被发放 %d 次", tok, n)
		}
	}
}

// TestWarmPool_ExpiredDiscarded 超过 TTL 的 token 绝不发放，并计入过期计数。
func TestWarmPool_ExpiredDiscarded(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 2, ttlSec: 90}, g.fn)
	defer p.Close()

	p.mu.Lock()
	p.tokens = append(p.tokens, warmToken{token: "stale", fetchedAt: time.Now().Add(-2 * time.Minute)})
	p.mu.Unlock()

	if _, ok := p.pop(); ok {
		t.Fatal("过期 token 不应发放")
	}
	if st := p.stats(); st.Expired != 1 {
		t.Fatalf("过期计数 = %d, want 1", st.Expired)
	}
}

// TestWarmPool_FetchFailureBackoff 抓取持续失败时按退避重试，不空转打 Google。
func TestWarmPool_FetchFailureBackoff(t *testing.T) {
	fetch := &countingFetch{err: errors.New("boom")}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 5, ttlSec: 90}, fetch.fn)
	defer p.Close()

	p.pop() // 触发补货
	waitFor(t, 3*time.Second, func() bool { return fetch.calls.Load() > 0 }, "应至少尝试一次抓取")
	time.Sleep(250 * time.Millisecond)
	if n := fetch.calls.Load(); n > int64(warmPoolRefillConcurrency) {
		t.Fatalf("失败后退避不足: calls=%d, 单轮上限 %d", n, warmPoolRefillConcurrency)
	}
}

// TestWarmPool_CloseStopsRefill 关闭后停止补货、可重复调用，且 pop 恒为未命中。
func TestWarmPool_CloseStopsRefill(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90}, fetch.fn)

	p.pop() // 触发补货
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "池应补满")

	p.Close()
	p.Close() // 幂等
	if _, ok := p.pop(); ok {
		t.Fatal("关闭后不应命中")
	}
	before := fetch.calls.Load()
	time.Sleep(150 * time.Millisecond)
	if after := fetch.calls.Load(); after != before {
		t.Fatalf("关闭后仍在抓取: %d -> %d", before, after)
	}
}

// TestWarmPool_PartialFailureKeepsFilling 部分抓取失败不应整体退避：
// 否则个别节点抖动会让池长期填不满（实测表现为"池内恒 0 + 失败计数增长"）。
func TestWarmPool_PartialFailureKeepsFilling(t *testing.T) {
	var calls atomic.Int64
	fetch := func(_ context.Context) (string, error) {
		n := calls.Add(1)
		if n%2 == 0 {
			return "", errors.New("boom")
		}
		return fmt.Sprintf("tok-%d", n), nil
	}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90}, fetch)
	defer p.Close()

	p.pop() // 触发补货
	waitFor(t, 5*time.Second, func() bool { return p.size() >= 3 }, "部分失败也应继续补满")
}

// TestWarmPool_ConfigFallback 配置非法（深度/TTL 为 0）时回退默认值。
func TestWarmPool_ConfigFallback(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 0, ttlSec: 0}, fetch.fn)
	defer p.Close()

	if got := p.depth(); got != defaultPrefetchDepth {
		t.Errorf("depth=%d, want %d", got, defaultPrefetchDepth)
	}
	if got := p.ttl(); got != defaultPrefetchTTL {
		t.Errorf("ttl=%v, want %v", got, defaultPrefetchTTL)
	}
}

// TestWarmPool_FetchTimeoutBudget 单轮预取预算可配：未配置时回退 15s，配置后按配置生效。
func TestWarmPool_FetchTimeoutBudget(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		want   time.Duration
	}{
		{"未配置_回退15s", 0, 15 * time.Second},
		{"配置45s_生效", 45, 45 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &gatedFetch{gate: make(chan struct{})}
			p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true, budgetSec: tc.budget}, g.fn)
			defer p.Close()
			if got := p.fetchTimeout(); got != tc.want {
				t.Fatalf("fetchTimeout=%v, want %v", got, tc.want)
			}
		})
	}

	t.Run("cfg缺失_回退默认", func(t *testing.T) {
		p := &warmPool{} //nolint:exhaustruct
		if got := p.fetchTimeout(); got != defaultWarmFetchBudget {
			t.Fatalf("fetchTimeout=%v, want %v", got, defaultWarmFetchBudget)
		}
	})
}

// seedStaleFullPool 直接播种"已补满但全部过期"的一池 token，避免为等待 TTL 而 sleep。
func seedStaleFullPool(p *warmPool, depth int, lastReqID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < depth; i++ {
		p.tokens = append(p.tokens, warmToken{
			token:     fmt.Sprintf("stale-%d", i),
			fetchedAt: time.Now().Add(-2 * time.Minute),
		})
	}
	p.lastReqID = lastReqID
}

// assertNoFetch 断言在给定窗口内没有任何后台抓取（自停的核心收益：零后台流量）。
func assertNoFetch(t *testing.T, fetch *countingFetch, msg string) {
	t.Helper()
	time.Sleep(120 * time.Millisecond)
	if n := fetch.calls.Load(); n != 0 {
		t.Fatalf("%s: 不应产生抓取, calls=%d", msg, n)
	}
}

// TestWarmPool_FullPoolExpirySuspendsRefill 池满后出现过期 token 即暂停补货。
// 这是稀疏流量的稳态：请求间隔长于 TTL，每来一次请求都会白抓一池 token。
func TestWarmPool_FullPoolExpirySuspendsRefill(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, fetch.fn)
	defer p.Close()
	seedStaleFullPool(p, 3, "")

	if _, ok := p.pop(); ok {
		t.Fatal("过期 token 不应发放")
	}
	st := p.stats()
	if !st.Paused || st.Pauses != 1 {
		t.Fatalf("池满后过期应置自停锁存: paused=%v pauses=%d", st.Paused, st.Pauses)
	}
	if st.Expired != 3 {
		t.Fatalf("过期计数 = %d, want 3", st.Expired)
	}
	assertNoFetch(t, fetch, "自停后")
}

// TestWarmPool_PartialExpirySuspendsButStillServesFresh 池满期间只要出现过期 token 即自停
// （含部分过期），但池内仍然新鲜的 token 照常发放——自停只停补货，不停供给。
func TestWarmPool_PartialExpirySuspendsButStillServesFresh(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, fetch.fn)
	defer p.Close()

	p.mu.Lock()
	p.tokens = append(p.tokens,
		warmToken{token: "stale", fetchedAt: time.Now().Add(-2 * time.Minute)},
		warmToken{token: "fresh-1", fetchedAt: time.Now()},
		warmToken{token: "fresh-2", fetchedAt: time.Now()})
	p.mu.Unlock()

	tok, ok := p.pop()
	if !ok || tok != "fresh-1" {
		t.Fatalf("新鲜 token 仍应发放, tok=%q ok=%v", tok, ok)
	}
	if !p.stats().Paused {
		t.Fatal("池满期间出现任意过期 token 即应自停")
	}
	assertNoFetch(t, fetch, "部分过期自停后")
}

// TestWarmPool_NewRequestResumesImmediately 新请求一到就立刻恢复补货，不看请求间隔。
// 请求是并发竞速的：一次请求会拉起 N 个候选、先后取走 N 个 token，补出来的 token 有机会
// 被本次请求后续的候选吃掉，所以不能按"间隔 < TTL"之类的条件延后恢复（那会白白错过这份
// 收益），哪怕上一次请求已是几分钟前。
func TestWarmPool_NewRequestResumesImmediately(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, fetch.fn)
	defer p.Close()
	seedStaleFullPool(p, 3, "req-1")

	p.pop() // prune 掉整池过期 token → 自停
	if !p.stats().Paused {
		t.Fatal("前置条件：应已自停")
	}
	assertNoFetch(t, fetch, "自停且无新请求时")

	p.NoteRequest("req-2") // 新请求到达（与上一次请求相隔任意久）
	if p.stats().Paused {
		t.Fatal("新请求到达应立即解除自停")
	}
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "解除自停后应立刻补满")
}

// TestWarmPool_SameRequestDoesNotResume 同一请求的重复通知（StreamChatCollect 会嵌套
// StreamChat）不算"新请求"，不得解除自停：否则锁存会被请求自己当场解除。
func TestWarmPool_SameRequestDoesNotResume(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, fetch.fn)
	defer p.Close()
	seedStaleFullPool(p, 3, "req-1")

	p.pop()
	if !p.stats().Paused {
		t.Fatal("前置条件：应已自停")
	}

	p.NoteRequest("req-1") // 同一请求的重复通知
	if !p.stats().Paused {
		t.Fatal("同一请求的重复通知不应解除自停")
	}
	assertNoFetch(t, fetch, "同一请求重复通知后")
}

// TestWarmPool_AdaptiveDisabledKeepsLegacyRefill 关闭自停开关时保持旧行为：
// 池满过期后照样补满（供回退对比）。
func TestWarmPool_AdaptiveDisabledKeepsLegacyRefill(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: false}, fetch.fn)
	defer p.Close()
	seedStaleFullPool(p, 3, "")

	p.pop()
	if p.stats().Paused {
		t.Fatal("关闭自停开关不应置锁存")
	}
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "关闭自停后应照旧补满")
}

// TestWarmPool_AdaptiveFreshTokensStillRefill 开启自停但 token 新鲜时不得误停：
// 消费驱动的正常补货与命中完全不受影响。
func TestWarmPool_AdaptiveFreshTokensStillRefill(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, fetch.fn)
	defer p.Close()

	p.NoteRequest("req-1")
	if _, ok := p.pop(); ok {
		t.Fatal("空池首次 pop 不应命中")
	}
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "应正常补满")
	if tok, ok := p.pop(); !ok || tok == "" {
		t.Fatalf("补满后应命中, tok=%q ok=%v", tok, ok)
	}
	if p.stats().Paused {
		t.Fatal("token 新鲜时不应自停")
	}
}

// TestWarmPool_ZeroOutputRoundsSuspend 连续多轮补货零产出即自停：池恒为空时（候选节点
// 已被健康判定禁空、或抓取预算始终不够）退避重试会无限进行，白占会话、白打上游、刷日志。
func TestWarmPool_ZeroOutputRoundsSuspend(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, g.fn)
	defer p.Close()

	for i := 1; i < warmPoolZeroOutputPauseRounds; i++ {
		if p.noteRefillOutcome(false) {
			t.Fatalf("第 %d 轮零产出不应暂停", i)
		}
	}
	if !p.noteRefillOutcome(false) {
		t.Fatalf("连续 %d 轮零产出应暂停补货", warmPoolZeroOutputPauseRounds)
	}
	if st := p.stats(); !st.Paused || st.Pauses != 1 {
		t.Fatalf("应置自停锁存: %+v", st)
	}
	if p.shouldRefill() {
		t.Fatal("自停后不应再补货")
	}
}

// TestWarmPool_ZeroOutputSuccessResets 每轮都有产出时不得暂停（偶发失败不该累积成自停）。
func TestWarmPool_ZeroOutputSuccessResets(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, g.fn)
	defer p.Close()

	for i := 0; i < warmPoolZeroOutputPauseRounds+2; i++ {
		p.noteRefillOutcome(false)
		p.noteRefillOutcome(true) // 成功一轮即清零
	}
	if p.stats().Paused {
		t.Fatal("每轮都有产出时不应暂停")
	}
}

// TestWarmPool_ZeroOutputSuspendResumesOnRequest 零产出触发的自停与过期触发的自停共用
// 同一锁存，同样由下一个连接请求解除。
func TestWarmPool_ZeroOutputSuspendResumesOnRequest(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, fetch.fn)
	defer p.Close()

	for i := 0; i < warmPoolZeroOutputPauseRounds; i++ {
		p.noteRefillOutcome(false)
	}
	if !p.stats().Paused {
		t.Fatal("前置条件：应已自停")
	}

	p.pop() // 新请求的候选取 token（池空 → 未命中）
	p.NoteRequest("req-1")
	if p.stats().Paused {
		t.Fatal("新请求到达应解除自停")
	}
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "解除自停后应补满")
}

// TestWarmPool_ShouldRefillGate 锁定补货门控的"单次加锁快照"语义：自停判定优先于
// "池未满且近期有消费"。若把 suspended 判断放到锁外，并发 pop 的自停判定会被插进
// 判定之间，刚自停的池仍会补一整轮（实测症状：自停后仍有后台抓取）。
func TestWarmPool_ShouldRefillGate(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 90, adaptive: true}, g.fn)
	defer p.Close()

	if p.shouldRefill() {
		t.Fatal("无消费时不应补货")
	}

	p.pop() // 记录消费；补货被闸住，池内状态完全由本用例控制
	if !p.shouldRefill() {
		t.Fatal("近期有消费且池未满时应补货")
	}

	p.mu.Lock()
	for i := 0; i < 3; i++ {
		p.tokens = append(p.tokens, warmToken{token: fmt.Sprintf("t-%d", i), fetchedAt: time.Now()})
	}
	p.mu.Unlock()
	if p.shouldRefill() {
		t.Fatal("池满时不应补货")
	}

	p.mu.Lock()
	p.tokens = p.tokens[:1] // 池未满 + 近期有消费
	p.mu.Unlock()
	if !p.shouldRefill() {
		t.Fatal("前置条件：池未满且有消费应可补货")
	}
	p.suspended.Store(true)
	if p.shouldRefill() {
		t.Fatal("自停时应拒绝补货：自停判定优先于池未满/近期消费")
	}
}

// TestWarmPool_StatsSizeCountsUsableOnly 展示用的池内数量必须是"可用（未超龄）"数：
// 超龄 token 虽仍在切片里，但绝不会被发放；否则面板会出现"池内 5/5 + 已暂停补货"的矛盾展示。
// 这里用未启用的池把内容冻住（禁用态不设定时器），才能在同一时刻断言两种口径。
func TestWarmPool_StatsSizeCountsUsableOnly(t *testing.T) {
	g := &gatedFetch{gate: make(chan struct{})}
	p := newWarmPool(fakeWarmCfg{enabled: false, depth: 3, ttlSec: 90, adaptive: true}, g.fn)
	defer p.Close()

	p.mu.Lock()
	p.tokens = append(p.tokens,
		warmToken{token: "stale", fetchedAt: time.Now().Add(-2 * time.Minute)},
		warmToken{token: "fresh", fetchedAt: time.Now()})
	p.mu.Unlock()

	if got := p.stats().Size; got != 1 {
		t.Fatalf("池内可用数 = %d, want 1（超龄项不应计入）", got)
	}
	if got := p.size(); got != 2 {
		t.Fatalf("切片长度 = %d, want 2（补货缺额仍按存储长度计算）", got)
	}
}

// TestWarmPool_IdleExpirySuspendsWithoutRequest 过期必须在空闲期就地发现：不依赖任何请求
// 触发 prune，否则面板会一直显示池满（实为一池全过期），暂停状态要拖到第一个请求才可见。
func TestWarmPool_IdleExpirySuspendsWithoutRequest(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 1, adaptive: true}, fetch.fn)
	defer p.Close()
	seedStaleFullPool(p, 3, "")

	// 无任何 pop / NoteRequest：仅靠到期定时器就应置自停并清掉过期项。
	waitFor(t, 3*time.Second, func() bool { return p.stats().Paused }, "空闲期过期应自动置自停")
	if got := p.stats().Size; got != 0 {
		t.Fatalf("过期项应被就地清掉, 可用数 = %d", got)
	}
	if st := p.stats(); st.Pauses != 1 || st.Expired != 3 {
		t.Fatalf("计数异常: pauses=%d expired=%d, want 1/3", st.Pauses, st.Expired)
	}
	assertNoFetch(t, fetch, "空闲期自停")
}

// TestWarmPool_FirstRequestAfterIdleExpiryRefills 回归"第一个新请求就应重新开始补货"：
// 自停必须已在空闲期置好（见上一条），这样第一个请求的 NoteRequest 才能解除它，
// 由该请求自己的候选消费补出来的 token；否则要拖到第二个请求才恢复。
func TestWarmPool_FirstRequestAfterIdleExpiryRefills(t *testing.T) {
	fetch := &countingFetch{}
	p := newWarmPool(fakeWarmCfg{enabled: true, depth: 3, ttlSec: 1, adaptive: true}, fetch.fn)
	defer p.Close()
	seedStaleFullPool(p, 3, "")

	waitFor(t, 3*time.Second, func() bool { return p.stats().Paused }, "空闲期过期应自动置自停")

	// 第一个新请求：到达即解除自停，随后其候选取 token（池已空 → 未命中）唤醒补货。
	p.NoteRequest("req-1")
	if p.stats().Paused {
		t.Fatal("第一个新请求到达即应解除自停")
	}
	if _, ok := p.pop(); ok {
		t.Fatal("过期项已清空，首次 pop 不应命中")
	}
	waitFor(t, 3*time.Second, func() bool { return p.size() >= 3 }, "第一个新请求就应重新补满")
}
