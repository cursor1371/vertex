package recaptcha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

// NodeSource 是 reCAPTCHA 抓取对出口节点池的最小消费契约（生产实现 *exitpool.Manager）。
// 类型位引用 []exitpool.Node 为 RFC 记录的显式豁免：候选结构体跨域只读传递。
type NodeSource interface {
	SelectForParallel(k int, debugMode bool) []exitpool.Node
	// SelectForTokenFetch 是 rT 抓取专用选点：把"对 rT 差/慢/冷却中"的节点排到同层最后
	// （好节点不足时仍会回退），不影响普通请求的选点。
	SelectForTokenFetch(k int, debugMode bool) []exitpool.Node
	// TokenFetchRank 返回节点当前的 rT 降权层（0 健康 / 1 差 / 2 冷却中），仅用于展示。
	TokenFetchRank(uri string) int
	NodeName(rawURI string) string
	// RecordTokenFetch 上报一次候选节点的 rT 抓取结果与耗时，供节点池更新 **rT 健康度**：
	// 它只影响 rT 抓取选点（降权/冷却），不参与普通请求选点、也不禁用节点。
	RecordTokenFetch(uri string, outcome exitpool.TokenFetchOutcome, ms int64, errStr string)
	// IncInFlight/DecInFlight 占用/释放节点在途名额。选点排序靠 InFlight 升序 + LastSelectedAt
	// 分散负载，而 rT 抓取此前完全不记账，导致并发抓取全砸在同一批节点上（节点数 ≤ 轮询
	// 宽度 5 时尤甚），加剧上游限流。必须配对调用（见 withInFlight）。
	IncInFlight(uri string)
	DecInFlight(uri string)
}

type TokenPool struct {
	net    *transport.NetworkClient
	cfg    config.ConfigProvider
	source NodeSource // 出口节点池消费端口；nil 时跳过候选轮询，仅走直连抓取
	fetch  func(proxyURI string) (string, error)
	// warm 是 rT token 预取池（默认关）。命中即取用，未命中一律回退实时抓取路径。
	warm *warmPool
	// testFetch 是单测注入的抓取钩子（签名与 fetchToken/fetchTokenQuiet 一致），仅测试使用。
	testFetch func(ctx context.Context, proxyURI string, debugMode bool) (string, error)
	// events 是 rT 抓取事件环形缓冲（实时记录流；零值即可用，无需初始化）。
	events tokenEventRing
}

// fallbackPollWidth 返回 rT 兜底轮询的出口节点采样宽度：
// 收敛为 min(parallel_pool_size, 5)——控制 rT 兜底放大（每个候选独立抓单次 token，
// 过宽的轮询会成倍命中 Google rT 端点，叠加并发后放大 429）。cfg 缺失或规格非法（<=0）时
// 退回历史固定值 5，保证零配置路径行为不变。
func fallbackPollWidth(cfg config.ConfigProvider) int {
	if cfg == nil {
		return 5
	}
	if size := cfg.ParallelPoolSize(); size > 0 {
		if size > 5 {
			return 5
		}
		return size
	}
	return 5
}

func NewTokenPool(net *transport.NetworkClient, cfg config.ConfigProvider, source NodeSource) *TokenPool {
	p := &TokenPool{net: net, cfg: cfg, source: source}
	// 预取池与请求路径共用同一抓取策略（见 fetchTokenLive），仅关闭"失败即失效版本缓存"。
	p.warm = newWarmPool(cfg, func(ctx context.Context) (string, error) {
		start := time.Now()
		res := p.fetchTokenLive(ctx, true, true)
		// 后台补货同样进实时记录流（来源标"预取"），便于直接看出补货到底在不在跑。
		p.recordTokenEvent(TokenSourcePrefetch, res, time.Since(start))
		return res.tok, res.err
	})
	return p
}

// NewTokenPoolCustom creates a token pool with a custom fetch function.
// Used for testing.
func NewTokenPoolCustom(fetch func(proxyURI string) (string, error)) *TokenPool {
	return &TokenPool{fetch: fetch}
}

// FetchTokenWithSession 在既有 Session 上执行 anchor→reload 校验
// （admin 节点测速路径的消费入口，委托共享核心实现）。
func (p *TokenPool) FetchTokenWithSession(ctx context.Context, sess *transport.Session) (string, error) {
	return FetchRecaptchaTokenWithSession(ctx, sess)
}

// GetTokenShared 每次调用都实时抓取一份独立的 reCAPTCHA token。
//
// 注意：不使用 singleflight 折叠并发——reCAPTCHA Enterprise token 是单次有效
// （single-use），并发请求必须各自持有独立 token，否则复用同一份会触发
// Google 侧验证失败（Failed to verify action / 429）。每个请求独立抓取，
// 由 15s 超时与前置池/出口节点降级路径兜底。
//
// ctx 参与生命周期管理：入口哨兵直接拒绝已取消的调用；15s 抓取预算自 ctx 派生
// （实际预算为 min(请求剩余时限, 15s)），请求死亡则抓取当场中止，杜绝幽灵抓取。
func (p *TokenPool) GetTokenShared(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if p.fetch != nil {
		return p.fetch("")
	}

	// 预取池优先：命中即取用（single-use，取出即移除）。未命中、未启用或池内仅有
	// 超龄 token 时一律回退下方既有的实时抓取路径，行为与未开启预取时完全一致。
	start := time.Now()
	if tok, ok := p.warm.pop(); ok {
		p.recordTokenEvent(TokenSourceWarm, tokenFetchResult{}, time.Since(start))
		return tok, nil
	}

	fetchCtx, fetchCancel := context.WithTimeout(ctx, p.liveBudget())
	defer fetchCancel()
	// 请求路径同样分批补取：一个请求最多 parallel_pool_size 个候选各抓一份 token、且都在
	// 同一秒发起。若每份一次取满 width 个节点，它们会同时选中同一批节点、同时挂死（表现为
	// "同一时间戳下同一个节点连续多条挂住"）。首次各取 1 个 → 并发自然分散；失败再补取。
	res := p.fetchTokenLive(fetchCtx, false, true)
	p.recordTokenEvent(TokenSourceLive, res, time.Since(start))
	return res.tok, res.err
}

// defaultLiveFetchBudget 是请求路径单轮 rT 抓取的默认预算（秒）。
const defaultLiveFetchBudget = 15 * time.Second

// liveBudget 返回请求路径单轮 rT 抓取的预算（配置非法时兜底 15s）。
//
// 注意它在请求关键路径上：prepareCandidate 里建联与取 token 并行发起、双双完成才发上游
// 请求，所以这个值等于"客户端最坏多等多久"；且 ctx 派生自候选 ctx，实际生效值 =
// min(候选剩余时限, 该值)——晚发的补位候选仍可能用不到调大的部分。
func (p *TokenPool) liveBudget() time.Duration {
	if p.cfg == nil {
		return defaultLiveFetchBudget
	}
	if sec := p.cfg.RecaptchaLiveBudgetSeconds(); sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return defaultLiveFetchBudget
}

// withInFlight 在尝试某个出口节点期间占用其 InFlight 名额。
//
// 选点（selectForParallelCore）按 InFlight 升序 + LastSelectedAt 分散负载，而 rT 抓取
// 此前从不记账：池子小的时候（节点数 ≤ 轮询宽度 5）每次调用几乎返回同一批节点，并发
// 抓取全砸在同样几个节点上，容易触发上游限流。用 defer 保证配对——DecInFlight 不做
// 下界钳制，漏减会把该节点算成"最空闲"而被优先选中。
func (p *TokenPool) withInFlight(uri string, fn func() (string, error)) (string, error) {
	if p.source == nil || uri == "" {
		return fn() // 入口/直连没有可归属的出口节点
	}
	p.source.IncInFlight(uri)
	defer p.source.DecInFlight(uri)
	return fn()
}

// errNoDirectFetch 是「禁止直连抓 rT 且没有可用出口候选」的错误：此时 rT 抓取不再静默
// 降级成直连，而是明确失败——候选以 infra 错误出局（请求失败），预取侧记失败并退避。
var errNoDirectFetch = errors.New(
	"禁止直连抓取 rT：没有可用出口候选节点（recaptcha_no_direct_fetch 已开启）")

// tokenFetchResult 是一次实时抓取的结果。
type tokenFetchResult struct {
	tok string
	// node 是最终供上 token 的出口节点 URI；空串表示走的是入口/直连路径。
	node string
	// noPath 表示压根没有可尝试的路径（禁止直连且无可用出口候选）：
	// 记录流据此留空节点，避免把"没尝试"误标成"入口/直连失败"。
	noPath bool
	// rtRank 是**选点时刻**该节点的 rT 降权层（0 健康 / 1 差 / 2 冷却中），仅供记录流展示。
	// 必须用选点时的快照：失败后健康度会立刻变化，事后再查会失真。
	rtRank int
	err    error
}

// tokenFetchPlan 是一次 rT 抓取的路径计划（纯由配置推导，便于单测穷举组合）。
type tokenFetchPlan struct {
	// tryEntryFirst 优先尝试入口/直连，成功即返回（不碰出口节点）。
	tryEntryFirst bool
	// allowEntry 是否允许入口/直连（proxyURI==""）这条路径。
	allowEntry bool
}

// tokenFetchPlanFor 由配置推导抓取计划。
//
// 「禁止直连抓 rT」是硬开关，优先于「优先前置/直连」：开启即整条入口/直连路径都不尝试
// （含前置代理——该路径在代码里与真·直连同属 proxyURI=="" 一档，无法区分）。
func tokenFetchPlanFor(cfg config.ConfigProvider) tokenFetchPlan {
	if cfg == nil {
		// 零配置路径保持既有行为：优先入口/直连，失败后轮询候选。
		return tokenFetchPlan{tryEntryFirst: true, allowEntry: true}
	}
	if cfg.RecaptchaNoDirectFetch() {
		return tokenFetchPlan{tryEntryFirst: false, allowEntry: false}
	}
	return tokenFetchPlan{tryEntryFirst: cfg.RecaptchaTryEntryOrDirect(), allowEntry: true}
}

// fetchTokenLive 是实时抓取主体（不含预取池查询）：按 tokenFetchPlanFor 推导的计划执行
// ——优先入口/直连（受 recaptcha_try_entry_or_direct 控制），失败后轮询健康候选出口节点，
// 最后兜底直连；禁止直连时只走出口候选节点。
//
// 预取池与请求路径共用本实现，保证"预取成功率 == 线上成功率"：早期版本让预取固定
// 只走前置/直连，在没有可用前置的环境下会永久失败、池恒为空。
//
// quiet=true 时不失效全局版本缓存（后台补货失败不应连累请求路径重拉 enterprise.js）。
// fetchTokenLive 的实时抓取主体。spreadOne=true 时**每批只取 1 个**候选节点，失败且预算还有
// 时再取下一批（跳过已试过的），直到成功或预算耗尽：
//   - 并发分散：后台预取一次发多份抓取（warmPoolRefillConcurrency=4），请求路径一次请求最多
//     parallel_pool_size 份（默认 15）且都在同一秒发起——若每份都取满一批节点，它们会同时撞在
//     同一批节点上、同时挂死；各取 1 个后自然分散到不同节点，rtRank 也刷得更快。
//   - 不牺牲换节点：失败后仍会补取未试过的节点，不会像"只取 1 个就放弃"那样降低成功率。
func (p *TokenPool) fetchTokenLive(ctx context.Context, quiet bool, spreadOne bool) tokenFetchResult {
	debugMode := false
	if p.cfg != nil {
		debugMode = p.cfg.DebugMode()
	}
	plan := tokenFetchPlanFor(p.cfg)
	fetch := p.fetchToken
	if quiet {
		fetch = p.fetchTokenQuiet
	}
	// testFetch 是测试注入钩子（仅测试设置）：用于替换每次节点尝试，便于对"分批改取/
	// 预算到点"等编排逻辑做确定性单测；生产恒为 nil，完全不影响行为。
	if p.testFetch != nil {
		fetch = p.testFetch
	}

	// lastErr/lastNode 记录最后一次尝试的失败原因与节点：失败时记录流要显示"死在哪个
	// 节点上"，否则节点留空会被映射成"入口/直连"，看起来像禁止直连失效滑落直连。
	var lastErr error
	lastNode := ""
	lastRank := 0
	tried := 0

	if plan.allowEntry && plan.tryEntryFirst {
		if tok, err := fetch(ctx, "", debugMode); err == nil && tok != "" {
			return tokenFetchResult{tok: tok}
		} else if err != nil {
			// 保留入口/直连的失败原因：候选为空时它才是真实死因，
			// 否则会退化成"failed to fetch recaptcha token"这种没有信息量的文案。
			lastErr, lastNode = err, ""
		}
	}

	// Fallback or disabled entry: poll healthy candidate nodes.
	// 兜底轮询宽度与竞速并发规格对齐（parallel_pool_size），不再写死：
	// 竞速开多少候选，rT 降级采样面就铺多宽。
	//
	// 分批改取而非一次取一批：并发抓取（一个请求最多 parallel_pool_size 个候选各抓一份）
	// 若一次都取满 width 个，它们会同时选中同一批节点、同时挂死。首次只取 spreadWidth 个，
	// 失败且预算还有时再取下一批（跳过已试过的），既分散了并发，又保留换节点重试能力。
	batchWidth := fallbackPollWidth(p.cfg)
	if spreadOne && batchWidth > 1 {
		batchWidth = 1
	}
	maxCands := fallbackPollWidth(p.cfg)
	triedURIs := make(map[string]bool, maxCands)
	// ranks 快照选点时刻的 rT 降权层：失败后健康度会立刻变化，事后再查会失真——记录流要
	// 回答的是"这次尝试时它是不是已经被降权/冷却了"。
	ranks := make(map[string]int, maxCands)

	for round := 0; p.source != nil && tried < maxCands; round++ {
		if ctx.Err() != nil {
			break
		}
		cands := p.source.SelectForTokenFetch(batchWidth+len(triedURIs), debugMode)
		// 跳过本轮已试过的节点，取够 batchWidth 个新节点。
		var batch []exitpool.Node
		for _, c := range cands {
			if triedURIs[c.RawURI] {
				continue
			}
			ranks[c.RawURI] = p.source.TokenFetchRank(c.RawURI)
			batch = append(batch, c)
			if len(batch) >= batchWidth {
				break
			}
		}
		if len(batch) == 0 {
			break // 已经没有未试过的可用节点
		}
		if round > 0 && debugMode {
			log.Printf("[Recaptcha] 补取 %d 个未尝试的出口候选（已试 %d 个）", len(batch), tried)
		}

		for _, cand := range batch {
			triedURIs[cand.RawURI] = true
			// 预算已到点：不再启动后续候选。CreateSession 不接 ctx，仍会去建会话/起临时
			// box（纯浪费），而且会把 lastNode 覆盖成"列表里最后一个候选"，把排查引偏——
			// 保留上一轮的值，才表示"死在哪个节点上"。
			if ctx.Err() != nil {
				break
			}
			tried++
			lastNode = cand.RawURI
			lastRank = ranks[cand.RawURI]
			attemptStart := time.Now()
			tok, err := p.withInFlight(cand.RawURI, func() (string, error) {
				return fetch(ctx, cand.RawURI, debugMode)
			})
			attemptMs := time.Since(attemptStart).Milliseconds()
			if err == nil && tok != "" {
				p.reportTokenFetch(ctx, cand.RawURI, true, attemptMs, nil)
				return tokenFetchResult{tok: tok, node: cand.RawURI, rtRank: ranks[cand.RawURI]}
			}
			p.reportTokenFetch(ctx, cand.RawURI, false, attemptMs, err)
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}

	// 兜底直连：仅当计划允许入口/直连、且它没被当作首选试过（否则重复一次没有意义）。
	if plan.allowEntry && !plan.tryEntryFirst {
		if tok, err := fetch(ctx, "", debugMode); err == nil && tok != "" {
			return tokenFetchResult{tok: tok}
		} else if err != nil {
			lastErr = err
			lastNode = "" // 最后尝试的是入口/直连
		}
	}

	if lastErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && tried > 0 {
			// 预算到点：把"试了几个候选"放句首，便于一眼区分"单节点挂住吃满 15s"（1 个）
			// 与"多节点各慢几秒、预算不够"（多个）。%w 保留底层 ctx 错误，
			// 使 reportTokenFetch 的"ctx 类失败不归因节点"判定继续成立。
			return tokenFetchResult{node: lastNode,
				err: fmt.Errorf("已试 %d 个出口候选后预算到点: %w", tried, lastErr)}
		}
		return tokenFetchResult{node: lastNode, rtRank: lastRank, err: lastErr}
	}
	if !plan.allowEntry {
		return tokenFetchResult{noPath: true, err: errNoDirectFetch}
	}
	return tokenFetchResult{err: fmt.Errorf("failed to fetch recaptcha token")}
}

// reportTokenFetch 把一次候选节点的 rT 抓取结果上报给出口节点池的 **rT 专属健康度**
// （见 exitpool.RecordTokenFetch）：只影响 rT 抓取选点的降权与 rT 冷却，**不再禁用节点**——
// "对 rT 坏"不等于"节点坏"，普通请求的候选池不该被 rT 问题侵蚀。
// 归因规则与请求路径的 applyNodeFailure 保持一致：
//   - uri 为空（入口/直连）不上报：没有可归属的出口节点；
//   - 失败且 ctx 已取消/超时（或错误本身属 context 类）不上报：那是抓取预算到点或请求
//     被取消，不是节点故障——后台补货的 4 路并发争抢本身就常以预算超时收场，把它记到
//     节点头上会成批误杀好节点。
func (p *TokenPool) reportTokenFetch(ctx context.Context, uri string, ok bool, ms int64, err error) {
	if p.source == nil || uri == "" {
		return
	}
	if ok {
		p.source.RecordTokenFetch(uri, exitpool.TokenFetchOK, ms, "")
		return
	}
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	// 被取消（竞速其它候选胜出 / 请求中断 / 客户端断开）：与节点无关，不上报。
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	// 挂住型：预算到点且一个尝试都没跑完 → 节点卡死，归因并先短冷却。
	var hang *tokenHangError
	if errors.As(err, &hang) {
		p.source.RecordTokenFetch(uri, exitpool.TokenFetchHang, ms, errStr)
		return
	}
	// 预算到点但尝试有完成（只是慢）：不再完全丢弃——它会记耗时与连败、让该节点在
	// rT 选点里降权，但**不按挂住升级冷却、也不禁用**（慢可能是预算不够或并发争抢，
	// 不能等同于节点故障）。
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		p.source.RecordTokenFetch(uri, exitpool.TokenFetchSlow, ms, errStr)
		return
	}
	p.source.RecordTokenFetch(uri, exitpool.TokenFetchFailed, ms, errStr)
}

// Close 停止预取池的后台补货协程并清空池（未启用预取时为空操作）。可重复调用。
func (p *TokenPool) Close() {
	if p == nil || p.warm == nil {
		return
	}
	p.warm.Close()
}

// WarmSnapshot 是预取池的观测快照：池内数量与命中/未命中/过期/失败计数，
// 以及"无收益自停"状态（Paused 为当前是否暂停补货，Pauses 为累计暂停次数）。
type WarmSnapshot struct {
	Size     int
	Hits     int64
	Misses   int64
	Expired  int64
	Failures int64
	Paused   bool
	Pauses   int64
}

// NoteRequest 告知预取池"有一次连接请求到达"（每个请求调用一次，由引擎入口注入）。
//
// 它只做一件事：作为预取自停锁存的解除点——池满后出现过期 token 即暂停补货，
// 新的连接请求一到达就立刻恢复补货（详见 warmPool.NoteRequest）。
// 未装配预取池（测试用 NewTokenPoolCustom）时为空操作。
func (p *TokenPool) NoteRequest(reqID string) {
	if p == nil || p.warm == nil {
		return
	}
	p.warm.NoteRequest(reqID)
}

// WarmStats 返回预取池观测快照。未装配预取池（测试用 NewTokenPoolCustom）时返回零值。
func (p *TokenPool) WarmStats() WarmSnapshot {
	if p == nil || p.warm == nil {
		return WarmSnapshot{}
	}
	return p.warm.stats()
}
