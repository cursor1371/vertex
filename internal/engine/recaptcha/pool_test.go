package recaptcha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

// TestTokenPoolNoCache 验证 30s 全局缓存已移除：
// 每次顺序调用 GetTokenShared 都必须发起新的抓取，返回全新 token。
func TestTokenPoolNoCache(t *testing.T) {
	var calls int32
	p := &TokenPool{fetch: func(_ string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		return fmt.Sprintf("tok-%d", n), nil
	}}

	// 第一次调用 -> fetch 1
	tok1, err := p.GetTokenShared(context.Background())
	if err != nil || tok1 != "tok-1" {
		t.Fatalf("Expected tok-1, got tok=%q err=%v", tok1, err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("Expected 1 fetch call, got %d", calls)
	}

	// 第二次调用必须重新抓取（无缓存），返回 tok-2
	tok2, err := p.GetTokenShared(context.Background())
	if err != nil || tok2 != "tok-2" {
		t.Fatalf("Expected fresh tok-2, got tok=%q err=%v", tok2, err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("Expected 2 fetch calls (no cache), got %d", calls)
	}

	tok3, err := p.GetTokenShared(context.Background())
	if err != nil || tok3 != "tok-3" {
		t.Fatalf("Expected tok-3, got tok=%q err=%v", tok3, err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("Expected 3 fetch calls, got %d", calls)
	}
}

// TestTokenPoolConcurrentFresh 验证移除 singleflight 后，并发调用各自抓取独立 token：
// reCAPTCHA token 是单次的，每个并发请求必须持有互不相同的 token。
func TestTokenPoolConcurrentFresh(t *testing.T) {
	var calls int32
	p := &TokenPool{fetch: func(_ string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		return fmt.Sprintf("tok-%d", n), nil
	}}

	var wg sync.WaitGroup
	toks := make([]string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tok, err := p.GetTokenShared(context.Background())
			if err == nil {
				toks[idx] = tok
			}
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt32(&calls) != 10 {
		t.Fatalf("Expected 10 independent fetch calls, got %d", calls)
	}
	seen := make(map[string]bool)
	for i, tok := range toks {
		if tok == "" {
			t.Errorf("Goroutine %d got empty token", i)
			continue
		}
		if seen[tok] {
			t.Errorf("Goroutine %d got duplicated token %q", i, tok)
		}
		seen[tok] = true
	}
}

// TestGetTokenShared_CancelledCtxSentinel 验证入口哨兵：
// 已取消的 ctx 直接拒绝，不发起任何抓取动作（含 custom fetch 桩路径）。
func TestGetTokenShared_CancelledCtxSentinel(t *testing.T) {
	var calls int32
	p := &TokenPool{fetch: func(_ string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "tok", nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tok, err := p.GetTokenShared(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Expected context.Canceled, got tok=%q err=%v", tok, err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("Cancelled request must not trigger fetch, got %d calls", calls)
	}
}

// TestFallbackPollWidth 验证 rT 兜底轮询宽度跟随并发规格（parallel_pool_size），
// cfg 缺失或规格非法时退回历史固定值 5。
func TestFallbackPollWidth(t *testing.T) {
	cases := []struct {
		name string
		size int
		want int
	}{
		{"规格合法_封顶5", 7, 5},
		{"规格为上限20_封顶5", 20, 5},
		{"规格最小1", 1, 1},
		{"规格非法0_回退5", 0, 5},
		{"规格负数_回退5", -3, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.ParallelPoolSize = tc.size
			if got := fallbackPollWidth(config.StaticProvider(cfg)); got != tc.want {
				t.Errorf("fallbackPollWidth(size=%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}

	t.Run("cfg缺失_回退5", func(t *testing.T) {
		if got := fallbackPollWidth(nil); got != 5 {
			t.Errorf("fallbackPollWidth(nil) = %d, want 5", got)
		}
	})
}

// TestTokenFetchPlanFor 抓取路径计划的配置推导：硬开关「禁止直连抓 rT」优先于
// 「优先前置/直连」（开启即入口/直连整条路径不尝试）；两者都关时仍保留兜底直连。
func TestTokenFetchPlanFor(t *testing.T) {
	cases := []struct {
		name      string
		tryEntry  bool
		noDirect  bool
		wantFirst bool
		wantAllow bool
	}{
		{"默认_优先入口且允许兜底", true, false, true, true},
		{"关闭优先_仍允许兜底直连", false, false, false, true},
		{"禁止直连_连优先入口也不走", true, true, false, false},
		{"禁止直连且关闭优先_只走候选", false, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.RecaptchaTryEntryOrDirect = tc.tryEntry
			cfg.RecaptchaNoDirectFetch = tc.noDirect
			plan := tokenFetchPlanFor(config.StaticProvider(cfg))
			if plan.tryEntryFirst != tc.wantFirst || plan.allowEntry != tc.wantAllow {
				t.Fatalf("plan=%+v, want tryEntryFirst=%v allowEntry=%v", plan, tc.wantFirst, tc.wantAllow)
			}
		})
	}

	t.Run("cfg缺失_保持既有行为", func(t *testing.T) {
		plan := tokenFetchPlanFor(nil)
		if !plan.tryEntryFirst || !plan.allowEntry {
			t.Fatalf("plan=%+v, want 优先入口且允许兜底", plan)
		}
	})
}

// tokenFetchReport 是一次上报到节点池的 rT 抓取结果。
type tokenFetchReport struct {
	uri     string
	outcome exitpool.TokenFetchOutcome
	ms      int64
	errStr  string
}

// fakeNodeSource 记录上报的 rT 抓取结果与 InFlight 占用，用于断言归因规则（不参与选点）。
// name 非空时作为 NodeName 的返回值（记录流的节点展示名断言用）。
type fakeNodeSource struct {
	reports []tokenFetchReport
	name    string
	inc     []string
	dec     []string
	ranks   map[string]int
	nodes   []exitpool.Node
}

func (f *fakeNodeSource) SelectForParallel(k int, _ bool) []exitpool.Node { return f.selectNodes(k) }
func (f *fakeNodeSource) SelectForTokenFetch(k int, _ bool) []exitpool.Node {
	return f.selectNodes(k)
}

// selectNodes 返回 nodes 中前 k 个（模拟"按排序取前 k 个"），供分批改取断言使用。
func (f *fakeNodeSource) selectNodes(k int) []exitpool.Node {
	if k <= 0 || len(f.nodes) == 0 {
		return nil
	}
	if k > len(f.nodes) {
		k = len(f.nodes)
	}
	out := make([]exitpool.Node, 0, k)
	return append(out, f.nodes[:k]...)
}
func (f *fakeNodeSource) NodeName(string) string        { return f.name }
func (f *fakeNodeSource) IncInFlight(uri string)        { f.inc = append(f.inc, uri) }
func (f *fakeNodeSource) DecInFlight(uri string)        { f.dec = append(f.dec, uri) }
func (f *fakeNodeSource) TokenFetchRank(uri string) int { return f.ranks[uri] }
func (f *fakeNodeSource) RecordTokenFetch(uri string, outcome exitpool.TokenFetchOutcome, ms int64, errStr string) {
	f.reports = append(f.reports, tokenFetchReport{uri: uri, outcome: outcome, ms: ms, errStr: errStr})
}

// TestTokenPool_WithInFlight rT 抓取必须占用/释放节点在途名额且严格配对
// （DecInFlight 不做下界钳制，漏减会把该节点算成最空闲而被优先选中）；
// 入口/直连（uri 为空）与未装配节点池时不记账。
func TestTokenPool_WithInFlight(t *testing.T) {
	t.Run("成功与失败路径都配对", func(t *testing.T) {
		src := &fakeNodeSource{}
		p := &TokenPool{source: src} //nolint:exhaustruct

		if tok, err := p.withInFlight("vless://a@h:443#n1", func() (string, error) {
			return "tok", nil
		}); err != nil || tok != "tok" {
			t.Fatalf("透传返回值异常: tok=%q err=%v", tok, err)
		}
		wantErr := errors.New("boom")
		if _, err := p.withInFlight("vless://a@h:443#n1", func() (string, error) {
			return "", wantErr
		}); !errors.Is(err, wantErr) {
			t.Fatalf("应原样透传错误, got %v", err)
		}

		if len(src.inc) != 2 || len(src.dec) != 2 {
			t.Fatalf("inc/dec 应各 2 次且配对: inc=%v dec=%v", src.inc, src.dec)
		}
		for i := range src.inc {
			if src.inc[i] != src.dec[i] {
				t.Fatalf("第 %d 对不匹配: inc=%s dec=%s", i, src.inc[i], src.dec[i])
			}
		}
	})

	t.Run("空uri不记账", func(t *testing.T) {
		src := &fakeNodeSource{}
		p := &TokenPool{source: src} //nolint:exhaustruct
		if _, err := p.withInFlight("", func() (string, error) { return "tok", nil }); err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(src.inc) != 0 || len(src.dec) != 0 {
			t.Fatalf("入口/直连不应记账: inc=%v dec=%v", src.inc, src.dec)
		}
	})

	t.Run("未装配节点池不panic", func(t *testing.T) {
		p := &TokenPool{} //nolint:exhaustruct
		if _, err := p.withInFlight("vless://a@h:443#n1", func() (string, error) { return "tok", nil }); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestTokenPool_LiveBudget 请求路径单轮预算可配：未配置时回退 15s，配置后按配置生效。
func TestTokenPool_LiveBudget(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RecaptchaLiveBudgetSec = 45
	p := &TokenPool{cfg: config.StaticProvider(cfg)} //nolint:exhaustruct
	if got := p.liveBudget(); got != 45*time.Second {
		t.Fatalf("liveBudget=%v, want 45s", got)
	}

	def := config.DefaultConfig()
	p2 := &TokenPool{cfg: config.StaticProvider(def)} //nolint:exhaustruct
	if got := p2.liveBudget(); got != defaultLiveFetchBudget {
		t.Fatalf("默认 liveBudget=%v, want %v", got, defaultLiveFetchBudget)
	}

	p3 := &TokenPool{} //nolint:exhaustruct
	if got := p3.liveBudget(); got != defaultLiveFetchBudget {
		t.Fatalf("cfg 缺失时 liveBudget=%v, want %v", got, defaultLiveFetchBudget)
	}
}

// TestReportTokenFetchAttribution rT 抓取结果的归因规则：
//   - 成功 → TokenFetchOK；
//   - 节点级失败（连接被拒等）→ TokenFetchFailed；
//   - 挂住型（tokenHangError：预算到点且一个尝试都没跑完）→ TokenFetchHang；
//   - 被取消（竞速其它候选胜出 / 请求中断）→ 不上报（与节点无关）；
//   - 预算到点但尝试有完成（只是慢）→ 不上报（预算问题，避免成批误杀好节点）；
//   - 空 uri（入口/直连）→ 不上报，与 applyNodeFailure 的规则一致。
func TestReportTokenFetchAttribution(t *testing.T) {
	src := &fakeNodeSource{}
	p := &TokenPool{source: src} //nolint:exhaustruct

	const nodeURI = "vless://a@h:443#n1"
	p.reportTokenFetch(context.Background(), nodeURI, true, 12, nil)
	p.reportTokenFetch(context.Background(), nodeURI, false, 34, errors.New("dial tcp: connection refused"))
	// 挂住型：预算到点且未完成任何尝试
	p.reportTokenFetch(context.Background(), "vless://a@h:443#n2", false, 15000,
		&tokenHangError{node: "n2", err: context.DeadlineExceeded})
	// 纯预算到点（尝试有完成）：不上报
	p.reportTokenFetch(context.Background(), "vless://a@h:443#n3", false, 15000, context.DeadlineExceeded)
	// 被取消：不上报
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	p.reportTokenFetch(cancelled, "vless://a@h:443#n4", false, 0, errors.New("dial tcp: connection refused"))
	// 空 uri：不上报
	p.reportTokenFetch(context.Background(), "", false, 0, errors.New("boom"))

	if len(src.reports) != 4 {
		t.Fatalf("应上报 4 条（成功 + 节点级失败 + 挂住 + 慢到超时），实际 %d 条: %+v",
			len(src.reports), src.reports)
	}
	if src.reports[0].outcome != exitpool.TokenFetchOK || src.reports[0].uri != nodeURI {
		t.Fatalf("成功上报异常: %+v", src.reports[0])
	}
	if src.reports[1].outcome != exitpool.TokenFetchFailed || !strings.Contains(src.reports[1].errStr, "connection refused") {
		t.Fatalf("节点级失败上报应携带底层原因: %+v", src.reports[1])
	}
	if src.reports[2].outcome != exitpool.TokenFetchHang {
		t.Fatalf("挂住型应上报 TokenFetchHang: %+v", src.reports[2])
	}
	// 慢到超时：记入 rT 健康度（降权），但不按挂住升级冷却。
	if src.reports[3].outcome != exitpool.TokenFetchSlow {
		t.Fatalf("慢到超时应上报 TokenFetchSlow: %+v", src.reports[3])
	}
}

// TestReportTokenFetchNilSource 未装配节点池（测试用 NewTokenPoolCustom）时不得 panic。
func TestReportTokenFetchNilSource(_ *testing.T) {
	p := &TokenPool{} //nolint:exhaustruct
	p.reportTokenFetch(context.Background(), "vless://a@h:443#n1", false, 0, errors.New("boom"))
}

// TestFetchTokenLive_BatchesCandidatesIncrementally 分批改取：首次只取 1 个，失败且预算还有时
// 再取**未尝试过**的节点，直到成功或预算耗尽。
//
// 这是"同一时间戳下同一节点连续多条挂住"的直接修复：并发抓取（一个请求最多
// parallel_pool_size 份）若一次都取满一批，会同时选中同一批节点、同时挂死。
func TestFetchTokenLive_BatchesCandidatesIncrementally(t *testing.T) {
	var attempted []string
	src := &fakeNodeSource{
		nodes: []exitpool.Node{
			{RawURI: "vless://a@h:443#n1"},
			{RawURI: "vless://a@h:443#n2"},
			{RawURI: "vless://a@h:443#n3"},
		},
	}
	p := &TokenPool{source: src, cfg: config.StaticProvider(config.DefaultConfig())} //nolint:exhaustruct
	// 前两个节点失败、第三个成功：应逐个补取，且绝不重复尝试同一个节点。
	p.testFetch = func(_ context.Context, uri string, _ bool) (string, error) {
		attempted = append(attempted, uri)
		if strings.TrimSpace(uri) == "vless://a@h:443#n3" {
			return "tok", nil
		}
		return "", errors.New("挂住")
	}

	res := p.fetchTokenLive(context.Background(), false, true)
	if res.err != nil || res.tok != "tok" {
		t.Fatalf("应最终成功: %+v", res)
	}
	// 逐个补取、顺序为 n1→n2→n3，且绝不重复尝试同一个节点。
	// 入口/直连分支在候选选点之前、也会记入 attempted（uri 为空），这里只看候选节点。
	var got []string
	for _, u := range attempted {
		if strings.TrimSpace(u) != "" {
			got = append(got, strings.TrimSpace(u))
		}
	}
	want := []string{"vless://a@h:443#n1", "vless://a@h:443#n2", "vless://a@h:443#n3"}
	if len(got) != len(want) {
		t.Fatalf("应逐个补取 3 个候选节点, 实际 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 次尝试 = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// TestFetchTokenLive_NoCandidateRetryAfterBudgetExhausted 预算到点后不再补取**候选节点**
// （不会无限换节点重试）。注意入口/直连分支在候选选点之前、且不受 tried 限制，
// 故它可能已经尝试过一次——这里断言的是候选节点一次都没被尝试。
func TestFetchTokenLive_NoCandidateRetryAfterBudgetExhausted(t *testing.T) {
	var attempted []string
	src := &fakeNodeSource{
		nodes: []exitpool.Node{
			{RawURI: "vless://a@h:443#n1"},
			{RawURI: "vless://a@h:443#n2"},
		},
	}
	p := &TokenPool{source: src, cfg: config.StaticProvider(config.DefaultConfig())} //nolint:exhaustruct
	p.testFetch = func(_ context.Context, uri string, _ bool) (string, error) {
		attempted = append(attempted, uri)
		return "", errors.New("挂住")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预算已到点
	res := p.fetchTokenLive(ctx, false, true)
	if res.err == nil {
		t.Fatal("预算到点应返回错误")
	}
	for _, u := range attempted {
		if u != "" {
			t.Fatalf("预算已到点不应再尝试候选节点, 实际: %v", attempted)
		}
	}
}
