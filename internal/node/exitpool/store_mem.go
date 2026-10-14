package exitpool

import (
	"sync/atomic"
)

type Node struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	RawURI   string `json:"raw_uri"`
	Disabled bool   `json:"disabled"`
	// Locked 为防删锁：置位后该节点不会被删除、去重移除或批量手动禁用。
	// 健康触发的自动禁用不受保护——防删保护的是"别被用户误删/误批量禁用"，
	// 而"这个节点实际连不通"必须摘出轮换，否则会被反复选中（可随时手动启用恢复）。
	Locked bool `json:"locked"`
	// 禁用审计四列（见 nodestore.DisableSource*）：回答"谁禁的、为什么、何时、当时什么故障"。
	// DisableSource 为空串即人工禁用；只有 health 来源可被自动探测恢复。
	DisableSource       string `json:"disable_source"`
	DisableReason       string `json:"disable_reason"`
	DisabledAt          int64  `json:"disabled_at"`
	DisableFailureClass string `json:"disable_failure_class"`
}

type NodeHealth struct { //nolint:govet
	SuccessCount        int `json:"success_count"`
	FailCount           int `json:"fail_count"`
	ConsecutiveFailures int `json:"consecutive_failures"`
	// TokenFailStreak 是 rT token 抓取的连续失败次数（成功即清零）。与 ConsecutiveFailures
	// 分开计数：后者会被请求路径的成功重置，而"对 GraphQL 正常、对 rT 端点坏"的节点必须
	// 能凭自己的连败被摘出轮换（见 RecordTokenFetch）。
	TokenFailStreak int `json:"token_fail_streak"`
	// RTLastMs 是最近一次 rT 抓取（该节点）的耗时（毫秒）；RTHangStreak 是挂住连败；
	// RTCooldownUntil 是 rT 专属冷却。三者只影响 **rT 抓取选点**（SelectForTokenFetch），
	// 不参与普通请求选点、也不禁用节点；均为内存态（重启归零，与 TokenFailStreak 同例）。
	RTLastMs        int64   `json:"rt_last_ms"`
	RTHangStreak    int     `json:"rt_hang_streak"`
	RTCooldownUntil int64   `json:"rt_cooldown_until"`
	LastTestMs      float64 `json:"last_test_ms"`
	LastTestError   string  `json:"last_test_error"`
	LastSuccessAt   int64   `json:"last_success_at"`
	LastFailAt      int64   `json:"last_fail_at"`
	CooldownUntil   int64   `json:"cooldown_until"`
	Last429At       int64   `json:"last_429_at"`
	RateLimitCount  int     `json:"rate_limit_count"`
	RecentUseCount  int     `json:"recent_use_count"`
	LastSelectedAt  int64   `json:"last_selected_at"`
	// LastTestAt 是最近一次检查的时间戳（LastTestMs 是本次耗时，不能当时间用），
	// 面板用它显示"最近检查时间"。LastFailureClass / LastHTTPStatus 是最近一次失败的
	// 归因类别与 HTTP 状态码，用于失败类别徽章与故障详情。
	LastTestAt       int64  `json:"last_test_at"`
	LastFailureClass string `json:"last_failure_class"`
	LastHTTPStatus   int    `json:"last_http_status"`
	// AutoProbeFailStreak 是自动探测的连续失败次数：达 health_disable_threshold 即把
	// 健康禁用升级为永久禁用（nodestore.DisableSourcePermanent），不再被探测。
	AutoProbeFailStreak int `json:"auto_probe_fail_streak"`
	// InFlight 导出给管理页展示"在途请求数"（P3-3 后 LoadHealth 深拷贝时以
	// atomic 读取，序列化读的是拷贝值，无竞态）。
	InFlight         int32 `json:"in_flight"`
	LastSubHealthyAt int64 `json:"last_sub_healthy_at"`
	// DisabledByHealth 标记 Disabled 由健康判定触发（内存态，重启归零）：
	// RecordTest 成功路径据此自动解除禁用；人工禁用（BatchUpdateNodesDisabled 会清本标记）
	// 永远不会被自动放开。导出给管理页区分"已禁用（健康·测速成功自动恢复）"与
	// "已禁用（人工）"。
	//
	// 它是 Node.DisableSource == nodestore.DisableSourceHealth 的内存视图，保留该字段
	// 是因为既有恢复路径与面板都按它判断；两处必须同步维护（见 syncDisableAuditUnsafe）。
	DisabledByHealth bool `json:"disabled_by_health"`
}

// TestProgress 是批量测速进度快照。
type TestProgress struct {
	Running     bool   `json:"running"`
	Paused      bool   `json:"paused"`
	Terminated  bool   `json:"terminated"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	OkCount     int    `json:"ok_count"`
	FailCount   int    `json:"fail_count"`
	CurrentNode string `json:"current_node"`
}

// ---- 内存节点池状态方法（与持久化/健康度解耦，见 store_db.go / store_health.go）----

func (m *Manager) IncInFlight(uri string) {
	m.mu.Lock()
	m.ensureLoaded()
	h, exists := m.healthMap[uri]
	if !exists {
		h = &NodeHealth{}
		m.healthMap[uri] = h
	}
	m.mu.Unlock()
	atomic.AddInt32(&h.InFlight, 1)
}

func (m *Manager) DecInFlight(uri string) {
	m.mu.Lock()
	m.ensureLoaded()
	h, exists := m.healthMap[uri]
	m.mu.Unlock()
	if !exists {
		return
	}
	// 下界钳制在 0：Inc 与 Dec 之间条目可能被删除重建（filterNodesUnsafe 清
	// healthMap、重新导入时 Inc 再建新条目），旧候选的 Dec 会命中新条目——不钳制
	// 会把 InFlight 减成负值，该节点在 inFlight 升序选点中永远排最前形成热点。
	for {
		cur := atomic.LoadInt32(&h.InFlight)
		if cur <= 0 || atomic.CompareAndSwapInt32(&h.InFlight, cur, cur-1) {
			return
		}
	}
}

func (m *Manager) Reset() {
	m.mu.Lock()
	m.nodeList = nil
	m.healthMap = make(map[string]*NodeHealth)
	m.nodeNameMap = make(map[string]string)
	m.loaded = false
	hooks := m.hooks
	m.mu.Unlock() // 先解锁再通知外部清理（如 transport 解析缓存），避免死锁
	if hooks.InvalidateAll != nil {
		hooks.InvalidateAll()
	}
}

// AllRawURIs 返回全部节点 URI（含禁用），供启动预热 IR 解析缓存使用。
func (m *Manager) AllRawURIs() []string {
	m.mu.RLock()
	if m.loaded {
		uris := make([]string, 0, len(m.nodeList))
		for _, n := range m.nodeList {
			uris = append(uris, n.RawURI)
		}
		m.mu.RUnlock()
		return uris
	}
	m.mu.RUnlock()

	m.mu.Lock()
	m.ensureLoaded()
	uris := make([]string, 0, len(m.nodeList))
	for _, n := range m.nodeList {
		uris = append(uris, n.RawURI)
	}
	m.mu.Unlock()
	return uris
}

// ---- 批量测速进度与流程控制 ----

func (m *Manager) GetTestProgress() TestProgress {
	m.progressMu.RLock()
	defer m.progressMu.RUnlock()
	return m.progress
}

func (m *Manager) IsTestRunning() bool {
	m.progressMu.RLock()
	defer m.progressMu.RUnlock()
	return m.progress.Running
}

// TryAcquireTestRun / ReleaseTestRun 是"独占测试运行"令牌：批量测速与自动探测都必须先
// 取得它。此前两者只用 IsTestRunning() 互判——"检查通过"到"真正启动"之间存在窗口
// （TOCTOU），同一节点可能被两边同时探测，导致失败计数重复累计、提前触阈。
// 令牌把"判定 + 占用"合成一次原子 CAS，消除该窗口。
func (m *Manager) TryAcquireTestRun() bool {
	return m.testRunBusy.CompareAndSwap(false, true)
}

// ReleaseTestRun 释放独占测试运行令牌（须与 TryAcquireTestRun 成对调用）。
func (m *Manager) ReleaseTestRun() {
	m.testRunBusy.Store(false)
}

func (m *Manager) StartTestProgress(total int) bool {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	if m.progress.Running {
		return false
	}
	// 同一把令牌：自动探测持有时批量测速不得启动（反之亦然）。
	if !m.TryAcquireTestRun() {
		return false
	}
	m.progress = TestProgress{
		Running:     true,
		Paused:      false,
		Terminated:  false,
		Total:       total,
		Done:        0,
		OkCount:     0,
		FailCount:   0,
		CurrentNode: "准备中...",
	}
	return true
}

func (m *Manager) UpdateTestProgress(nodeName string, ok bool) {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	if !m.progress.Running || m.progress.Terminated {
		return
	}
	m.progress.Done++
	if ok {
		m.progress.OkCount++
	} else {
		m.progress.FailCount++
	}
	m.progress.CurrentNode = nodeName
}

func (m *Manager) FinishTestProgress() {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	m.progress.Running = false
	m.progress.Paused = false
	m.progress.CurrentNode = "测试完成"
	m.testControl.Broadcast()
	// 释放独占令牌：必须与 StartTestProgress 里的获取成对，否则后续批量测速与
	// 自动探测都无法再启动。
	m.ReleaseTestRun()
}

func (m *Manager) PauseTestProgress() {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	if m.progress.Running && !m.progress.Terminated {
		m.progress.Paused = true
		m.progress.CurrentNode = "已暂停..."
	}
}

func (m *Manager) ResumeTestProgress() {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	if m.progress.Running && m.progress.Paused {
		m.progress.Paused = false
		m.progress.CurrentNode = "恢复测试中..."
		m.testControl.Broadcast()
	}
}

func (m *Manager) TerminateTestProgress() {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	if m.progress.Running {
		m.progress.Terminated = true
		m.progress.Paused = false
		m.progress.CurrentNode = "正在终止..."
		m.testControl.Broadcast()
	}
}

func (m *Manager) CheckTestControl() bool {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	for m.progress.Running && m.progress.Paused && !m.progress.Terminated {
		m.testControl.Wait()
	}
	return !m.progress.Running || m.progress.Terminated
}

// ---- 节点名映射 ----

func (m *Manager) rebuildNodeNameMapUnsafe() {
	names := make(map[string]string, len(m.nodeList))
	for _, n := range m.nodeList {
		names[n.RawURI] = n.Name
	}
	m.nodeNameMap = names
}

// NodeName 返回节点展示名（满足 transport.NodeNamer）。
func (m *Manager) NodeName(uri string) string {
	m.mu.RLock()
	if m.loaded {
		name, ok := m.nodeNameMap[uri]
		m.mu.RUnlock()
		if ok {
			return name
		}
		return "Unknown"
	}
	m.mu.RUnlock()

	m.mu.Lock()
	m.ensureLoaded()
	name, ok := m.nodeNameMap[uri]
	m.mu.Unlock()
	if ok {
		return name
	}
	return "Unknown"
}

// URIForName 按展示名反查原始 URI（与 NodeName 互为逆映射）。
// 同名多 URI 返回首个匹配；找不到时返回 name 本身——因为 name 可能是 URI 回退
// （nodeNameOrURI 在名称缺失时以 URI 作为展示名），此时原样返回保证锁定可用。
func (m *Manager) URIForName(name string) string {
	if name == "" {
		return ""
	}
	m.mu.RLock()
	if m.loaded {
		uri := m.uriForNameUnsafe(name)
		m.mu.RUnlock()
		return uri
	}
	m.mu.RUnlock()

	m.mu.Lock()
	m.ensureLoaded()
	uri := m.uriForNameUnsafe(name)
	m.mu.Unlock()
	return uri
}

func (m *Manager) uriForNameUnsafe(name string) string {
	for _, n := range m.nodeList {
		if n.Name == name {
			return n.RawURI
		}
	}
	return name
}
