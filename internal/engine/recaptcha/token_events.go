package recaptcha

import (
	"context"
	"errors"
	"sync"
	"time"
)

// rT token 抓取的来源标签（实时记录流展示用）。
const (
	// TokenSourceWarm 预取池命中：直接从池内取用，关键路径上没有抓取耗时。
	TokenSourceWarm = "池命中"
	// TokenSourceLive 请求路径的实时抓取（入口/直连，或候选节点轮询）。
	TokenSourceLive = "实时抓取"
	// TokenSourcePrefetch 后台预取池的补货抓取（没有请求在等它）。
	TokenSourcePrefetch = "预取"
	// tokenNodeEntry 是入口/直连路径的展示名：该路径没有可归属的出口节点。
	tokenNodeEntry = "入口/直连"
)

// tokenEventRingSize 是实时记录流的环形缓冲容量。竞速下一次请求会取十几份 token
// （每个候选各一份），故容量取"最近若干次请求"的量级；超出即丢弃最旧的。
const tokenEventRingSize = 100

// TokenEvent 是一次 rT token 抓取的事件记录，供 TUI 面板与管理页实时记录流展示。
type TokenEvent struct {
	// Seq 单调递增，供展示端排序与去重。
	Seq       int64     `json:"seq"`
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	NodeURI   string    `json:"node_uri,omitempty"`
	NodeName  string    `json:"node_name,omitempty"`
	LatencyMs int64     `json:"latency_ms"`
	OK        bool      `json:"ok"`
	Err       string    `json:"err,omitempty"`
	// Canceled 标记"抓取被 ctx 取消"（竞速其它候选胜出 / 请求中断 / 客户端断开）：
	// 这不是失败，与节点无关，展示端应中性呈现，不要与真超时混为一谈。
	Canceled bool `json:"canceled,omitempty"`
	// RTRank 是**选点时刻**该节点的 rT 降权层：0 健康 / 1 对 rT 差或慢 / 2 rT 冷却中。
	// 它解释了"为什么这条记录用/没用这个节点"——排后降权的节点只会在好节点不足时出现。
	RTRank int `json:"rt_rank,omitempty"`
}

// tokenEventRing 是固定容量的抓取事件环形缓冲（并发安全；零值即可用）。
type tokenEventRing struct {
	mu     sync.Mutex
	events []TokenEvent
	seq    int64
}

func (r *tokenEventRing) record(ev TokenEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	ev.Seq = r.seq
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.events = append(r.events, ev)
	if len(r.events) > tokenEventRingSize {
		// 整体前移而不是原地切片：避免底层数组随长期运行不断增长。
		drop := len(r.events) - tokenEventRingSize
		r.events = append(r.events[:0:0], r.events[drop:]...)
	}
}

// recent 返回最新的 limit 条（newest first）；limit<=0 表示全部。
func (r *tokenEventRing) recent(limit int) []TokenEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.events) {
		limit = len(r.events)
	}
	out := make([]TokenEvent, 0, limit)
	for i := len(r.events) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.events[i])
	}
	return out
}

// RecentTokenEvents 返回最近的 rT 抓取事件（newest first，至多 limit 条）。
// 记录流独立于预取开关：预取关闭时请求路径照样抓 token，记录照常产生。
func (p *TokenPool) RecentTokenEvents(limit int) []TokenEvent {
	if p == nil {
		return nil
	}
	return p.events.recent(limit)
}

// recordTokenEvent 记录一次抓取事件。res 携带节点归属（空=入口/直连路径）、是否"压根没有
// 可尝试的路径"（noPath：禁止直连且无可用候选，此时节点留空、不误标成"入口/直连"）、
// 选点时刻的 rT 降权层与错误。池命中不携带节点（池内 token 的来源在它被抓取那一刻已单独记过）。
func (p *TokenPool) recordTokenEvent(source string, res tokenFetchResult, latency time.Duration) {
	if p == nil {
		return
	}
	ev := TokenEvent{
		Source:    source,
		NodeURI:   res.node,
		LatencyMs: latency.Milliseconds(),
		OK:        res.err == nil,
		RTRank:    res.rtRank,
	}
	if res.err != nil {
		ev.Err = res.err.Error()
		ev.Canceled = errors.Is(res.err, context.Canceled)
	}
	if source != TokenSourceWarm && !res.noPath {
		ev.NodeName = p.nodeDisplayName(res.node)
	}
	p.events.record(ev)
}

// nodeDisplayName 把节点 URI 映射为展示名；空 uri 即入口/直连路径。
func (p *TokenPool) nodeDisplayName(uri string) string {
	if uri == "" {
		return tokenNodeEntry
	}
	if p.source != nil {
		if name := p.source.NodeName(uri); name != "" {
			return name
		}
	}
	return uri
}
