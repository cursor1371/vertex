package nodestore

import "sync"

// 全局故障保护：短时间内大量**不同节点**出现同一类失败时，判定为疑似公共依赖/环境故障，
// 暂停该类别的自动禁用。
//
// 为什么必须存在：节点级失败与全局故障在单节点视角下无法区分。本 Issue 原文里的
// `lookup ct.cloudflare.byoip.top: NXDOMAIN` 就是典型——若每个节点各自"连败达阈值"就禁用，
// 一次本地 DNS 故障或上游公共依赖抖动会在几秒内把整个节点池逐个禁掉，而节点本身毫无问题。
// 保护器把"多个不同节点同因失败"作为全局故障信号，宁可暂时不禁用（后续失败仍会继续记账，
// 窗口过后照常判定），也不误杀整池。
//
// 只作用于 FailureClass.GuardsAgainstGlobalFault() 为真的类别（当前仅节点级连通性故障）。

// GlobalFaultGuard 是全局故障的滑动窗口观测器。零值不可用，须经 NewGlobalFaultGuard 构造。
type GlobalFaultGuard struct {
	mu sync.Mutex
	// cfg 每次调用重新读取开关与参数（热更新：面板改完下一轮判定即生效）。
	// 返回 false 时整个保护器停用（等价"永远不判定全局故障"）。
	cfg func() (enabled bool, windowSec, minNodes int)
	// seen 记录每个失败类别下"最近一次失败时间"按节点的分布，用于统计窗口内的不同节点数。
	seen map[FailureClass]map[string]int64
}

// NewGlobalFaultGuard 构造保护器。cfg 为 nil 时保护器始终不生效（等价关闭），
// 便于无配置的测试装配与"不关心该保护"的调用方。
func NewGlobalFaultGuard(cfg func() (enabled bool, windowSec, minNodes int)) *GlobalFaultGuard {
	return &GlobalFaultGuard{cfg: cfg, seen: make(map[FailureClass]map[string]int64)}
}

// Observe 记录一次失败，并返回该类别当前是否处于"疑似全局故障"状态。
// 处于该状态时调用方应暂停自动禁用（记账照常）。
func (g *GlobalFaultGuard) Observe(class FailureClass, uri string, now int64) bool {
	if g == nil || g.cfg == nil || uri == "" {
		return false
	}
	enabled, windowSec, minNodes := g.cfg()
	if !enabled {
		return false
	}
	if windowSec <= 0 {
		windowSec = 120
	}
	if minNodes < 2 {
		minNodes = 2
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.seen[class]
	if m == nil {
		m = make(map[string]int64)
		g.seen[class] = m
	}
	m[uri] = now
	// 顺手清理窗口外的节点，避免 map 无界增长（节点池规模有限，这里只做兜底）。
	cutoff := now - int64(windowSec)
	for u, ts := range m {
		if ts < cutoff {
			delete(m, u)
		}
	}
	return len(m) >= minNodes
}

// Active 返回当前处于疑似全局故障的类别及其窗口内节点数（供面板展示）。
// windowSec 取调用时的配置值。
func (g *GlobalFaultGuard) Active(now int64) map[string]int {
	out := make(map[string]int)
	if g == nil || g.cfg == nil {
		return out
	}
	enabled, windowSec, minNodes := g.cfg()
	if !enabled {
		return out
	}
	if windowSec <= 0 {
		windowSec = 120
	}
	if minNodes < 2 {
		minNodes = 2
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := now - int64(windowSec)
	for class, m := range g.seen {
		n := 0
		for _, ts := range m {
			if ts >= cutoff {
				n++
			}
		}
		if n >= minNodes {
			out[string(class)] = n
		}
	}
	return out
}
