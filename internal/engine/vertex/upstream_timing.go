package vertex

import (
	"context"
	"sync/atomic"
	"time"
)

// UpstreamTiming 采集"上游首字节"时刻。
//
// 背景：内部与 Vertex 上游的连接恒为流式（executeStreamingAttempt 是唯一通路，
// 非流式的 CompleteChat 内部同样流式收包后再聚合）。故上游侧永远可以按首字节计时，
// 与客户端侧看到的形态（逐帧 / 聚合 / 单次）无关。
// 客户端侧的现有 TTFB 锚在"写出首个 SSE 帧"上，中间夹着竞速等待、解包、序列化等
// 全部代理开销；两者之差即代理自身处理耗时。
//
// 同一类型有两种用法，靠挂载的 context 层级区分：
//   - 候选级：由竞速的每个候选各自持有一个，记录该候选的首字节；
//   - 请求级：由 api 层挂载，记录"首个产出有效内容的候选"的首字节（即胜出候选）。
//
// 必须区分到胜出候选：竞速下某候选可能很快读到字节却始终产不出有效内容而落败，
// 若直接取"全候选最早首字节"会给出偏乐观的假象。
type UpstreamTiming struct {
	// firstByteUnixNano 是候选级用法下的首字节时刻（UnixNano），0 表示未采集到。
	firstByteUnixNano atomic.Int64
	// winnerUnixNano 是请求级用法下胜出候选的首字节时刻，0 表示未采集到。
	winnerUnixNano atomic.Int64
	// onWinner 是请求级胜出值首次落定后的即时通知回调（可为 nil）。
	// 由 api 层在开跑前注册，用于"上游首字节一到就推首字"，而不必等到整包收完
	// （非流式/聚合流下收尾推送会让首字拖到数据传输结束才出现）。
	//
	// 只在开跑前写入一次、之后仅被读，故无需额外同步。
	onWinner func()
}

// NewUpstreamTiming 构造一个空时序采集器。
func NewUpstreamTiming() *UpstreamTiming { return &UpstreamTiming{} }

type upstreamTimingKey struct{}

// WithUpstreamTiming 把时序采集器挂进 context。
// 同一 key 可被候选级覆盖：竞速闭包用它把候选级采集器替换为自己的实例。
func WithUpstreamTiming(ctx context.Context, t *UpstreamTiming) context.Context {
	if ctx == nil || t == nil {
		return ctx
	}
	return context.WithValue(ctx, upstreamTimingKey{}, t)
}

// TimingFromContext 取出当前层级的时序采集器；未挂载返回 nil（各标记方法对 nil 安全）。
func TimingFromContext(ctx context.Context) *UpstreamTiming {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(upstreamTimingKey{}).(*UpstreamTiming)
	return t
}

// MarkFirstByte 记录候选级首字节时刻。仅首次生效（竞速下并发调用安全）。
func (t *UpstreamTiming) MarkFirstByte(nano int64) {
	if t == nil || nano <= 0 {
		return
	}
	t.firstByteUnixNano.CompareAndSwap(0, nano)
}

// MarkWinner 把候选的首字节时刻记为请求级胜出值。仅首次生效：
// 竞速下多个候选都可能产出有效内容，只有第一个（即胜出者）应被采纳。
// 胜出值首次落定时回调 onWinner（若已注册），供调用方即时获知"首字已就绪"。
func (t *UpstreamTiming) MarkWinner(candidate *UpstreamTiming) {
	if t == nil || candidate == nil {
		return
	}
	nano := candidate.firstByteUnixNano.Load()
	if nano <= 0 {
		return
	}
	if t.winnerUnixNano.CompareAndSwap(0, nano) && t.onWinner != nil {
		t.onWinner()
	}
}

// SetOnWinner 注册"上游首字节就绪"回调。必须在竞速开跑前调用（只写不读竞争）。
func (t *UpstreamTiming) SetOnWinner(fn func()) {
	if t == nil {
		return
	}
	t.onWinner = fn
}

// FirstByteNano 返回采集到的首字节时刻（UnixNano）；0 表示未采集到。
func (t *UpstreamTiming) FirstByteNano() int64 {
	if t == nil {
		return 0
	}
	return t.firstByteUnixNano.Load()
}

// UpstreamTTFB 计算自 start 起至上游首字节的耗时；未采集到返回 0。
func (t *UpstreamTiming) UpstreamTTFB(start time.Time) time.Duration {
	nano := t.winnerNano()
	if nano <= 0 {
		return 0
	}
	at := time.Unix(0, nano)
	d := at.Sub(start)
	if d < 0 {
		return 0
	}
	return d
}

// winnerNano 返回请求级胜出值（候选级实例上该值恒为 0）。
func (t *UpstreamTiming) winnerNano() int64 {
	if t == nil {
		return 0
	}
	return t.winnerUnixNano.Load()
}
