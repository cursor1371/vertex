package vertex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

// 窗口监督者选点口径：补位先走严格通道（尊重冷却），不足即走宽松通道（忽略冷却）。
// 宽松通道仅排除 Disabled 节点，故"在飞归零且双通道均无可补位点"等价于
// "节点池内不存在任何启用节点"，属不可时间恢复的稳态——立即收敛失败，
// 不做任何定时空轮询等待。

// requestTimeout 返回每候选独立超时（锚定各自 launchNode 时刻）；配置 ≤0 时兜底 180 秒。
// 本函数是全引擎唯一的单候选超时口径，取代旧版"每轮重置"的批级超时语义。
func requestTimeout(cfg config.ConfigProvider) time.Duration {
	timeoutSec := cfg.RequestTimeoutSeconds()
	if timeoutSec <= 0 {
		timeoutSec = 180
	}
	return time.Duration(timeoutSec) * time.Second
}

// isContextClass 判定错误是否为 context 类（取消或超时）。
func isContextClass(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// nodeNameOrURI 返回节点的展示名；当名称缺失（空或 Unknown）时回退为原始 URI，
// 保证统计里"按代理节点"能区分具体代理，而不是全部聚到 Unknown。
func nodeNameOrURI(pool NodePool, uri string) string {
	name := pool.NodeName(uri)
	if name == "" || name == "Unknown" {
		return uri
	}
	return name
}

// raceConfig 是 RunRace 的可配置策略。
type raceConfig[T any] struct {
	preserveRaceCtxOnWin bool // 仅控制 RunRace 返回时是否 cancel 主竞速 ctx；落败候选的关停由 cancelOthers 独立完成，与此字段无关
	failFastOnHardError  bool
	// latencyLabel 是胜出日志中耗时指标的显示名；空值在选项装配后归一为 "首字耗时"
	// （首个有效结果即胜出的流式口径）。收齐全量响应才交付裁决的调用方应显式传 "总耗时"。
	// 耗时数值统一为请求级口径：自竞速启动起算，涵盖窗口填充与此前全部失败补位尝试；
	// 节点健康记账（RecordTest）仍使用每候选独立耗时。
	latencyLabel string
	// isWinningResult 决定某个成功结果是否可"立即胜出"。
	// nil 表示首个无错误结果立即胜出（流式默认）。
	// 非 nil：返回 true 即时胜出，false 表示结果被收集（CompleteChat 的非 STOP 结果）。
	isWinningResult func(val T) bool
	// collectedResults 累积 isWinningResult 返回 false 的成功结果。
	collectedResults []raceResult[T]
	// finalizeCollected 在收集多个非胜出结果后调用，选出最佳结果并返回其节点 URI。
	// 返回的 URI 用于竞速记账：收集模式下"真正成功的那个节点"即最终被选中的那个，
	// 其余候选一律计为不成功。nil 时直接取收集到的第一个结果。
	finalizeCollected func([]raceResult[T]) (T, string, error)
}

type RaceOption[T any] func(*raceConfig[T])

// WithPreserveRaceCtxOnWin 使 RaceCtx 在胜出路径上不被 cancel（默认 defer cancel() 总是执行）。
// 此 flag 仅控制主竞速 context 的 defer cancel；落败候选的关停由 cancelOthers 独立完成，与此选项无关。
// 流式路径（StreamParallel）用此选项，使胜出后保留主 ctx 让胜出节点的流继续传完。
func WithPreserveRaceCtxOnWin[T any]() RaceOption[T] {
	return func(cfg *raceConfig[T]) {
		cfg.preserveRaceCtxOnWin = true
	}
}

// WithWinningCheck 注入成功判定：fn 返回 true 时该结果立即胜出；
// fn 返回 false 时结果被收集，所有候选结束后通过 pickBestResult 选出最佳结果。
func WithWinningCheck[T any](fn func(T) bool) RaceOption[T] {
	return func(cfg *raceConfig[T]) {
		cfg.isWinningResult = fn
	}
}

// WithFailFastOnHardError 使 RunRace 在收到不可重试硬错误时立即终止请求（流式首帧默认行为）。
// 不注入时，硬错误只记录节点失败、继续补位（CompleteChat 语义）。
func WithFailFastOnHardError[T any]() RaceOption[T] {
	return func(cfg *raceConfig[T]) {
		cfg.failFastOnHardError = true
	}
}

// WithCollectedFinalizer 注入最终结果选择函数，在收集多个非胜出结果后调用。
// 函数需返回 (选中结果, 该结果所属节点 URI, error)，URI 用于把"真正成功的节点"计入竞速统计。
// 默认行为：取收集到的第一个结果。
func WithCollectedFinalizer[T any](fn func([]raceResult[T]) (T, string, error)) RaceOption[T] {
	return func(cfg *raceConfig[T]) {
		cfg.finalizeCollected = fn
	}
}

// WithLatencyLabel 自定义胜出日志的耗时指标名。
// 默认空值回退 "首字耗时"——适用于首个有效结果即胜出的流式路径；
// 收齐全量响应后才交付裁决的调用方（CompleteChat 等）必须显式声明，
// 避免把整响应耗时误标为首字耗时。
func WithLatencyLabel[T any](label string) RaceOption[T] {
	return func(cfg *raceConfig[T]) {
		cfg.latencyLabel = label
	}
}

type raceResult[T any] struct {
	uri string
	val T
	err error
	// ms 是候选自身耗时（毫秒），在结果回收时填充；收集模式最终选出胜者时用于记账。
	ms int64
}

// errorPriority 返回错误的优先级数值（越小优先级越高）。
// 核心原则：确定性业务真相优先于节点瞬态噪声。
//
//	0 Committed：已向客户端输出内容后断流（Truncated），绝不重试，最高优先透传真实原因；
//	1 FailFast：请求级硬错误/拦截（safety/invalid/notfound/infra）——请求载荷对所有节点
//	  恒相同，任一节点的确定性判定即全局真相（依据见方案 2.1 节）；
//	2 RateLimit：上游限流（429/RESOURCE_EXHAUSTED），客户端应退避；
//	3 其他 Transient：节点级网络/服务抖动（502/503/504/network/empty/auth），可补位噪声；
//	4 Terminal：节点级权限（403 permission）或单节点取消（context），不判全局；
//	5 其他未知错误。
//
// 注意：本函数只决定"收敛路径"（pickBestError）的挑选结果；流式 failFast 的
// 首达即终止语义（RunRace 内 ClassifyBatch()==FailFast 直接 return）不受本函数影响。
func errorPriority(err error) int {
	var ve *VertexError
	if !errors.As(err, &ve) {
		return 5
	}
	switch ve.ClassifyBatch() {
	case Committed:
		return 0
	case FailFast:
		return 1
	case Transient:
		if ve.Kind == "ratelimit" || ve.Code == 429 {
			return 2
		}
		if ve.Kind == "notsignature" {
			// 仅签名缺失（QUERY_SIGNATURE_NOT_FOUND）与 429 同级：签名过期是节点级
			// 瞬态，换节点重取即可恢复。判据必须用 Kind 精确匹配——Code 已是 404，
			// 若按 Code==404 判会把真正的"模型不存在"404（notfound/client，Terminal）
			// 一起误判为可重试。
			return 2
		}
		return 3
	case Terminal:
		return 4
	default:
		return 5
	}
}

// pickBestError 从多个错误中挑选优先级最高（数值最小）的一个返回。
// 容忍切片中的 nil 元素（跨轮累积场景的防御），全空时返回内部错误。
func pickBestError(errs []error) error {
	var best error
	bestPrio := 0
	for _, e := range errs {
		if e == nil {
			continue
		}
		p := errorPriority(e)
		if best == nil || p < bestPrio {
			best = e
			bestPrio = p
		}
	}
	if best == nil {
		return NewInternalError("all nodes failed", nil)
	}
	return NormalizeError(best)
}

// convergeRaceFailure 统一收敛请求失败结果为规范化的 *VertexError。
func convergeRaceFailure(ctx context.Context, failedErrors []error, lastErr error) *VertexError {
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return NormalizeError(ctx.Err())
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if len(failedErrors) > 0 {
				return NormalizeError(pickBestError(failedErrors))
			}
			return NormalizeError(ctx.Err())
		}
	}
	if len(failedErrors) > 0 {
		return NormalizeError(pickBestError(failedErrors))
	}
	if lastErr != nil {
		return NormalizeError(lastErr)
	}
	return NewInternalError("all nodes failed", nil)
}

// RunRace 以恒满滑动窗口模型调度多出口节点竞速。
//
// 核心机制（取代旧版"批次快照 + 对冲延迟 + 整批退避重试"）：
//   - 启动即经严格选点拉满 W=ParallelPoolSize 个候选；
//   - 任一槽位释放（候选失败或非胜出收集）立即瞬时补位，先严格通道、不足走宽松通道
//     （忽略冷却，Disabled 始终排除），同一 URI 同时最多一个活跃候选；
//   - 发射硬预算 B=(MaxRetries+1)×W：统计所有成功启动的候选总数，胜出即停，
//     FailFast 早退作废剩余；预算耗尽或在飞归零且无可补位点时统一收敛；
//   - 每候选独立 requestTimeout 超时（锚定各自启动时刻），主竞速 ctx 无全局大限；
//   - context 类候选错误不改节点健康态（维持"ctx 不记账"规则），父 ctx 存活时照常补位；
//   - 客户端 ctx 取消是一票终止，收敛为 ctx 错误。
//
// 结果处置细则见窗口重构方案 §4 处置表（.kilo/plans/windowed-race-refactor.md）。
func RunRace[T any](ctx context.Context, pool NodePool, cfg config.ConfigProvider,
	run func(ctx context.Context, proxyURI string) (T, error),
	opts ...RaceOption[T],
) (T, error) {
	var rc raceConfig[T]
	for _, o := range opts {
		o(&rc)
	}
	if rc.latencyLabel == "" {
		rc.latencyLabel = "首字耗时" // 流式默认口径：首个有效结果即胜出
	}
	if pool == nil {
		pool = nopNodePool{}
	}

	var zero T
	window := cfg.ParallelPoolSize()
	cands := pool.SelectForParallel(window, cfg.DebugMode())
	if len(cands) == 0 && cfg.ParallelPoolEnabled() {
		// 初始严格选点为空（如全员 429 冷却）时以宽松通道兜底拉起窗口，
		// 避免误降级直连；仍为空（零启用节点）才走直连分支。
		cands = pool.SelectForParallelRelaxed(window, cfg.DebugMode())
	}

	if !cfg.ParallelPoolEnabled() || len(cands) == 0 {
		return runDirectMode(ctx, pool, cfg, rc.failFastOnHardError, rc.latencyLabel, run)
	}

	budget := (cfg.MaxRetries() + 1) * window
	if budget <= 0 {
		// 防御：异常配置（如 max_retries 为负）会使预算归零，窗口路径将一发不发
		// 且永无结果可消费而挂死；降级直连路径（其内部对轮数下限钳制为 1）。
		return runDirectMode(ctx, pool, cfg, rc.failFastOnHardError, rc.latencyLabel, run)
	}
	// launched 是发射计数器（含初始填充与全部补位），严格受 budget 封顶；
	// 初值必须为 0，由 tryLaunchBatch(cands) 统一累加，切勿预置 len(cands)
	// （预置会使预算闸门在初始填充前即饱和，导致一发不发）。
	launched := 0
	// failedErrors 累积全部候选级失败（含 context 类超时），供终局收敛挑选最优错误。
	var failedErrors []error
	// lastFailedURI 记录最后一个失败候选的 URI，供失败收敛时把请求归属到该节点；
	// 仅由非 context 类失败分支写入，避免客户端取消/候选超时被错误归属。
	var lastFailedURI string

	if cfg.DebugMode() {
		log.Printf("[Vertex] [RunRace] 恒满窗口竞速: 窗口=%d, 初始候选=%d, 发射预算=%d", window, len(cands), budget)
		for _, c := range cands {
			log.Printf("[Vertex] [RunRace] 初始候选节点: %s", c.Name)
		}
	}

	cli.UpdateReqState(RequestIDFromContext(ctx), "⚡ 并发竞速", "\033[33m", fmt.Sprintf("并行节点: %d", len(cands)))

	ctxRace, cancel := context.WithCancel(ctx)
	var returnedOnWinPath bool
	defer func() {
		// 非胜出路径返回 → 总是 cancel()；胜出路径且 preserveRaceCtxOnWin=true → 不 cancel，
		// 保留主 ctxRace 让胜出节点那条流继续传完。落败候选始终由 cancelOthers 独立关停。
		if !returnedOnWinPath || !rc.preserveRaceCtxOnWin {
			cancel()
		}
	}()

	resCh := make(chan raceResult[T], window)
	var active int32
	activeKeys := make(map[string]bool)
	// pending 记录"已发射但尚未记账"的候选 URI：胜出后窗口内其余候选统一记为 lost（被抢先）。
	// 统计口径：一次批量竞速只有胜出者算成功，其余候选一律视为不成功。
	pending := make(map[string]bool)
	var mu sync.Mutex

	// candidateCancels 存储每个候选的独立取消函数，用于胜出后立即取消落败者。
	// 补位产生的候选也会登记于此，故关停循环必须遍历本 map 而非初始快照。
	candidateCancels := make(map[string]context.CancelFunc)

	candidateStarts := make(map[string]time.Time)

	// launchNode 启动单个候选；URI 已活跃时去重跳过并返回 false。
	launchNode := func(uri string) bool {
		mu.Lock()
		if activeKeys[uri] {
			mu.Unlock()
			return false
		}
		activeKeys[uri] = true
		pending[uri] = true
		candidateStarts[uri] = time.Now()
		mu.Unlock()

		pool.IncInFlight(uri)
		atomic.AddInt32(&active, 1)
		// 每候选独立倒计时（锚定本时刻），胜出者沿用自身剩余时限交付。
		candCtx, candCancel := context.WithTimeout(ctxRace, requestTimeout(cfg))
		mu.Lock()
		candidateCancels[uri] = candCancel
		mu.Unlock()

		go func(u string) {
			defer pool.DecInFlight(u)
			// 防御性 recover：候选 run 内部 panic 不能打穿竞速引擎。
			// 恢复后以非阻塞方式把内部错误结果推入 resCh；若主竞速 ctx 已取消则直接放弃，
			// 避免阻塞在已关闭/无人消费的通道上导致 active 计数失步。
			defer func() {
				if r := recover(); r != nil {
					panicErr := NewInternalError(fmt.Sprintf("node candidate panic on %s: %v", u, r), nil)
					select {
					case resCh <- raceResult[T]{uri: u, err: panicErr}:
					case <-ctxRace.Done():
					}
				}
			}()
			v, err := run(candCtx, u)
			select {
			case resCh <- raceResult[T]{uri: u, val: v, err: err}:
			case <-ctxRace.Done():
			}
		}(uri)
		return true
	}

	cancelOthers := func(winner string) {
		mu.Lock()
		for u, c := range candidateCancels {
			if u != winner {
				c()
			}
		}
		mu.Unlock()
	}

	cancelAll := func() {
		mu.Lock()
		for _, c := range candidateCancels {
			c()
		}
		mu.Unlock()
	}

	// recordLostOthers 把仍在"待记账"集合中的候选统一记为 lost（被抢先）。
	// 一次批量竞速只有胜出者算成功，窗口内其余候选一律视为不成功。
	// 已交付并记账的候选（win/collect/timeout/error）在结果回收时已移出集合，不会重复计入。
	// 注意：调用方需自行保证不在客户端取消路径上调用，避免把主动断开记成节点失败。
	recordLostOthers := func() {
		reqID := RequestIDFromContext(ctx)
		type loser struct {
			uri string
			ms  int64
		}
		mu.Lock()
		losers := make([]loser, 0, len(pending))
		for u := range pending {
			ms := int64(0)
			if st, ok := candidateStarts[u]; ok {
				ms = time.Since(st).Milliseconds()
			}
			losers = append(losers, loser{uri: u, ms: ms})
			delete(pending, u)
		}
		mu.Unlock()
		for _, l := range losers {
			stats.RecordRaceOutcome(reqID, nodeNameOrURI(pool, l.uri), l.uri, "lost", l.ms)
		}
	}

	// tryLaunchBatch 批量发射候选列表，受窗口与预算双重约束，返回实际发射数。
	tryLaunchBatch := func(nodes []exitpool.Node) int {
		count := 0
		for _, c := range nodes {
			if launched >= budget || atomic.LoadInt32(&active) >= int32(window) {
				break
			}
			if launchNode(c.RawURI) {
				launched++
				count++
			}
		}
		return count
	}

	// maybeRefill 补满窗口：先严格选点，缺口走宽松通道（忽略冷却）。
	// 返回本次实际新发射数。
	//
	// 注意向池索要整窗规模而非精确缺口：选点排序可能被在飞 URI 前置占位
	// （如刚失败的节点降入低优先层而在飞节点占据首位），按缺口数请求会
	// 因引擎侧去重拒绝而漏掉排序更深的空闲节点，造成补位饥饿；
	// 多要的部分由 launchNode 在飞去重与窗口/预算双闸门兜底，不会超发。
	maybeRefill := func() int {
		target := window - int(atomic.LoadInt32(&active))
		if target <= 0 || launched >= budget || ctxRace.Err() != nil {
			return 0
		}
		got := tryLaunchBatch(pool.SelectForParallel(window, cfg.DebugMode()))
		if got < target && launched < budget && ctxRace.Err() == nil {
			got += tryLaunchBatch(pool.SelectForParallelRelaxed(window, cfg.DebugMode()))
		}
		return got
	}

	// evaluateTerminal 在每个结果处理完毕后的终结评估：
	// 在飞归零即收敛——补位机会已在各结果分支同步用尽（maybeRefill 若能发射，
	// active 必然 >0；无法发射说明预算耗尽，或严格+宽松双通道均无可选点即
	// 节点池已无任何启用节点）。收敛时 collectedResults 优先于错误，
	// 与旧版终局优先级一致。返回 (是否终结, 值, 错误)。
	evaluateTerminal := func(lastResErr error) (bool, T, error) {
		if atomic.LoadInt32(&active) != 0 {
			return false, zero, nil
		}
		cancelAll()
		cancel()
		if len(rc.collectedResults) > 0 {
			val := rc.collectedResults[0].val
			winURI := rc.collectedResults[0].uri
			if rc.finalizeCollected != nil {
				v, uri, finErr := rc.finalizeCollected(rc.collectedResults)
				if finErr != nil {
					return true, zero, NormalizeError(finErr)
				}
				val, winURI = v, uri
			}
			// 收集模式下的"真正成功节点" = 最终被选中的那个：记 win（并补上节点归属），
			// 其余候选统一记 lost。此前收集路径既不记 win 也不归属节点，
			// 导致非流式/聚合流请求在统计里既无胜出也无节点。
			if winURI != "" {
				ms := int64(0)
				for _, r := range rc.collectedResults {
					if r.uri == winURI {
						ms = r.ms
						break
					}
				}
				stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, winURI), winURI, "win", ms)
				stats.RecordNode(RequestIDFromContext(ctx), nodeNameOrURI(pool, winURI), winURI)
			}
			recordLostOthers()
			return true, val, nil
		}
		// 失败收敛：把请求归属到最后一个失败候选（父 ctx 已取消时保留未归属，
		// 避免把客户端断开/超时误记到节点上）。
		if ctx.Err() == nil {
			if lastFailedURI != "" {
				stats.RecordNode(RequestIDFromContext(ctx), nodeNameOrURI(pool, lastFailedURI), lastFailedURI)
			}
			recordLostOthers()
		}
		return true, zero, convergeRaceFailure(ctx, failedErrors, lastResErr)
	}

	// raceStart 是请求级耗时锚点：胜出日志的 首字耗时/总耗时 自此起算，
	// 涵盖窗口填充与此前全部失败补位尝试；节点健康记账仍用每候选 elapsedMs。
	raceStart := time.Now()

	// 初始填充：一次拉满窗口（受预算与窗口双重约束，实际发射 = min(W, 可用节点, 预算)）。
	tryLaunchBatch(cands)

	for {
		select {
		case <-ctx.Done():
			cancelAll()
			cancel()
			return zero, convergeRaceFailure(ctx, failedErrors, ctx.Err())

		case res := <-resCh:
			atomic.AddInt32(&active, -1)
			name := pool.NodeName(res.uri)

			mu.Lock()
			// 候选已交付结果：解除 URI 占用，使其可被后续补位再次选中。
			// （窗口模型允许同一节点跨轮次复用；activeKeys 只表达"当前在飞"语义。）
			delete(activeKeys, res.uri)
			delete(pending, res.uri) // 该候选结局已按下方分支记账，移出待记账集合
			candStart := candidateStarts[res.uri]
			// 失败即终局：该候选的 goroutine 与流式中继均已退出，立即释放其
			// 个人定时器，避免滞留至竞速结束或时限自然到点。（成功/收集结果
			// 的候选可能仍在交付，其上下文必须存活，交由 cancelOthers/cancelAll 处置。）
			if res.err != nil {
				if c, ok := candidateCancels[res.uri]; ok {
					c()
					delete(candidateCancels, res.uri)
				}
			}
			mu.Unlock()
			elapsedMs := float64(time.Since(candStart).Milliseconds())
			res.ms = int64(elapsedMs)

			if res.err == nil {
				// 判定是否可立即胜出。
				if rc.isWinningResult == nil || rc.isWinningResult(res.val) {
					name := nodeNameOrURI(pool, res.uri)
					log.Printf("[Racing] 竞速胜出节点: %s (%s: %.2fs)", name, rc.latencyLabel, time.Since(raceStart).Seconds())
					cli.UpdateReqWinner(RequestIDFromContext(ctx), name)
					cli.UpdateReqState(RequestIDFromContext(ctx), "🟢 数据传输", "\033[32m", "已建立连接")
					stats.RecordNode(RequestIDFromContext(ctx), name, res.uri)
					stats.RecordRaceOutcome(RequestIDFromContext(ctx), name, res.uri, "win", int64(elapsedMs))
					pool.RecordTest(res.uri, true, elapsedMs, "")

					// 统计口径：批量竞速只有胜出者算成功，窗口内其余候选一律记为不成功。
					recordLostOthers()

					returnedOnWinPath = true
					cancelOthers(res.uri)
					return res.val, nil
				}

				// 成功但非胜出（收集）：释放槽位照常补位，等待择优。
				rc.collectedResults = append(rc.collectedResults, res)
				pool.RecordTest(res.uri, true, elapsedMs, "")
				stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, res.uri), res.uri, "collect", int64(elapsedMs))
				if cfg.DebugMode() {
					log.Printf("[Racing] 节点 %s 结果已收集(非胜出)，触发槽位补位", name)
				}
				maybeRefill()
			} else if isContextClass(res.err) {
				// context 类候选错误（含每候选独立超时到点）：不记账健康态。
				if ctx.Err() != nil {
					// 客户端已断开/外部取消：立即终止。
					cancelAll()
					cancel()
					return zero, convergeRaceFailure(ctx, failedErrors, res.err)
				}
				if cfg.DebugMode() {
					log.Printf("[Racing] 节点 %s 候选级超时/取消，换点补位: %v", name, res.err)
				}
				stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, res.uri), res.uri, "timeout", int64(elapsedMs))
				failedErrors = append(failedErrors, res.err)
				maybeRefill()
			} else {
				if cfg.DebugMode() {
					log.Printf("[Racing] 节点 %s 失败: %s", name, res.err.Error())
				}

				failedErrors = append(failedErrors, res.err)
				// 记录最后一个失败候选，供终局失败收敛时把请求归属到该节点。
				lastFailedURI = res.uri
				// 429 是上游正常竞争限流而非节点失败：单列 ratelimit 结局，
				// 不进 race_errors、不拉低节点 Racing 成功率（健康态由
				// applyNodeFailure → RecordRateLimit 做纯冷却避让）。
				if ve := asVertexError(res.err); ve != nil && (ve.Kind == "ratelimit" || ve.Code == 429) {
					stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, res.uri), res.uri, "ratelimit", int64(elapsedMs))
				} else {
					stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, res.uri), res.uri, "error", int64(elapsedMs))
				}

				// 节点池健康态旁路（不触碰错误内容、不影响裁决）。
				applyNodeFailure(pool, res.uri, res.err)

				ve := asVertexError(res.err)
				if rc.failFastOnHardError && ve != nil && ve.ClassifyBatch() == FailFast {
					if cfg.DebugMode() {
						log.Printf("[Racing] 节点 %s 触发请求级全局硬错误(%s)，终止请求: %s", name, ve.Kind, ve.Message)
					}
					cancelAll()
					cancel()
					// 硬错误终局归属触发该错误的节点，比"最后一个失败候选"更准确。
					if res.uri != "" {
						stats.RecordNode(RequestIDFromContext(ctx), nodeNameOrURI(pool, res.uri), res.uri)
					}
					// 硬错误终局同样只有触发者被记账，其余在飞候选记为不成功。
					recordLostOthers()
					return zero, ve
				}

				maybeRefill()
			}

			if done, val, err := evaluateTerminal(res.err); done {
				return val, err
			}
		}
	}
}

// runDirectMode 是直连/锁定模式（节点池关闭或严格选点为空）的降级路径：
// 视为窗口=1，对 ActiveNodeURI 顺序尝试至预算耗尽（MaxRetries+1 次），无退避连续接力。
// FailFast 门禁、客户端取消静默、context 不记账规则与主路径一致；
// latencyLabel 为胜出日志的请求级耗时指标名（与主窗口路径同格式）。
func runDirectMode[T any](ctx context.Context, pool NodePool, cfg config.ConfigProvider,
	failFast bool, latencyLabel string, run func(ctx context.Context, proxyURI string) (T, error),
) (T, error) {
	var zero T
	proxy := cfg.ActiveNodeURI()
	log.Printf("[Vertex] [RunParallel] 降级为单节点运行: %s", pool.NodeName(proxy))
	raceStart := time.Now()

	attempts := cfg.MaxRetries() + 1
	if attempts < 1 {
		// 下限钳制：异常配置（max_retries 为负）至少保证单发尝试，
		// 避免零轮循环静默返回零值成功。
		attempts = 1
	}
	var bestErr error
	for round := 1; round <= attempts; round++ {
		if ctx.Err() != nil {
			return zero, convergeRaceFailure(ctx, nil, ctx.Err())
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, requestTimeout(cfg))
		attemptStart := time.Now()
		val, err := run(attemptCtx, proxy)
		if err == nil {
			winName := nodeNameOrURI(pool, proxy)
			log.Printf("[Racing] 竞速胜出节点: %s (%s: %.2fs)", winName, latencyLabel, time.Since(raceStart).Seconds())
			stats.RecordNode(RequestIDFromContext(ctx), winName, proxy)
			stats.RecordRaceOutcome(RequestIDFromContext(ctx), winName, proxy, "win", time.Since(attemptStart).Milliseconds())
			// 胜出路径不得取消 attemptCtx：流式候选的交付仍由该上下文供数
			// （与主窗口路径的胜者语义一致——沿用个人时限直至交付完成，
			// ctx 随请求父级结束而释放）。_ = attemptCancel 为有意不取消的
			// 显式声明，供 lostcancel 类分析器识别。
			_ = attemptCancel
			return val, nil
		}
		attemptCancel()
		if isContextClass(err) {
			if ctx.Err() != nil {
				return zero, NormalizeError(ctx.Err())
			}
			// 候选级独立超时到点：视作本轮失败，继续下一发。
			if cfg.DebugMode() {
				log.Printf("[Racing] 单节点候选级超时，立即接力: %v", err)
			}
			stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, proxy), proxy, "timeout", time.Since(attemptStart).Milliseconds())
		} else {
			// 锁定模式/直连模式失败也旁路记录健康态；proxy==""（直连/前置）时内部无操作。
			// 429 单列 ratelimit 结局（正常竞争限流，不算节点失败）。
			ve0 := asVertexError(err)
			isRateLimit := ve0 != nil && (ve0.Kind == "ratelimit" || ve0.Code == 429)
			if isRateLimit {
				stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, proxy), proxy, "ratelimit", time.Since(attemptStart).Milliseconds())
			} else {
				stats.RecordRaceOutcome(RequestIDFromContext(ctx), nodeNameOrURI(pool, proxy), proxy, "error", time.Since(attemptStart).Milliseconds())
			}
			applyNodeFailure(pool, proxy, err)
			ve := ve0
			// 429 即收敛：同 URI 背靠背重试只会加剧上游限流，立即返回 429 让客户端退避。
			// 签名缺失同理（同 URI 换 token 重取无意义，属窗口路径补位的职责）。
			// 判据用 Kind 精确匹配；不得用 Code==404——那会把普通 404
			//（notfound/client，应继续走 failFast 终局）一并提前收敛。
			// 限流失败不做节点归属（RecordNode 是"服务记录"，会把限流请求
			// 计入该节点的服务统计）。
			if ve != nil && (ve.Kind == "ratelimit" || ve.Code == 429 || ve.Kind == "notsignature") {
				if ve.Kind == "notsignature" && proxy != "" {
					stats.RecordNode(RequestIDFromContext(ctx), nodeNameOrURI(pool, proxy), proxy)
				}
				return zero, ve
			}
			if failFast && ve != nil && ve.ClassifyBatch() == FailFast {
				// 硬错误终局归属当前尝试节点。
				if proxy != "" {
					stats.RecordNode(RequestIDFromContext(ctx), nodeNameOrURI(pool, proxy), proxy)
				}
				return zero, ve
			}
			if cfg.DebugMode() {
				log.Printf("[Racing] 单节点第 %d/%d 次尝试失败，立即接力: %s", round, attempts, err.Error())
			}
		}
		bestErr = pickBestError([]error{bestErr, err})
	}
	// 直连失败收敛：把请求归属到所尝试的节点（父 ctx 已取消或未配置节点时保留未归属）。
	if proxy != "" && ctx.Err() == nil {
		stats.RecordNode(RequestIDFromContext(ctx), nodeNameOrURI(pool, proxy), proxy)
	}
	return zero, NormalizeError(bestErr)
}
