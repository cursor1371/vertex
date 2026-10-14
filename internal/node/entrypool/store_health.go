package entrypool

import (
	"log"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// ---- 前置节点健康度记录与可选筛选 ----

// entryFailCooldownSec 是前置节点测速失败后的冷却秒数。
// 冷却期内 SelectableNodes 会跳过该节点，实现透明降级。
const entryFailCooldownSec = 60

func (m *EntryManager) updateSingleEntryDisabledUnsafe(uri string, disabled bool) {
	// 同行写 disabled_by_health 与审计四列：调用方在修改 entryHealthMap 标记之后才走到这里，
	// 按当前内存态落库，保证"健康禁用可自动恢复 / 人工禁用不自动恢复"重启后仍成立，
	// 且面板能看出"谁禁的、为什么禁"。
	byHealth := false
	if h, ok := m.entryHealthMap[uri]; ok {
		byHealth = h.DisabledByHealth
	}
	reason, source, class, disabledAt := "", nodestore.DisableSourceManual, "", int64(0)
	for i := range m.entryList {
		if m.entryList[i].RawURI == uri {
			reason = m.entryList[i].DisableReason
			source = m.entryList[i].DisableSource
			class = m.entryList[i].DisableFailureClass
			disabledAt = m.entryList[i].DisabledAt
			break
		}
	}
	_ = nodestore.UpsertDisabled(m.database, "entry_nodes", uri, nodestore.DisableState{
		Disabled:         disabled,
		DisabledByHealth: byHealth,
		Source:           source,
		Reason:           reason,
		DisabledAt:       disabledAt,
		FailureClass:     class,
	})
}

func (m *EntryManager) saveSingleEntryHealthUnsafe(uri string, h *NodeHealth) {
	if h == nil {
		return
	}
	_ = nodestore.UpsertHealth(m.database, "entry_node_health", nodestore.HealthRow{
		RawURI:              uri,
		SuccessCount:        h.SuccessCount,
		FailCount:           h.FailCount,
		ConsecutiveFailures: h.ConsecutiveFailures,
		LastTestMs:          h.LastTestMs,
		LastTestError:       h.LastTestError,
		LastSuccessAt:       h.LastSuccessAt,
		LastFailAt:          h.LastFailAt,
		CooldownUntil:       h.CooldownUntil,
		LastTestAt:          h.LastTestAt,
		LastFailureClass:    h.LastFailureClass,
		LastHTTPStatus:      h.LastHTTPStatus,
		AutoProbeFailStreak: h.AutoProbeFailStreak,
	})
}

// RecordTest 记录一次前置代理 204 连通性测试结果并落库（兼容入口，等价于
// ReportProbe 且来源为 request）。
//
// 失败时进入短暂冷却；只有**节点级连通性故障**（节点池共享分类器，见 failure_class.go）
// 连续多次（门槛可配置 health_disable_threshold）才自动禁用，未达门槛按冷却降级；
// DNS 解析失败、403、5xx、超时一律只记录不禁用。成功后自动解除健康触发的禁用
// （人工禁用与 admin 测试禁用的节点不受影响）。
func (m *EntryManager) RecordTest(uri string, ok bool, ms float64, errStr string) {
	m.ReportProbe(uri, ProbeResult{OK: ok, Ms: ms, ErrStr: errStr, Source: nodestore.ProbeSourceRequest})
}

// ProbeResult 是一次前置节点探测/失败上报的入参（与出口池同构）。
type ProbeResult struct {
	OK     bool
	Ms     float64
	ErrStr string
	Class  nodestore.FailureClass
	Target string
	Source string
}

// ReportProbe 是前置池的统一上报入口：分类 → 记账 → 全局故障保护 → 阈值禁用 → 故障记录。
// 返回本次是否真的触发了禁用。
func (m *EntryManager) ReportProbe(uri string, r ProbeResult) (disabled bool) {
	if uri == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	h, exists := m.entryHealthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		m.entryHealthMap[uri] = h
	}
	now := time.Now().Unix()
	h.LastTestMs = r.Ms
	// 与出口池同口径：落库与面板展示前统一脱敏（保留域名/状态码/错误类型等定位信息）。
	h.LastTestError = nodestore.MaskSecrets(r.ErrStr)
	h.LastTestAt = now

	if r.OK {
		h.SuccessCount++
		h.ConsecutiveFailures = 0
		h.LastSuccessAt = now
		h.CooldownUntil = 0
		h.AutoProbeFailStreak = 0
		h.LastFailureClass = ""
		h.LastHTTPStatus = 0
		// 自动恢复仅限来源为 health 的禁用；人工/admin 测试/协议不支持不受影响。
		idx := m.entryIndexByURISeek(uri)
		if idx >= 0 && m.entryList[idx].Disabled && nodestore.AutoRecoverable(m.entryList[idx].DisableSource) {
			m.entryList[idx].Disabled = false
			m.clearEntryDisableAuditUnsafe(idx)
			h.DisabledByHealth = false
			m.updateSingleEntryDisabledUnsafe(uri, false)
			log.Printf("[Health] 前置节点 %s 测速成功，自动解除健康触发的禁用", uri)
		}
		m.saveSingleEntryHealthUnsafe(uri, h)
		return false
	}

	info := nodestore.ClassifyFailure(r.ErrStr)
	if r.Class != "" {
		info.Class = r.Class
	}
	h.FailCount++
	h.ConsecutiveFailures++
	h.LastFailAt = now
	h.LastFailureClass = string(info.Class)
	h.LastHTTPStatus = info.Status
	h.CooldownUntil = now + entryFailCooldownSec

	// 故障记录（脱敏原文 + 聚合签名）。前置池尚无自动探测通道，记录是唯一的可查线索。
	if m.database != nil {
		_ = nodestore.RecordFault(m.database, nodestore.FaultRow{
			Scope:        "entry",
			NodeURI:      uri,
			NodeName:     m.entryNodeName(uri),
			FailureClass: string(info.Class),
			HTTPStatus:   info.Status,
			ErrorCode:    info.Code,
			ErrorSig:     info.Sig,
			RawError:     info.Detail,
			Target:       r.Target,
			Source:       r.Source,
			FirstSeenAt:  now,
			LastSeenAt:   now,
			Count:        1,
		})
	}

	pol := m.policyFor(info.Class)

	// 全局故障保护：语义锁死为"只防、不治"——命中只阻止后续节点被摘除，不恢复已禁用节点。
	globalFault := false
	if info.Class.GuardsAgainstGlobalFault() && m.globalFault != nil {
		globalFault = m.globalFault.Observe(info.Class, uri, now)
	}

	// 首次移出可用池由**策略**决定（与出口池同一张策略表），不受阈值门控。
	// 前置池第一版**不新增自动探测协程**，因此 disable_then_probe 的恢复通道只有
	// "用户触发的测速成功"与"人工启用"（既有行为，保持不变）——UI 会在策略页明确提示
	// "前置池暂不自动探测"，避免用户以为它会像出口节点一样到点自动回来。
	// 防删锁不豁免（与出口池一致）。
	if pol.DisablesOnFirstFailure() && !globalFault {
		idx := m.entryIndexByURISeek(uri)
		if idx >= 0 {
			if m.entryList[idx].Locked {
				log.Printf("[Health] 前置节点 %s 已防删锁定，但策略判定失败仍按规则禁用", uri)
			}
			if m.entryList[idx].Disabled {
				// 不回溯：已禁用节点（任何来源）不被本路径改写状态与来源。
			} else {
				reason := "命中策略「" + nodestore.PolicyActionLabel(pol.Action) + "」（" + string(info.Class) + "）"
				if pol.ProbesForRecovery() {
					reason += "；前置池不执行自动探测，靠测速成功或人工启用恢复"
				}
				if info.Code != "" {
					reason += "（错误码 " + info.Code + "）"
				}
				if info.Detail != "" {
					reason += "：" + info.Detail
				}
				source := nodestore.DisableSourceSingleShot
				if pol.ProbesForRecovery() {
					source = nodestore.DisableSourceHealth
				}
				m.entryList[idx].Disabled = true
				m.entryList[idx].DisableSource = source
				m.entryList[idx].DisableReason = reason
				m.entryList[idx].DisabledAt = now
				m.entryList[idx].DisableFailureClass = string(info.Class)
				h.DisabledByHealth = nodestore.AutoRecoverable(source)
				m.updateSingleEntryDisabledUnsafe(uri, true)
				disabled = true
				log.Printf("[Health] 前置节点 %s 命中策略「%s」，已移出可用池（来源 %s）",
					uri, nodestore.PolicyActionLabel(pol.Action), source)
			}
		}
	}
	m.saveSingleEntryHealthUnsafe(uri, h)
	return disabled
}

// entryIndexByURISeek 返回 uri 在 entryList 中的下标（调用方须持锁）；找不到返回 -1。
func (m *EntryManager) entryIndexByURISeek(uri string) int {
	for i := range m.entryList {
		if m.entryList[i].RawURI == uri {
			return i
		}
	}
	return -1
}

// entryNodeName 返回前置节点展示名（仅用于故障记录，找不到时留空）。
func (m *EntryManager) entryNodeName(uri string) string {
	if idx := m.entryIndexByURISeek(uri); idx >= 0 {
		return m.entryList[idx].Name
	}
	return ""
}

// clearEntryDisableAuditUnsafe 清空启用节点的禁用审计（启用后不再有"为何被禁"）。
func (m *EntryManager) clearEntryDisableAuditUnsafe(idx int) {
	m.entryList[idx].DisableSource = nodestore.DisableSourceManual
	m.entryList[idx].DisableReason = ""
	m.entryList[idx].DisabledAt = 0
	m.entryList[idx].DisableFailureClass = ""
}

// GlobalFaultActive 返回当前疑似全局故障的类别与窗口内节点数（供面板展示）。
func (m *EntryManager) GlobalFaultActive() map[string]int {
	if m.globalFault == nil {
		return map[string]int{}
	}
	return m.globalFault.Active(time.Now().Unix())
}

// SetGlobalFaultGuard 注入全局故障保护器（装配期调用；为 nil 时该项保护停用）。
func (m *EntryManager) SetGlobalFaultGuard(g *nodestore.GlobalFaultGuard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.globalFault = g
}

// SelectableNodes 返回可选前置节点：未禁用且不在冷却期。
func (m *EntryManager) SelectableNodes() []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	now := time.Now().Unix()
	var out []Node
	for _, n := range m.entryList {
		if n.Disabled {
			continue
		}
		if h, exists := m.entryHealthMap[n.RawURI]; exists && h.CooldownUntil > now {
			continue
		}
		if m.identity != nil && !m.identity.Supported(n.RawURI) {
			continue
		}
		out = append(out, n)
	}
	return out
}
