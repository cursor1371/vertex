package exitpool

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// IdentityResolver 由外部注入，替代原 NodeIdentityFunc/IsSupportedFunc 函数指针全局变量。
// 接口定义于消费方（node 域），实现位于 transport（NewIdentityResolver）；
// 仅暴露 (key, ok)/(bool) 原语，不泄漏 transport.ParsedNode 类型。
type IdentityResolver interface {
	Identity(rawURI string) (key string, ok bool)
	Supported(rawURI string) bool
}

// Hooks 聚合跨域失效回调（双钩子：删除联动 + 全量重置联动）。
type Hooks struct {
	// InvalidateParsed 节点删除联动：失效 IR 解析缓存
	// （替代 DeleteNodeCallback + DeleteNodesBatchCallback，统一为批量形参）。
	InvalidateParsed func(uris []string)
	// InvalidateAll 全量重置联动：清空整个 IR 解析缓存
	// （替代 ResetStateCallback）。
	InvalidateAll func()
}

// healthQueueCap 是健康度异步写队列容量（原包级 healthQueue 语义）。
const healthQueueCap = 1024

// Manager 封装出口节点池的全部内存状态、持久化编排与健康度调度。
type Manager struct {
	mu          sync.RWMutex
	nodeList    []Node
	healthMap   map[string]*NodeHealth
	nodeNameMap map[string]string
	loaded      bool

	// loadDirty / loadRetryAfter：加载失败时置位（内存态）。加载失败意味着内存列表
	// 不可信（可能为空），此间拒绝全量覆盖保存（saveNodesUnsafe），避免把库里的存量
	// 节点整表抹掉；loadRetryAfter 限制 ensureLoaded 的延迟重试节奏，防止 DB 持续
	// 故障时逐请求重试风暴。
	loadDirty      bool
	loadRetryAfter time.Time

	rrIndex uint64 // 竞速轮询游标（mu 保护下原子递增）

	database *sql.DB          // 进程级 SQLite 句柄（nil=无库内存模式）
	identity IdentityResolver // 替代 NodeIdentityFunc / IsSupportedFunc
	hooks    Hooks            // 替代 Delete/DeleteBatch/ResetState 三类回调

	// healthThreshold 是探测阶段失败阈值的取值函数（可配置，热更新）；
	// 在 NewManager 里被包进默认 PolicySet，池内不再单独读它（单一来源，见 policyFor）。
	healthThreshold func() int

	// globalFault 是全局故障保护器（多节点同因失败时暂停自动禁用），
	// 经 SetGlobalFaultGuard 注入；nil 时该项保护停用。
	// 语义锁死为"只防、不治"：命中只阻止后续节点被摘除，不恢复任何已禁用节点。
	globalFault *nodestore.GlobalFaultGuard

	// policies 是"类别 → 处理策略"解析器（动作与探测失败阈值均可配置）。
	// 经 SetPolicySet 注入；nil 时用进程级默认集（全内置默认），
	// 保证测试装配与未接线场景的行为与"全默认策略"一致。
	policies *nodestore.PolicySet

	queue chan healthEvent // 健康度异步写队列

	// 健康写 worker 生命周期：StartHealthWorker 幂等启动；StopHealthWorker 触发
	// healthStop 并等待排空（须在 database.Close 之前调用，否则丢最后一批落库）。
	// 两个守卫必须独立：共用一个 Once 会让"先 Start 后 Stop"时 close 永不执行、
	// Wait 永久阻塞。
	healthStop     chan struct{}
	workerStarted  atomic.Bool
	workerStopOnce sync.Once
	healthWG       sync.WaitGroup

	progressMu  sync.RWMutex
	progress    TestProgress
	testControl *sync.Cond
	// testRunBusy 是批量测速与自动探测共享的独占令牌（见 TryAcquireTestRun）。
	testRunBusy atomic.Bool
}

// NewManager 构造出口节点池管理器。database 为 nil 时运行于无库内存模式；
// identity/hooks 可为零值，由装配链显式注入。healthThreshold 为健康禁用连败
// 阈值取值函数（nil 时按 nodestore.DefaultProbeFailThreshold 兜底）。
func NewManager(database *sql.DB, identity IdentityResolver, hooks Hooks, healthThreshold func() int) *Manager {
	m := &Manager{
		healthMap:   make(map[string]*NodeHealth),
		nodeNameMap: make(map[string]string),
		database:    database,
		identity:    identity,
		hooks:       hooks,
		queue:       make(chan healthEvent, healthQueueCap),
		healthStop:  make(chan struct{}),
	}
	// 阈值函数同时作为策略解析器的"全局默认探测失败阈值"来源（单一来源，见 policyFor）。
	m.policies = nodestore.NewPolicySet(healthThreshold)
	m.testControl = sync.NewCond(&m.progressMu)
	return m
}

// SetPolicySet 注入策略解析器（装配期调用，通常由 main 传入"DB 自定义行 + 配置全局阈值"的实例）。
// 传 nil 时保持 NewManager 建立的默认解析器（全内置默认动作 + 注入的阈值函数）。
func (m *Manager) SetPolicySet(p *nodestore.PolicySet) {
	if p == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies = p
}

// policyFor 返回某类别当前的生效策略。
//
// **单一来源**：解析器在 NewManager 里就按注入的阈值函数建好（未注入 PolicySet 时即用它），
// 因此"池内注入的默认阈值"与"策略解析器读到的全局阈值"永远是同一个值——此前两者各存一份，
// 测试注入 3 而解析器仍按内置默认 2 判定，是真实的漂移隐患。
// 调用方须持 m.mu（PolicySet 自带 RWMutex，这里只保证 m.policies 本身的可见性）。
func (m *Manager) policyFor(class nodestore.FailureClass) nodestore.FaultPolicy {
	return m.policies.For(class)
}
