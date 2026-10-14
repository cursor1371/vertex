package exitpool

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// ---- SQLite 持久化与节点 CRUD ----
//
// 全部 SQL 操作委托 internal/node/nodestore 通用内核（表名固定 "nodes" / "node_health"），
// 本文件仅保留内存态编排与回调编排；数据库句柄经 Manager 注入。

func (m *Manager) ensureLoaded() {
	if m.loaded {
		// 首装失败后的延迟重试（见 loadRetryInterval）：恢复成功即解除脏标记，
		// 并把内存与库重新对齐（补齐 dirty 期间被拒绝的落库）。
		if m.loadDirty && time.Now().After(m.loadRetryAfter) {
			m.loadRetryAfter = time.Now().Add(loadRetryInterval)
			if err := m.loadFromDBUnsafe(); err != nil {
				log.Printf("[Nodes] 节点池加载重试仍失败: %v", err)
				return
			}
			m.loadDirty = false
			m.rebuildNodeNameMapUnsafe()
			m.pruneHealthUnsafe()
			m.saveNodesUnsafe()
			log.Printf("[Nodes] 节点池加载失败已恢复，内存与库重新对齐")
		}
		return
	}
	m.loaded = true

	if m.database == nil {
		return
	}

	if err := m.loadFromDBUnsafe(); err != nil {
		// 首装失败：置脏而不是装作"已加载的空池"。脏态期间全量覆盖保存被拒绝，
		// 否则后续 MergeNodes 的 DELETE+INSERT 会把库里的存量节点整表抹掉。
		m.loadDirty = true
		m.loadRetryAfter = time.Now().Add(loadRetryInterval)
		log.Printf("[Nodes] 节点池加载失败，已置为脏状态（全量保存暂停直至加载成功）: %v", err)
		return
	}
	m.rebuildNodeNameMapUnsafe()
	m.pruneHealthUnsafe()
}

// loadRetryInterval 是节点池加载失败后的重试间隔：DB 一次性故障（如启动时 busy）
// 在此之后自愈；DB 持续故障时把重试风暴压到该节奏。
const loadRetryInterval = 30 * time.Second

// loadFromDBUnsafe 从库装载节点与健康度（调用方须持锁）。
//
// 节点采用合并式装载：以库行为准，dirty 重试时保留 dirty 期间加入内存、库中尚不
// 存在的节点（它们当时被拒绝落库，不能因恢复加载而被挤掉）；健康度只补缺不覆盖，
// dirty 期间内存新产生的状态较库中数据新鲜。
func (m *Manager) loadFromDBUnsafe() error {
	var loaded []Node
	healthFlags := make(map[string]bool)
	if err := nodestore.LoadNodesFull(m.database, "nodes", func(r nodestore.NodeRow) {
		// 来源归一：库行只带了旧的 disabled_by_health 布尔（老库、或历史写入路径），
		// 而来源列为空时，内存必须按 health 处理——否则该节点既不在
		// HealthAutoDisabledURIs（自动探测不恢复它），面板也会把它显示成人工禁用。
		if r.Disabled && r.DisableSource == nodestore.DisableSourceManual && r.DisabledByHealth {
			r.DisableSource = nodestore.DisableSourceHealth
		}
		loaded = append(loaded, Node{ //nolint:exhaustruct
			RawURI:              r.RawURI,
			Type:                r.Type,
			Name:                r.Name,
			Disabled:            r.Disabled,
			Locked:              r.Locked,
			DisableSource:       r.DisableSource,
			DisableReason:       r.DisableReason,
			DisabledAt:          r.DisabledAt,
			DisableFailureClass: r.DisableFailureClass,
		})
		// 健康禁用标记只在库行确实处于禁用态时才有意义（历史残留的 enabled+flag 组合忽略）。
		// 来源与旧布尔列任一命中即视为可自动恢复：老库迁移会把 disabled_by_health=1
		// 回填成 disable_source='health'，这里对新旧两种写法都认。
		if r.Disabled && (r.DisabledByHealth || nodestore.AutoRecoverable(r.DisableSource)) {
			healthFlags[r.RawURI] = true
		}
	}); err != nil {
		return err
	}
	if m.loadDirty {
		inDB := make(map[string]bool, len(loaded))
		for _, n := range loaded {
			inDB[n.RawURI] = true
		}
		for _, n := range m.nodeList {
			if !inDB[n.RawURI] {
				loaded = append(loaded, n)
			}
		}
	}
	m.nodeList = loaded

	hs, err := nodestore.LoadHealth(m.database, "node_health")
	if err != nil {
		// 健康表读取失败同样视为加载失败：DB 能读节点却写不进健康行的状态不可信，
		// 后续保存仍应被脏标记拦截。
		return err
	}
	for i := range hs {
		h := hs[i]
		if _, exists := m.healthMap[h.RawURI]; exists {
			continue // 只补缺：dirty 期间内存健康状态较新
		}
		m.healthMap[h.RawURI] = &NodeHealth{ //nolint:exhaustruct
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
			// 库行携带的健康禁用标记回填：Disabled 本身已持久化，标记不回填的话
			// 重启后"成功测速自动解除禁用"（ReportProbe 的自动恢复分支）失效。
			DisabledByHealth: healthFlags[h.RawURI],
		}
	}
	// 标记回填不得依赖 node_health 行存在：auth 失败禁用（node_error.go）的健康
	// 事件走异步队列，队列满即丢弃——节点已禁用但其健康行可能从未落库。此时也必须
	// 建最小健康条目携带标记，否则重启后该节点既不在 HealthAutoDisabledURIs、也
	// 不在批量测速自禁用名单里，成为无任何自动恢复通道的事实永久禁用。
	for uri := range healthFlags {
		if _, exists := m.healthMap[uri]; !exists {
			m.healthMap[uri] = &NodeHealth{DisabledByHealth: true} //nolint:exhaustruct
		}
	}
	return nil
}

// EnsureLoaded 强制完成节点池装载（幂等），供启动预热与测试使用。
func (m *Manager) EnsureLoaded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
}

func (m *Manager) LoadNodes() []Node {
	m.mu.RLock()
	if m.loaded {
		cp := make([]Node, len(m.nodeList))
		copy(cp, m.nodeList)
		m.mu.RUnlock()
		return cp
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	log.Printf("[Nodes] 获取所有节点 (数量: %d)", len(m.nodeList))
	cp := make([]Node, len(m.nodeList))
	copy(cp, m.nodeList)
	return cp
}

// LoadHealth 返回节点健康度快照（值深拷贝）。
//
// 此前返回 map[string]*NodeHealth 指针浅拷贝：管理 API 在锁外 json.Marshal 时，
// 与 RecordTest/RecordTokenFetch 的锁内写字段并发，构成 -race 下真实可报的数据
// 竞态（含 LastTestError 等 string 的撕裂读），且调用方可借指针篡改内部状态。
// 改为值拷贝后，InFlight 以 atomic 读取。注意：快照不可用于写回——需要修改健康
// 状态请走 Manager 的公开方法。
func (m *Manager) LoadHealth() map[string]NodeHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	cp := make(map[string]NodeHealth, len(m.healthMap))
	for k, v := range m.healthMap {
		h := *v
		h.InFlight = atomic.LoadInt32(&v.InFlight)
		cp[k] = h
	}
	return cp
}

func (m *Manager) saveNodesUnsafe() {
	if m.loadDirty {
		// 加载失败意味着内存列表不完整（可能为空）：此时全量 DELETE+INSERT 会把
		// 库里的存量节点整表抹掉。拒绝覆盖写，直至加载恢复（ensureLoaded 周期重试）。
		log.Printf("[Nodes] 节点池处于加载失败态，拒绝全量保存以避免抹掉存量节点")
		return
	}
	rows := make([]nodestore.NodeRow, 0, len(m.nodeList))
	for _, n := range m.nodeList {
		// healthMap 可能没有该节点的条目（MergeNodes 不强制建健康行），必须 nil 安全。
		byHealth := false
		if h, ok := m.healthMap[n.RawURI]; ok {
			byHealth = h.DisabledByHealth
		}
		rows = append(rows, nodestore.NodeRow{
			RawURI:              n.RawURI,
			Type:                n.Type,
			Name:                n.Name,
			Disabled:            n.Disabled,
			Locked:              n.Locked,
			DisabledByHealth:    byHealth,
			DisableSource:       n.DisableSource,
			DisableReason:       n.DisableReason,
			DisabledAt:          n.DisabledAt,
			DisableFailureClass: n.DisableFailureClass,
		})
	}
	if err := nodestore.SaveNodes(m.database, "nodes", rows); err != nil {
		log.Printf("[Nodes] 保存节点失败: %v", err)
	}
}

func (m *Manager) pruneHealthUnsafe() {
	nodeKeys := make(map[string]bool, len(m.nodeList))
	for _, n := range m.nodeList {
		nodeKeys[n.RawURI] = true
	}
	keys := make(map[string]bool, len(m.healthMap))
	for k := range m.healthMap {
		keys[k] = true
	}
	nodestore.PruneHealthKeys(keys, nodeKeys)
	for k := range m.healthMap {
		if !keys[k] {
			delete(m.healthMap, k)
		}
	}
}

func (m *Manager) MergeNodes(newNodes []Node) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	existing := make(map[string]bool)
	for _, n := range m.nodeList {
		existing[n.RawURI] = true
	}
	for _, n := range newNodes {
		if !existing[n.RawURI] {
			m.nodeList = append(m.nodeList, n)
			existing[n.RawURI] = true
		}
	}
	m.pruneHealthUnsafe()
	m.rebuildNodeNameMapUnsafe()
	m.saveNodesUnsafe()
}

// DeleteNode 删除指定 URI 的出口节点（委托 BatchDeleteNodes 统一编排）。
func (m *Manager) DeleteNode(uri string) {
	m.BatchDeleteNodes([]string{uri})
}

// filterNodesUnsafe 按 keep 谓词过滤节点池并清理被移除节点的健康度，返回被移除 URI 列表。
func (m *Manager) filterNodesUnsafe(keep func(Node) bool) []string {
	var kept []Node
	var removed []string
	for _, n := range m.nodeList {
		if keep(n) {
			kept = append(kept, n)
		} else {
			removed = append(removed, n.RawURI)
			delete(m.healthMap, n.RawURI)
		}
	}
	m.nodeList = kept
	m.rebuildNodeNameMapUnsafe()
	return removed
}

func (m *Manager) DedupNodes() int {
	m.mu.Lock()
	m.ensureLoaded()
	// 组内择优：同身份组保留"最好"的代表（优先未禁用，其次连败少、最近成功近），
	// 其余移除。此前固定保留首个——导入是尾部追加，新导入的可用节点必然排在
	// 旧禁用节点之后而被误删，导致去重反而造成数据损失。
	type groupInfo struct {
		repIdx int // 代表节点在 nodeList 中的下标
		repSet bool
	}
	groups := make(map[string]*groupInfo)
	lockedKept := make(map[int]bool) // locked 节点永不作为重复项移除
	for i := range m.nodeList {
		n := m.nodeList[i]
		key := n.RawURI
		if m.identity != nil {
			if k, ok := m.identity.Identity(n.RawURI); ok {
				key = k
			}
		}
		if n.Locked {
			// 防删锁：locked 节点永不作为重复项被移除（宁可少去重，也不动锁定节点）。
			// 同时占据其身份组：组内后续未锁定节点仍视为重复移除（保持"locked 先到
			// 则后来者删"的既有语义）；若后来者更优则共同保留（locked 豁免删除）。
			lockedKept[i] = true
			if _, ok := groups[key]; !ok {
				groups[key] = &groupInfo{repIdx: i}
			}
			continue
		}
		g, ok := groups[key]
		if !ok {
			groups[key] = &groupInfo{repIdx: i, repSet: true}
			continue
		}
		if m.dedupBetter(n, m.nodeList[g.repIdx]) {
			g.repIdx = i
		}
	}
	keepIdx := make(map[int]bool)
	var removedURIs []string
	for _, g := range groups {
		keepIdx[g.repIdx] = true
	}
	for i := range m.nodeList {
		if lockedKept[i] || keepIdx[i] {
			keepIdx[i] = true
			continue
		}
		removedURIs = append(removedURIs, m.nodeList[i].RawURI)
	}
	kept := make([]Node, 0, len(m.nodeList)-len(removedURIs))
	for i := range m.nodeList {
		if keepIdx[i] {
			kept = append(kept, m.nodeList[i])
		}
	}
	removed := len(removedURIs)
	if removed == 0 {
		m.mu.Unlock()
		return 0
	}
	// 有数据库：定向删除先行，事务成功后才应用内存结果（失败则内存保持原状）。
	if m.database != nil {
		if err := nodestore.DeleteNodesBatch(m.database, "nodes", "node_health", removedURIs); err != nil {
			log.Printf("[Nodes] DedupNodes 删除失败: %v", err)
			m.mu.Unlock()
			return 0
		}
	}
	// 事务成功后统一清理健康度与节点列表，保证失败路径内存零变更。
	for _, u := range removedURIs {
		delete(m.healthMap, u)
	}
	m.nodeList = kept
	m.rebuildNodeNameMapUnsafe()
	hooks := m.hooks
	m.mu.Unlock() // 先解锁再通知销毁连接池

	if hooks.InvalidateParsed != nil {
		hooks.InvalidateParsed(removedURIs)
	}
	return removed
}

// dedupBetter 判定 candidate 是否比 incumbent 更值得作为同身份组的保留代表：
// 优先未禁用（可立即恢复服务），其次连败少（更健康），再次最近成功更近（更鲜活）。
// 全部持平返回 false（保持原顺序，结果稳定可预测）。调用方须持 m.mu。
func (m *Manager) dedupBetter(candidate, incumbent Node) bool {
	ch := m.healthMap[candidate.RawURI]
	ih := m.healthMap[incumbent.RawURI]
	cDis, iDis := candidate.Disabled, incumbent.Disabled
	if cDis != iDis {
		return iDis // 未禁用者优先
	}
	cFail, iFail := 0, 0
	if ch != nil {
		cFail = ch.ConsecutiveFailures
	}
	if ih != nil {
		iFail = ih.ConsecutiveFailures
	}
	if cFail != iFail {
		return cFail < iFail
	}
	cSucc, iSucc := int64(0), int64(0)
	if ch != nil {
		cSucc = ch.LastSuccessAt
	}
	if ih != nil {
		iSucc = ih.LastSuccessAt
	}
	return cSucc > iSucc
}

func (m *Manager) DeleteDisabled() int {
	m.mu.Lock()
	m.ensureLoaded()
	var removedURIs []string
	if m.database == nil {
		// 无库模式：纯内存过滤（保持现状语义）；locked 节点永不删除。
		removedURIs = m.filterNodesUnsafe(func(n Node) bool { return !n.Disabled || n.Locked })
	} else {
		// 有数据库：定向删除先行，事务成功后才同步内存（失败则内存保持原状）。
		removed, uris, err := nodestore.DeleteDisabledNodes(m.database, "nodes", "node_health")
		if err != nil {
			log.Printf("[Nodes] DeleteDisabled 删除失败: %v", err)
			m.mu.Unlock()
			return 0
		}
		if removed > 0 {
			targets := make(map[string]bool, len(uris))
			for _, u := range uris {
				targets[u] = true
			}
			m.filterNodesUnsafe(func(n Node) bool { return !targets[n.RawURI] })
		}
		removedURIs = uris
	}
	hooks := m.hooks
	m.mu.Unlock()

	if hooks.InvalidateParsed != nil {
		hooks.InvalidateParsed(removedURIs)
	}
	return len(removedURIs)
}

// BatchUpdateNodesDisabled 批量启用/禁用（**人工禁用语义**）：禁用时把来源记为人工
// （空串）并清空审计原因，此后任何成功探测都不得把这些节点自动放开。
// 防删保护：禁用操作跳过 locked 节点（启用不受限，便于手动恢复）。
func (m *Manager) BatchUpdateNodesDisabled(uris []string, disabled bool) {
	m.batchSetDisabled(uris, disabled, nodestore.DisableSourceManual, "", "")
}

// BatchDisableWithSource 按指定来源批量禁用：语义与人工禁用相同（不会被自动恢复，
// 清空可恢复标记），只是把"谁禁的、为什么禁、当时什么故障"写进审计。
// 用于 admin 测试失败（admin_test）与协议不支持（unsupported）两条路径——
// 评审已确认它们失败即直接禁用，不走阈值。
func (m *Manager) BatchDisableWithSource(uris []string, source, reason, class string) {
	m.batchSetDisabled(uris, true, source, reason, class)
}

func (m *Manager) batchSetDisabled(uris []string, disabled bool, source, reason, class string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	// 防删保护：禁用操作跳过 locked 节点（启用不受限，便于手动恢复）。
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
	}
	if disabled {
		for _, n := range m.nodeList {
			if n.Locked {
				delete(targets, n.RawURI)
			}
		}
	}
	if len(targets) == 0 {
		return
	}
	effURIs := make([]string, 0, len(targets))
	for u := range targets {
		effURIs = append(effURIs, u)
	}
	now := time.Now().Unix()
	if !disabled {
		// 启用：清空审计与自动探测连败，走统一的内存+落库路径。
		for _, u := range effURIs {
			if h, ok := m.healthMap[u]; ok {
				h.AutoProbeFailStreak = 0
			}
			m.syncDisableAuditUnsafe(u, false, nodestore.DisableSourceManual, "", "", now)
		}
		return
	}
	// 有数据库：事务提交成功后才更新内存，失败则内存保持原状，
	// 避免「内存成功、数据库失败」的伪成功状态。
	st := nodestore.DisableState{
		Disabled:         true,
		DisabledByHealth: nodestore.AutoRecoverable(source),
		Source:           source,
		Reason:           reason,
		DisabledAt:       now,
		FailureClass:     class,
	}
	if m.database != nil {
		if err := nodestore.UpdateDisabledBatch(m.database, "nodes", effURIs, st); err != nil {
			log.Printf("[Nodes] 批量禁用更新失败: %v", err)
			return
		}
	}
	for _, u := range effURIs {
		idx := m.nodeByURISeek(u)
		if idx < 0 {
			continue
		}
		m.nodeList[idx].Disabled = true
		m.nodeList[idx].DisableSource = source
		m.nodeList[idx].DisableReason = reason
		m.nodeList[idx].DisabledAt = now
		m.nodeList[idx].DisableFailureClass = class
		if h, ok := m.healthMap[u]; ok {
			h.DisabledByHealth = st.DisabledByHealth
		}
	}
}

// BatchUpdateNodesLocked 批量设置/解除防删锁（locked）。
// locked 节点由删除、去重与批量手动禁用路径豁免；健康触发的自动禁用不豁免
// （节点实际连不通时必须摘出轮换，否则会被反复选中）。
func (m *Manager) BatchUpdateNodesLocked(uris []string, locked bool) {
	if len(uris) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	targets := make(map[string]bool, len(uris))
	for _, u := range uris {
		targets[u] = true
	}
	if m.database == nil {
		for i, n := range m.nodeList {
			if targets[n.RawURI] {
				m.nodeList[i].Locked = locked
			}
		}
		return
	}
	if err := nodestore.UpdateLockedBatch(m.database, "nodes", uris, locked); err != nil {
		log.Printf("[Nodes] BatchUpdateNodesLocked 更新失败: %v", err)
		return
	}
	for i, n := range m.nodeList {
		if targets[n.RawURI] {
			m.nodeList[i].Locked = locked
		}
	}
}

func (m *Manager) BatchDeleteNodes(uris []string) {
	if len(uris) == 0 {
		return
	}
	m.mu.Lock()
	m.ensureLoaded()
	// 防删保护：locked 节点永不参与删除，先从目标集合剔除。
	locked := make(map[string]bool, len(m.nodeList))
	for _, n := range m.nodeList {
		if n.Locked {
			locked[n.RawURI] = true
		}
	}
	targets := make(map[string]bool, len(uris))
	for _, u := range uris {
		if locked[u] {
			continue
		}
		targets[u] = true
	}
	if len(targets) == 0 {
		m.mu.Unlock()
		return
	}
	delURIs := make([]string, 0, len(targets))
	for u := range targets {
		delURIs = append(delURIs, u)
	}
	var removedURIs []string
	if m.database == nil {
		// 无库模式：纯内存过滤（保持现状语义）
		removedURIs = m.filterNodesUnsafe(func(n Node) bool { return !targets[n.RawURI] })
	} else {
		// 有数据库：定向删除先行，事务成功后才同步内存（失败则内存保持原状）。
		if err := nodestore.DeleteNodesBatch(m.database, "nodes", "node_health", delURIs); err != nil {
			log.Printf("[Nodes] BatchDeleteNodes 删除失败: %v", err)
			m.mu.Unlock()
			return
		}
		removedURIs = m.filterNodesUnsafe(func(n Node) bool { return !targets[n.RawURI] })
	}
	hooks := m.hooks
	m.mu.Unlock() // 防止在批量删除时引发卡死死锁

	if hooks.InvalidateParsed != nil {
		hooks.InvalidateParsed(removedURIs)
	}
}
