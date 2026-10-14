package nodestore

import (
	"sort"
	"strings"
	"sync"
)

// 本文件是"故障处理策略"的定义与解析内核（纯逻辑，无持久化；DB 读写见
// failure_policy_store.go）。设计依据：Issue #5「策略可配置化：实施计划基准」。
//
// 核心约定（评审已定稿）：
//   - 每个故障类别一条策略，动作三选一：record_only / single_shot / disable_then_probe；
//   - **首次移出可用池由策略驱动**，不受任何全局阈值门控；
//   - 探测失败阈值 N 只作用于探测阶段：N>0 连续探测失败达 N 次转永久禁用；N=0 无限探测；
//   - 内置默认表是唯一默认来源；DB 里只存"与默认不同"的自定义行（稀疏覆盖），
//     因此"重置"就是删行，未知/未来新增类别天然落默认、不会异常。

// PolicyAction 是一个故障类别被匹配后采取的动作。
type PolicyAction string

const (
	// PolicyRecordOnly 只记录：不移出可用池、不进入自动探测。
	PolicyRecordOnly PolicyAction = "record_only"
	// PolicySingleShot 单次禁用：首次匹配即移出可用池，**不自动恢复**，只能人工启用。
	PolicySingleShot PolicyAction = "single_shot"
	// PolicyDisableThenProbe 禁用后恢复探测：首次匹配即移出可用池并进入自动探测；
	// 探测成功恢复，连续失败达 N 次转永久禁用（N=0 时无限探测、永不因探测失败转永久）。
	PolicyDisableThenProbe PolicyAction = "disable_then_probe"
)

// 探测失败阈值的两个特殊取值。
const (
	// ProbeThresholdFollowGlobal 表示该类别没有自定义探测失败阈值，跟随全局配置
	// health_probe_fail_threshold。DB 列默认值即此值。
	ProbeThresholdFollowGlobal = -1
	// ProbeThresholdUnlimited 表示无限探测：不因探测失败转永久（探测成功仍照常恢复）。
	ProbeThresholdUnlimited = 0
)

// DefaultProbeFailThreshold 是探测失败阈值的全局默认值（配置项
// health_probe_fail_threshold 缺失时的兜底）。默认 2：一次探测失败多为抖动，
// 两次才判定"持续不可恢复"。
const DefaultProbeFailThreshold = 2

// PolicyActionValid 报告字符串是否为合法动作（PUT 校验与 DB 行合法性检查共用）。
func PolicyActionValid(a string) bool {
	switch PolicyAction(a) {
	case PolicyRecordOnly, PolicySingleShot, PolicyDisableThenProbe:
		return true
	}
	return false
}

// Action 的中文标签（面板与日志共用，避免多处各写一份文案）。
func PolicyActionLabel(a PolicyAction) string {
	switch a {
	case PolicyRecordOnly:
		return "不禁用"
	case PolicySingleShot:
		return "单次禁用"
	case PolicyDisableThenProbe:
		return "禁用后恢复探测"
	}
	return string(a)
}

// FaultPolicy 是一个故障类别的策略（解析后的完整形态：动作 + 已解析的阈值）。
type FaultPolicy struct {
	Class  FailureClass `json:"class"`
	Action PolicyAction `json:"action"`
	// ProbeFailThreshold 是已解析的探测失败阈值：0=无限，>0=连败 N 次转永久。
	// 仅在 Action == PolicyDisableThenProbe 时参与判定。
	ProbeFailThreshold int `json:"probe_fail_threshold"`
	// ThresholdFollowsGlobal 报告该阈值是否来自全局配置（面板展示"来源：默认/自定义"）。
	ThresholdFollowsGlobal bool `json:"threshold_follows_global"`
	// ActionIsDefault 报告动作是否等于内置默认（false = 用户自定义过）。
	ActionIsDefault bool `json:"action_is_default"`
}

// DisablesOnFirstFailure 报告"首次匹配的可归因失败即移出可用池"。
// 两个禁用档都满足；只有 record_only 不摘出。
func (p FaultPolicy) DisablesOnFirstFailure() bool {
	return p.Action == PolicySingleShot || p.Action == PolicyDisableThenProbe
}

// ProbesForRecovery 报告该类别的禁用是否进入自动探测（即可被探测成功恢复）。
func (p FaultPolicy) ProbesForRecovery() bool {
	return p.Action == PolicyDisableThenProbe
}

// builtinPolicyActions 是内置默认动作表（唯一默认来源）。
//
// 取值依据（Issue #5「策略可配置化：实施计划基准」§2 + 06:55 评审结论）：
//   - node_network / permission(403) / auth：禁用后恢复探测。
//     403 与认证失败必须进入自动恢复流程，"一次即禁 + 可恢复"是已确认的产品策略；
//     全局性的账号/API 配置问题由全局故障保护识别（见 GuardsAgainstGlobalFault）。
//   - other：不禁用。它的定义就是"无法归因"，对未知成因自动摘出误判率最高；
//     且分类器已把 wsarecv 等明确信号收进 node_network，留在 other 的正是最不该自动处置的。
//   - dns：不禁用（DNS 走本机/公共解析链路，单节点解析失败不足以判定节点失效）。
//   - 其余（ratelimit/upstream/timeout/probe_flow/unsupported）：不禁用，
//     各自走冷却、降权或 admin 测试通道处理。
var builtinPolicyActions = map[FailureClass]PolicyAction{
	FailureNodeNetwork: PolicyDisableThenProbe,
	FailurePermission:  PolicyDisableThenProbe,
	FailureAuth:        PolicyDisableThenProbe,
	FailureDNS:         PolicyRecordOnly,
	FailureTimeout:     PolicyRecordOnly,
	FailureRateLimit:   PolicyRecordOnly,
	FailureUpstream5xx: PolicyRecordOnly,
	FailureProbeFlow:   PolicyRecordOnly,
	FailureUnsupported: PolicyRecordOnly,
	FailureUnknown:     PolicyRecordOnly,
}

// BuiltinPolicyAction 返回某类别的内置默认动作；未知类别返回"不禁用"——
// 未来新增分类在没有任何配置时绝不会凭空获得自动禁用能力。
func BuiltinPolicyAction(class FailureClass) PolicyAction {
	if a, ok := builtinPolicyActions[class]; ok {
		return a
	}
	return PolicyRecordOnly
}

// KnownFailureClasses 返回全部内置类别（面板列出类别、以及"未来新增分类自动出现"的依据）。
func KnownFailureClasses() []FailureClass {
	out := make([]FailureClass, 0, len(builtinPolicyActions))
	for c := range builtinPolicyActions {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PolicySet 是"类别 → 生效策略"的解析器：内置默认 + 用户自定义覆盖。
//
// 自定义行很少变化（用户改配置时），但会被并发读取（每个失败上报都要查策略），
// 故用 RWMutex 保护；Replace 在 DB 载入与 PUT 之后调用。
type PolicySet struct {
	mu sync.RWMutex
	// custom 只含"用户显式设置过"的类别；缺行即回落内置默认。
	custom map[FailureClass]customPolicy
	// globalThreshold 返回全局探测失败阈值的当前值（热读配置，含钳制后的合法值）。
	globalThreshold func() int
}

// customPolicy 是自定义行的原始形态：动作 + 原始阈值
// （ProbeThresholdFollowGlobal 表示未自定义阈值，跟随全局）。
type customPolicy struct {
	action    PolicyAction
	threshold int
}

// NewPolicySet 构造解析器。globalThreshold 为 nil 时使用 DefaultProbeFailThreshold。
func NewPolicySet(globalThreshold func() int) *PolicySet {
	s := &PolicySet{custom: make(map[FailureClass]customPolicy)}
	s.globalThreshold = globalThreshold
	return s
}

// defaultPolicySet 是无自定义配置时的进程级兜底：节点池未注入 PolicySet 时（测试装配、
// 仅前置池等场景）行为与"全默认策略"完全一致。
//
//nolint:gochecknoglobals // 不可变兜底实例
var defaultPolicySet = NewPolicySet(nil)

// DefaultPolicySet 返回进程级默认策略集（只读用途；节点池可复用它）。
func DefaultPolicySet() *PolicySet {
	return defaultPolicySet
}

// Replace 用 DB 中的自定义行整体替换当前覆盖集（DB 载入 / PUT / 重置后调用）。
// 非法行（动作不合法、阈值 < -1）一律**丢弃并回落默认**，不静默按可疑值执行。
func (s *PolicySet) Replace(rows []FaultPolicy) {
	next := make(map[FailureClass]customPolicy, len(rows))
	for _, r := range rows {
		if !PolicyActionValid(string(r.Action)) {
			continue
		}
		if r.ProbeFailThreshold < ProbeThresholdFollowGlobal {
			continue
		}
		next[r.Class] = customPolicy{action: r.Action, threshold: r.ProbeFailThreshold}
	}
	s.mu.Lock()
	s.custom = next
	s.mu.Unlock()
}

// Customizations 返回当前自定义行的原始形态（供面板展示"来源：自定义"与管理接口回显）。
func (s *PolicySet) Customizations() map[FailureClass]FaultPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[FailureClass]FaultPolicy, len(s.custom))
	for class, c := range s.custom {
		out[class] = FaultPolicy{
			Class:                  class,
			Action:                 c.action,
			ProbeFailThreshold:     c.threshold,
			ThresholdFollowsGlobal: c.threshold == ProbeThresholdFollowGlobal,
		}
	}
	return out
}

// For 解析某类别的生效策略：自定义动作优先，阈值按"自定义 > 全局"顺序解析。
// 未知类别（未来新增分类）回落 record_only + 全局阈值，不会异常。
func (s *PolicySet) For(class FailureClass) FaultPolicy {
	s.mu.RLock()
	c, hasCustom := s.custom[class]
	s.mu.RUnlock()

	action := BuiltinPolicyAction(class)
	followGlobal := true
	threshold := ProbeThresholdFollowGlobal
	if hasCustom {
		action = c.action
		threshold = c.threshold
		followGlobal = c.threshold == ProbeThresholdFollowGlobal
	}

	resolved := threshold
	if followGlobal {
		resolved = s.globalProbeThreshold()
	}
	// 只有"禁用后恢复探测"用到阈值；其余动作统一上报为全局默认值，
	// 避免面板出现"不禁用却显示探测阈值 7"这类无意义组合。
	if action != PolicyDisableThenProbe {
		resolved = 0
		followGlobal = true
	}

	return FaultPolicy{
		Class:                  class,
		Action:                 action,
		ProbeFailThreshold:     resolved,
		ThresholdFollowsGlobal: followGlobal,
		ActionIsDefault:        action == BuiltinPolicyAction(class),
	}
}

// GlobalThreshold 返回当前的全局探测失败阈值（0 = 无限探测）。
// 面板展示"跟随全局"时要用具体数值，故单独暴露。
func (s *PolicySet) GlobalThreshold() int {
	return s.globalProbeThreshold()
}

// All 返回全部内置类别的生效策略（面板列表用，顺序稳定）。
func (s *PolicySet) All() []FaultPolicy {
	classes := KnownFailureClasses()
	out := make([]FaultPolicy, 0, len(classes))
	for _, c := range classes {
		out = append(out, s.For(c))
	}
	return out
}

// globalProbeThreshold 读取全局探测失败阈值（负值/异常回落默认；0 合法 = 无限探测）。
func (s *PolicySet) globalProbeThreshold() int {
	if s.globalThreshold == nil {
		return DefaultProbeFailThreshold
	}
	v := s.globalThreshold()
	if v < 0 {
		return DefaultProbeFailThreshold
	}
	return v
}

// PolicyActionFromString 把接口/DB 传入的字符串规整为动作（大小写与空白容错）。
// 返回 false 表示非法，调用方应拒绝写入而不是静默接受。
func PolicyActionFromString(s string) (PolicyAction, bool) {
	a := PolicyAction(strings.ToLower(strings.TrimSpace(s)))
	return a, PolicyActionValid(string(a))
}
