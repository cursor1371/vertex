package recaptcha

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// warmPoolConfig 是预取池对配置的最小消费契约（config.ConfigProvider 结构性满足，
// 单测可用极小假体替换）。
type warmPoolConfig interface {
	RecaptchaTokenPrefetch() bool
	RecaptchaPrefetchDepth() int
	RecaptchaPrefetchTTLSeconds() int
	RecaptchaPrefetchAdaptive() bool
	RecaptchaPrefetchBudgetSeconds() int
	DebugMode() bool
}

const (
	// defaultWarmFetchBudget 是单轮预取抓取的默认超时预算（与请求路径的 15s 抓取预算一致）。
	// 可经 recaptcha_prefetch_budget_sec 调高：开启「禁止直连抓 rT」后抓取只能走出口候选
	// （每个候选最多 3 次尝试 × 2~3 个 HTTP），15s 常连一个候选都跑不完；后台无人在等，
	// 调高它不影响请求延迟。
	defaultWarmFetchBudget = 15 * time.Second
	// warmPoolRefillConcurrency 单轮补货的最大并发抓取数。每个请求会瞬间消费 depth 个
	// token，补货必须跟得上消费速率，故不能低速串行；上限同时抑制后台抓取突发。
	warmPoolRefillConcurrency = 4
	// warmPoolIdleSuspend 无消费超过该时长即挂起补货：空闲实例不产生任何后台流量。
	warmPoolIdleSuspend = 5 * time.Minute
	// warmPoolBackoffBase/warmPoolBackoffMax 补货连续失败的指数退避区间。
	warmPoolBackoffBase = 1 * time.Second
	warmPoolBackoffMax  = 30 * time.Second

	// warmPoolZeroOutputPauseRounds 是"连续零产出即自停"的轮数阈值：池恒为空时（候选节点
	// 已被健康判定禁空、或抓取预算始终不够）退避重试会永远进行下去——白占会话、白打上游、
	// 刷日志。连续这么多轮一个 token 都没抓到即置自停锁存，等下一个连接请求再试。
	warmPoolZeroOutputPauseRounds = 3

	// defaultPrefetchDepth/defaultPrefetchTTL 是配置缺失或非法时的兜底值。
	defaultPrefetchDepth = 5
	defaultPrefetchTTL   = 90 * time.Second
)

// warmToken 是池中的一份预热 token 及其抓取时刻。
type warmToken struct {
	token     string
	fetchedAt time.Time
}

// warmPool 是 rT token 预取池。
//
// 语义约束：
//   - single-use：token 取出即从池中移除，绝不重复发放（并发 pop 由互斥锁保证互斥）。
//   - 消费驱动：仅近期有消费时才补货，空闲实例不产生后台抓取（仅按 token 到期时刻做一次
//     本地 prune 判定，用于就地发现"预取白费"）。
//   - 不阻塞请求：命中即返回，未命中/未启用/已过期一律由调用方回退既有实时抓取路径。
//   - TTL 硬约束：Google 侧 token 有效期 2 分钟，池内超过 TTL 的 token 直接丢弃，
//     绝不发放（过期 token 会以 timeout-or-duplicate 失败，且不落在第一发补偿的
//     识别范围内，会把候选直接打死）。
//   - 无收益自停：池内曾补满、随后出现过期 token（即这池预取根本没被消费）时暂停补货，
//     直到新的连接请求到达即立刻恢复（见 NoteRequest）。
type warmPool struct {
	cfg   warmPoolConfig
	fetch func(ctx context.Context) (string, error)

	baseCtx context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	stop    chan struct{}
	closed  atomic.Bool

	stopOnce sync.Once
	wg       sync.WaitGroup

	mu      sync.Mutex
	tokens  []warmToken
	lastPop time.Time
	// lastReqID 是上一次"连接请求"的 reqID（NoteRequest 写入，mu 保护），用于合并同一
	// 请求内的重复通知：同一请求的重复通知不算"新请求"，不重复解除自停。
	lastReqID string

	// suspended 是无收益自停锁存；pauses 是累计暂停次数（观测用）。
	suspended atomic.Bool
	pauses    atomic.Int64

	// zeroRounds 是连续"补货零产出"的轮数（mu 保护），成功一轮即归零。
	zeroRounds int

	hits     atomic.Int64
	misses   atomic.Int64
	expired  atomic.Int64
	failures atomic.Int64
}

// newWarmPool 创建并启动预取池；fetch 为实际抓取函数（由 TokenPool 注入）。
func newWarmPool(cfg warmPoolConfig, fetch func(ctx context.Context) (string, error)) *warmPool {
	baseCtx, cancel := context.WithCancel(context.Background())
	p := &warmPool{
		cfg:     cfg,
		fetch:   fetch,
		baseCtx: baseCtx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
	p.wg.Add(1)
	go p.run()
	return p
}

// Close 停止补货协程并清空池。可重复调用；关闭后 pop 恒为未命中。
func (p *warmPool) Close() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.closed.Store(true)
		close(p.stop)
		p.cancel()
	})
	p.wg.Wait()
	p.mu.Lock()
	p.tokens = nil
	p.mu.Unlock()
}

// pop 取出一个可用 token（single-use，取出即移除）。池空、未启用、已关闭或
// 仅有超龄 token 时返回 false，由调用方回退实时抓取。
func (p *warmPool) pop() (string, bool) {
	if p == nil || p.closed.Load() {
		return "", false
	}
	p.mu.Lock()
	// 无论命中与否都记录消费时刻并唤醒补货：未命中同样意味着有真实需求。
	p.lastPop = time.Now()
	if !p.enabled() {
		p.mu.Unlock()
		return "", false
	}
	_, suspendedNow := p.pruneLocked(time.Now())
	tok := ""
	ok := false
	if len(p.tokens) > 0 {
		tok = p.tokens[0].token
		p.tokens = p.tokens[1:]
		ok = true
	}
	p.mu.Unlock()

	if suspendedNow {
		p.logSuspend()
	}
	if ok {
		p.hits.Add(1)
	} else {
		p.misses.Add(1)
	}
	p.signal()
	return tok, ok
}

// put 放入一份新抓取的 token；池满、已关闭或空 token 直接丢弃。
func (p *warmPool) put(tok string) {
	if p == nil || tok == "" || p.closed.Load() {
		return
	}
	p.mu.Lock()
	kept, suspendedNow := p.pruneLocked(time.Now())
	if kept >= p.depth() {
		p.mu.Unlock()
		if suspendedNow {
			p.logSuspend()
		}
		return
	}
	p.tokens = append(p.tokens, warmToken{token: tok, fetchedAt: time.Now()})
	p.mu.Unlock()
	if suspendedNow {
		p.logSuspend()
	}
}

// NoteRequest 记录一次"连接请求到达"（每个请求调用一次，由引擎入口注入）。
//
// 它是自停锁存的解除点：池满后出现过期 token 即暂停补货，而只要有新的连接请求到达就
// 立刻恢复补货并唤醒补一轮。这里刻意不设"请求间隔 < TTL"之类的恢复条件——请求本身是
// 并发竞速的（一次请求会拉起 N 个候选、先后取走 N 个 token），补出来的 token 有机会被
// 本次请求后续的候选吃掉，按间隔条件延后恢复反而会白白错过这份收益。
//
// 按 reqID 合并同一请求内的重复通知（StreamChatCollect 会嵌套 StreamChat）：同一请求
// 的重复通知不算"新请求"，不重复解除。reqID 为空时无法合并，按新请求处理——退回旧行为，
// 不会因缺 ID 而永久停补。
func (p *warmPool) NoteRequest(reqID string) {
	if p == nil || p.closed.Load() || !p.enabled() || !p.adaptive() {
		return
	}
	p.mu.Lock()
	if reqID != "" && reqID == p.lastReqID {
		p.mu.Unlock()
		return
	}
	p.lastReqID = reqID
	p.mu.Unlock()

	// Swap 返回旧值：只有"确实处于自停"才需要唤醒补货（未自停时 pop 自己会 signal）。
	if !p.suspended.Swap(false) {
		return
	}
	log.Printf("[Recaptcha] 预取池恢复补货：新的连接请求到达（累计暂停 %d 次）", p.pauses.Load())
	p.signal()
}

// size 返回当前池内 token 数（含尚未被 prune 的超龄项）。
func (p *warmPool) size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokens)
}

// stats 返回观测快照。Size 是"仍可用（未超龄）"的 token 数而非切片长度：超龄项虽还
// 留在切片里，但绝不会被发放，展示可用量才能看出"池内看着是满的，其实全是过期 token"。
// 此处只读不 prune——展示读取绝不能反过来改动池状态或触发自停。
func (p *warmPool) stats() WarmSnapshot {
	if p == nil {
		return WarmSnapshot{}
	}
	now := time.Now()
	ttl := p.ttl()
	p.mu.Lock()
	size := 0
	for _, t := range p.tokens {
		if now.Sub(t.fetchedAt) <= ttl {
			size++
		}
	}
	p.mu.Unlock()
	return WarmSnapshot{
		Size:     size,
		Hits:     p.hits.Load(),
		Misses:   p.misses.Load(),
		Expired:  p.expired.Load(),
		Failures: p.failures.Load(),
		Paused:   p.suspended.Load(),
		Pauses:   p.pauses.Load(),
	}
}

// pruneLocked 丢弃超龄 token，返回剩余数量与"本次是否刚置上自停锁存"（日志由调用方
// 在解锁后记录，见 logSuspend）。调用方须持锁。
//
// 顺带判定"预取无收益"：prune 前池内已达目标深度、且本次丢弃了过期 token，说明这池
// 预取根本没被消费（请求间隔长于 TTL），置自停锁存暂停补货——否则每来一次请求就白抓
// 一池 token，命中恒 0、过期计数只增，还白白放大 Google 侧 429。
func (p *warmPool) pruneLocked(now time.Time) (kept int, suspendedNow bool) {
	ttl := p.ttl()
	before := len(p.tokens)
	alive := p.tokens[:0]
	dropped := 0
	for _, t := range p.tokens {
		if now.Sub(t.fetchedAt) > ttl {
			p.expired.Add(1)
			dropped++
			continue
		}
		alive = append(alive, t)
	}
	p.tokens = alive
	if dropped > 0 && before >= p.depth() {
		suspendedNow = p.suspend()
	}
	return len(p.tokens), suspendedNow
}

// suspend 置自停锁存，返回本次是否发生了"未暂停 → 暂停"跳变（幂等）。调用方须持锁，
// 故此处绝不记日志。
func (p *warmPool) suspend() bool {
	if !p.adaptive() {
		return false
	}
	if p.suspended.Swap(true) {
		return false
	}
	p.pauses.Add(1)
	return true
}

// logSuspend 记录一次暂停日志。日志会被 TUI 拦截并投递消息，绝不可持锁调用。
func (p *warmPool) logSuspend() {
	log.Printf("[Recaptcha] 预取池暂停补货：池内已达 %d 个且出现过期 token（预取未被消费）；待新的连接请求到达即立刻恢复",
		p.depth())
}

// enabled 返回总开关状态。
func (p *warmPool) enabled() bool {
	return p.cfg != nil && p.cfg.RecaptchaTokenPrefetch()
}

// adaptive 返回"无收益自停"开关。关闭即退回"有请求就补满"的旧行为。
func (p *warmPool) adaptive() bool {
	return p.cfg != nil && p.cfg.RecaptchaPrefetchAdaptive()
}

// depth 返回池目标深度（配置非法时兜底）。
func (p *warmPool) depth() int {
	if p.cfg == nil {
		return 0
	}
	if d := p.cfg.RecaptchaPrefetchDepth(); d > 0 {
		return d
	}
	return defaultPrefetchDepth
}

// ttl 返回 token 在池内的最长存活时间（配置非法时兜底）。
func (p *warmPool) ttl() time.Duration {
	if p.cfg == nil {
		return defaultPrefetchTTL
	}
	if sec := p.cfg.RecaptchaPrefetchTTLSeconds(); sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return defaultPrefetchTTL
}

// fetchTimeout 返回单轮预取的抓取预算（配置非法时兜底 15s）。
// 后台补货没有请求在等，调大它只影响后台会话持有时长，不增加请求延迟。
func (p *warmPool) fetchTimeout() time.Duration {
	if p.cfg == nil {
		return defaultWarmFetchBudget
	}
	if sec := p.cfg.RecaptchaPrefetchBudgetSeconds(); sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return defaultWarmFetchBudget
}

// shouldRefill 判定是否应继续补货：未关闭、总开关开启、未处于无收益自停、近期有消费
// 且池未满。关闭判定必须放在最前：Close 之后 put 变为空操作，若此处仍判定可补，
// 补货循环会因"抓取成功但池永远填不满"而持续空转（实测会一直转到空闲挂起才退出）。
//
// 判定必须整体在同一把锁内完成：suspended 与 lastPop 都由 pop/put 持该锁写入，若把
// 自停判断放在锁外（先读 suspended、再取锁读 lastPop），一次并发 pop 的自停判定会插进
// 两者之间——先读到"未自停"，再取锁时又读到该 pop 刚写入的新 lastPop，于是刚自停的池
// 仍会补一整轮（实测症状：自停后仍有后台抓取，抓取数恰等于 prune 后的缺额）。
func (p *warmPool) shouldRefill() bool {
	if p.closed.Load() || !p.enabled() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suspended.Load() {
		return false
	}
	if p.lastPop.IsZero() || time.Since(p.lastPop) >= warmPoolIdleSuspend {
		return false
	}
	return len(p.tokens) < p.depth()
}

// signal 非阻塞唤醒补货（信号可合并，池满/空闲时多一次唤醒无副作用）。
func (p *warmPool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run 是补货主循环：有需求（池未满、近期有消费且未自停）就补，补满/空闲/未启用/自停
// 则挂起等消费或新请求唤醒（见 NoteRequest）。
//
// "先判断再补、退避后直接重试"的结构是刻意的：若退避后回到 select 等唤醒，
// 一次失败会一直卡到下一次请求才恢复，池会长期停在未满状态。
func (p *warmPool) run() {
	defer p.wg.Done()
	backoff := time.Duration(0)
	for {
		if p.shouldRefill() {
			if backoff > 0 && !p.sleep(backoff) {
				return
			}
			if p.closed.Load() {
				return
			}
			if err := p.refillRound(); err != nil {
				p.failures.Add(1)
				log.Printf("[Recaptcha] 预取池补货失败（池内 %d/%d，单轮预算 %v）: %v",
					p.size(), p.depth(), p.fetchTimeout(), err)
				if backoff == 0 {
					backoff = warmPoolBackoffBase
				} else {
					backoff *= 2
				}
				if backoff > warmPoolBackoffMax {
					backoff = warmPoolBackoffMax
				}
				if p.noteRefillOutcome(false) {
					p.logSuspendZeroOutput()
				}
			} else {
				backoff = 0
				p.noteRefillOutcome(true)
			}
			continue
		}

		// 挂起：等下一次消费/新请求唤醒、关闭，或池内最早 token 到期。
		//
		// 到期唤醒是刻意的：过期必须能在空闲期"就地"被发现并置自停，而不是等到下一个请求
		// pop 时才 prune——否则面板会一直显示池满（实为一池全过期），暂停状态也要拖到第一个
		// 请求才可见。定时器只做本地 prune 判定，不发起任何抓取。
		//
		// 仅在"已启用 + 已开自停"时设定时器：关闭自停即要求退回旧的惰性行为（过期项留在
		// 切片里、等下一个请求再 prune），禁用态池则既不发放也不补货，保持完全惰性。
		var (
			expiry <-chan time.Time
			timer  *time.Timer
		)
		if p.enabled() && p.adaptive() {
			if d, ok := p.timeToExpiry(); ok {
				timer = time.NewTimer(d)
				expiry = timer.C
			}
		}
		select {
		case <-p.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-p.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-expiry:
			if suspendedNow := p.pruneExpired(); suspendedNow {
				p.logSuspend()
			}
		}
	}
}

// timeToExpiry 返回距离池内最早 token 超龄的时长；池内无 token 时返回 false（不设定时器，
// 空池空闲即零唤醒）。池内按抓取先后排列（put 追加、pop 从头取、prune 保序），故首个即
// 最早到期者。
//
// 额外加 1ms 余量：prune 用"严格大于"判定超龄，定时器若恰在边界触发会漏掉该 token，
// 于是回到循环顶部又立刻重新武装同一个到期点，形成空转。
func (p *warmPool) timeToExpiry() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tokens) == 0 {
		return 0, false
	}
	d := time.Until(p.tokens[0].fetchedAt.Add(p.ttl())) + time.Millisecond
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d, true
}

// pruneExpired 就地 prune 一次，返回本次是否刚置上自停锁存（到期定时器唤醒路径使用）。
func (p *warmPool) pruneExpired() bool {
	p.mu.Lock()
	_, suspendedNow := p.pruneLocked(time.Now())
	p.mu.Unlock()
	return suspendedNow
}

// noteRefillOutcome 记录一轮补货的结果，返回是否应就此暂停补货（连续零产出）。
//
// 池恒为空时（候选节点已被健康判定禁空、或抓取预算始终不够）退避重试会永远进行下去，
// 连续 warmPoolZeroOutputPauseRounds 轮一个 token 都没抓到即置自停锁存，等下一个连接
// 请求（NoteRequest）再恢复。调用方不得持锁：suspend 只需原子操作，但本方法要取锁读计数。
func (p *warmPool) noteRefillOutcome(ok bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		p.zeroRounds = 0
		return false
	}
	p.zeroRounds++
	if p.zeroRounds < warmPoolZeroOutputPauseRounds {
		return false
	}
	return p.suspend()
}

// logSuspendZeroOutput 记录"连续零产出"触发的暂停日志。绝不可持锁调用。
func (p *warmPool) logSuspendZeroOutput() {
	log.Printf("[Recaptcha] 预取池暂停补货：连续 %d 轮补货零产出（池内 %d/%d，单轮预算 %v）；待新的连接请求到达即立刻恢复",
		warmPoolZeroOutputPauseRounds, p.size(), p.depth(), p.fetchTimeout())
}

// refillRound 并发抓取一轮（至多 warmPoolRefillConcurrency 个），返回首个错误。
func (p *warmPool) refillRound() error {
	need := p.depth() - p.size()
	if need <= 0 {
		return nil
	}
	if need > warmPoolRefillConcurrency {
		need = warmPoolRefillConcurrency
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		firstE  error
		okCount int
	)
	for i := 0; i < need; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(p.baseCtx, p.fetchTimeout())
			defer cancel()
			tok, err := p.fetch(ctx)
			if err != nil || tok == "" {
				mu.Lock()
				if firstE == nil {
					if err != nil {
						firstE = err
					} else {
						firstE = fmt.Errorf("prefetch: empty token")
					}
				}
				mu.Unlock()
				return
			}
			p.put(tok)
			mu.Lock()
			okCount++
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 仅当本轮一个都没抓到才算失败：部分成功说明链路可用，应继续补满，
	// 而不是因为个别节点抖动就整体退避到 30 秒（那会让池长期填不满）。
	if okCount == 0 {
		return firstE
	}
	return nil
}

// sleep 等待 d，被 Close 打断时返回 false。
func (p *warmPool) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.stop:
		return false
	case <-timer.C:
		return true
	}
}
