// Package entrypool 管理前置代理（Entry Proxy）节点池。
//
// 与出口竞速节点池（internal/node/exitpool）物理解耦：前置代理仅用于第一跳 SOCKS5
// 回环通道，SQLite 持久化到独立的 entry_nodes / entry_node_health 表，
// 不影响现有竞速引擎的节点选择逻辑。
package entrypool

import (
	"database/sql"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// Node 是前置代理节点的内存表示，字段与 entry_nodes 表一一对应。
type Node struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	RawURI   string `json:"raw_uri"`
	Disabled bool   `json:"disabled"`
	// Locked 为防删锁：置位后该节点不会被删除、去重移除或批量手动禁用。
	// 健康触发的自动禁用不受保护（节点实际连不通时仍会被摘出轮换，可手动启用恢复）。
	Locked bool `json:"locked"`
	// 禁用审计四列（语义与出口池一致，见 nodestore.DisableSource*）。
	DisableSource       string `json:"disable_source"`
	DisableReason       string `json:"disable_reason"`
	DisabledAt          int64  `json:"disabled_at"`
	DisableFailureClass string `json:"disable_failure_class"`
}

// NodeHealth 是前置代理节点的健康度快照，字段与 entry_node_health 表一一对应。
type NodeHealth struct { //nolint:govet
	SuccessCount        int     `json:"success_count"`
	FailCount           int     `json:"fail_count"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastTestMs          float64 `json:"last_test_ms"`
	LastTestError       string  `json:"last_test_error"`
	LastSuccessAt       int64   `json:"last_success_at"`
	LastFailAt          int64   `json:"last_fail_at"`
	CooldownUntil       int64   `json:"cooldown_until"`
	// LastTestAt / LastFailureClass / LastHTTPStatus 与出口池同义：最近检查时间、
	// 最近失败归因类别、最近失败的 HTTP 状态码。
	LastTestAt          int64  `json:"last_test_at"`
	LastFailureClass    string `json:"last_failure_class"`
	LastHTTPStatus      int    `json:"last_http_status"`
	AutoProbeFailStreak int    `json:"auto_probe_fail_streak"`
	// DisabledByHealth 标记 Disabled 由健康判定触发（内存态，重启归零）：
	// RecordTest 成功路径据此自动解除禁用；人工禁用（BatchUpdateNodesDisabled 会清本标记）
	// 永远不会被自动放开。与出口池（exitpool）语义对齐。
	DisabledByHealth bool `json:"-"`
}

// IdentityResolver 由外部注入（transport.NewIdentityResolver 实现），
// 替代原 EntryIdentityFunc/EntryIsSupportedFunc 函数指针全局变量。
type IdentityResolver interface {
	Identity(rawURI string) (key string, ok bool)
	Supported(rawURI string) bool
}

// EntryHooks 聚合跨域失效回调（删除为单 URI 形参，对齐原 EntryDeleteCallback）。
type EntryHooks struct {
	// InvalidateParsed 前置节点删除联动：失效 IR 解析缓存。
	InvalidateParsed func(uri string)
}

// EntryManager 封装前置节点池的全部内存状态与持久化编排。
type EntryManager struct {
	mu             sync.RWMutex
	entryList      []Node
	entryHealthMap map[string]*NodeHealth
	entryLoaded    bool

	// entryDirty / loadRetryAfter：加载失败时置位（内存态），语义与出口池
	// （exitpool.Manager.loadDirty）对齐：脏态期间拒绝全量覆盖保存，避免把库里的
	// 存量前置节点整表抹掉；loadRetryAfter 限制延迟重试节奏。
	entryDirty     bool
	loadRetryAfter time.Time

	database *sql.DB          // 进程级 SQLite 句柄（nil=无库内存模式）
	identity IdentityResolver // 替代 EntryIdentityFunc / EntryIsSupportedFunc
	hooks    EntryHooks       // 替代 EntryDeleteCallback / ResetEntryState 联动

	// healthThreshold 返回"连续 N 次网络类失败才禁用"的门槛（可配置，热更新）。
	// 经 SetPolicySet 注入的 PolicySet 读取，全局默认值见 nodestore.DefaultProbeFailThreshold。
	healthThreshold func() int

	// globalFault 是全局故障保护器（多节点同因失败时暂停自动禁用），
	// 经 SetGlobalFaultGuard 注入；nil 时该项保护停用。
	// 语义锁死为"只防、不治"：命中只阻止后续节点被摘除，不恢复任何已禁用节点。
	globalFault *nodestore.GlobalFaultGuard

	// policies 是"类别 → 处理策略"解析器，经 SetPolicySet 注入；
	// nil 时用进程级默认集（全内置默认），保证测试装配与未接线场景行为一致。
	policies *nodestore.PolicySet
}

// NewEntryManager 构造前置节点池管理器。database 为 nil 时运行于无库内存模式。
// healthThreshold 为探测阶段失败阈值的全局默认取值函数（nil 时按 nodestore.DefaultProbeFailThreshold 兜底）。
func NewEntryManager(database *sql.DB, identity IdentityResolver, hooks EntryHooks, healthThreshold func() int) *EntryManager {
	return &EntryManager{
		entryHealthMap:  make(map[string]*NodeHealth),
		database:        database,
		identity:        identity,
		hooks:           hooks,
		healthThreshold: healthThreshold,
		// 阈值函数同时作为策略解析器的全局默认探测失败阈值来源（单一来源）。
		policies: nodestore.NewPolicySet(healthThreshold),
	}
}

// SetPolicySet 注入策略解析器（装配期调用；传 nil 时保持 NewEntryManager 建立的默认解析器）。
func (m *EntryManager) SetPolicySet(p *nodestore.PolicySet) {
	if p == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies = p
}

// policyFor 返回某类别当前的生效策略（单一来源：解析器在 NewEntryManager 里按注入阈值建好）。
func (m *EntryManager) policyFor(class nodestore.FailureClass) nodestore.FaultPolicy {
	return m.policies.For(class)
}
