package entrypool

import (
	"log"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// ---- SQLite 持久化与前置节点 CRUD ----
//
// 全部 SQL 操作委托 internal/node/nodestore 通用内核（表名固定 "entry_nodes" /
// "entry_node_health"），本文件仅保留内存态编排与回调编排；数据库句柄经 EntryManager 注入。

func (m *EntryManager) ensureEntryLoaded() {
	if m.entryLoaded {
		// 首装失败后的延迟重试（与出口池 loadRetryInterval 同节奏）：恢复成功即
		// 解除脏标记，并把内存与库重新对齐（补齐 dirty 期间被拒绝的落库）。
		if m.entryDirty && time.Now().After(m.loadRetryAfter) {
			m.loadRetryAfter = time.Now().Add(loadRetryInterval)
			if err := m.loadEntriesFromDBUnsafe(); err != nil {
				log.Printf("[EntryNodes] 前置节点池加载重试仍失败: %v", err)
				return
			}
			m.entryDirty = false
			m.pruneEntryHealthUnsafe()
			m.saveEntryNodesUnsafe()
			log.Printf("[EntryNodes] 前置节点池加载失败已恢复，内存与库重新对齐")
		}
		return
	}
	m.entryLoaded = true

	if m.database == nil {
		return
	}

	if err := m.loadEntriesFromDBUnsafe(); err != nil {
		// 首装失败：置脏而不是装作"已加载的空池"。脏态期间全量覆盖保存被拒绝，
		// 否则后续 MergeNodes 的 DELETE+INSERT 会把库里的存量前置节点整表抹掉。
		m.entryDirty = true
		m.loadRetryAfter = time.Now().Add(loadRetryInterval)
		log.Printf("[EntryNodes] 前置节点池加载失败，已置为脏状态（全量保存暂停直至加载成功）: %v", err)
		return
	}
	m.pruneEntryHealthUnsafe()
}

// loadRetryInterval 是前置节点池加载失败后的重试间隔（与出口池同节奏）。
const loadRetryInterval = 30 * time.Second

// loadEntriesFromDBUnsafe 从库装载前置节点与健康度（调用方须持锁）。
//
// 节点采用合并式装载：以库行为准，dirty 重试时保留 dirty 期间加入内存、库中尚不
// 存在的节点；健康度只补缺不覆盖，dirty 期间内存新产生的状态较库中数据新鲜。
func (m *EntryManager) loadEntriesFromDBUnsafe() error {
	var loaded []Node
	healthFlags := make(map[string]bool)
	if err := nodestore.LoadNodesFull(m.database, "entry_nodes", func(r nodestore.NodeRow) {
		// 来源归一：库行只带旧的 disabled_by_health 布尔而来源列为空时按 health 处理
		//（与出口池同口径，避免老库节点被显示成人工禁用且丢失自动恢复资格）。
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
		// 来源与旧布尔列任一命中即视为可自动恢复（老库迁移会把布尔列回填成来源）。
		if r.Disabled && (r.DisabledByHealth || nodestore.AutoRecoverable(r.DisableSource)) {
			healthFlags[r.RawURI] = true
		}
	}); err != nil {
		return err
	}
	if m.entryDirty {
		inDB := make(map[string]bool, len(loaded))
		for _, n := range loaded {
			inDB[n.RawURI] = true
		}
		for _, n := range m.entryList {
			if !inDB[n.RawURI] {
				loaded = append(loaded, n)
			}
		}
	}
	m.entryList = loaded

	hs, err := nodestore.LoadHealth(m.database, "entry_node_health")
	if err != nil {
		// 健康表读取失败同样视为加载失败（与出口池口径一致）。
		return err
	}
	for i := range hs {
		h := hs[i]
		if _, exists := m.entryHealthMap[h.RawURI]; exists {
			continue // 只补缺：dirty 期间内存健康状态较新
		}
		m.entryHealthMap[h.RawURI] = &NodeHealth{ //nolint:exhaustruct
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
			// 库行携带的健康禁用标记回填：Disabled 已持久化，标记不回填的话
			// 重启后"成功测速自动解除禁用"失效。
			DisabledByHealth: healthFlags[h.RawURI],
		}
	}
	return nil
}

// LoadNodes 返回全量前置节点列表（含禁用）。
// 返回浅拷贝切片，切断与内部全局切片底层的引用共享（对齐 LoadHealth 安全设计）。
func (m *EntryManager) LoadNodes() []Node {
	m.mu.RLock()
	if m.entryLoaded {
		cp := make([]Node, len(m.entryList))
		copy(cp, m.entryList)
		m.mu.RUnlock()
		return cp
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	cp := make([]Node, len(m.entryList))
	copy(cp, m.entryList)
	return cp
}

// LoadHealth 返回前置节点健康度快照（值深拷贝）。
//
// 此前返回指针浅拷贝：管理 API 在锁外 json.Marshal 与 RecordTest 的锁内写并发，
// 构成 -race 可报的数据竞态，且调用方可借指针篡改内部状态。改为值拷贝后，
// 快照不可用于写回——需要修改健康状态请走 EntryManager 的公开方法。
func (m *EntryManager) LoadHealth() map[string]NodeHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	cp := make(map[string]NodeHealth, len(m.entryHealthMap))
	for k, v := range m.entryHealthMap {
		cp[k] = *v
	}
	return cp
}

func (m *EntryManager) saveEntryNodesUnsafe() {
	if m.entryDirty {
		// 加载失败意味着内存列表不完整（可能为空）：此时全量 DELETE+INSERT 会把
		// 库里的存量前置节点整表抹掉。拒绝覆盖写，直至加载恢复（延迟重试）。
		log.Printf("[EntryNodes] 前置节点池处于加载失败态，拒绝全量保存以避免抹掉存量节点")
		return
	}
	rows := make([]nodestore.NodeRow, 0, len(m.entryList))
	for _, n := range m.entryList {
		// entryHealthMap 可能没有该节点的条目，必须 nil 安全。
		byHealth := false
		if h, ok := m.entryHealthMap[n.RawURI]; ok {
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
	if err := nodestore.SaveNodes(m.database, "entry_nodes", rows); err != nil {
		log.Printf("[EntryNodes] 保存节点失败: %v", err)
	}
}

func (m *EntryManager) saveEntryHealthUnsafe() {
	rows := make([]nodestore.HealthRow, 0, len(m.entryHealthMap))
	for uri, h := range m.entryHealthMap {
		rows = append(rows, nodestore.HealthRow{
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
	if err := nodestore.SaveHealth(m.database, "entry_node_health", rows); err != nil {
		log.Printf("[EntryNodes] 保存健康度失败: %v", err)
	}
}

func (m *EntryManager) pruneEntryHealthUnsafe() {
	nodeKeys := make(map[string]bool, len(m.entryList))
	for _, n := range m.entryList {
		nodeKeys[n.RawURI] = true
	}
	keys := make(map[string]bool, len(m.entryHealthMap))
	for k := range m.entryHealthMap {
		keys[k] = true
	}
	nodestore.PruneHealthKeys(keys, nodeKeys)
	for k := range m.entryHealthMap {
		if !keys[k] {
			delete(m.entryHealthMap, k)
		}
	}
}

// MergeNodes 追加导入新前置节点（按 raw_uri 去重）。
func (m *EntryManager) MergeNodes(newNodes []Node) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	existing := make(map[string]bool)
	for _, n := range m.entryList {
		existing[n.RawURI] = true
	}
	for _, n := range newNodes {
		if !existing[n.RawURI] {
			m.entryList = append(m.entryList, n)
			existing[n.RawURI] = true
		}
	}
	m.pruneEntryHealthUnsafe()
	m.saveEntryNodesUnsafe()
}

// DedupNodes 按身份键（IdentityResolver）去重，返回移除数量。
// 组内择优：同身份组保留"最好"的代表（优先未禁用），其余移除——此前固定保留
// 首个，导入尾部追加会让新导入的可用节点排在旧禁用节点之后而被误删。
// 有数据库时保存先行（用去重后的列表做全量替换），失败则内存保持原状，
// 避免"内存已改、库未改"的伪状态。
func (m *EntryManager) DedupNodes() int {
	m.mu.Lock()
	m.ensureEntryLoaded()
	type groupInfo struct {
		repIdx int
	}
	groups := make(map[string]*groupInfo)
	lockedKept := make(map[int]bool)
	for i := range m.entryList {
		n := m.entryList[i]
		key := n.RawURI
		if m.identity != nil {
			if k, ok := m.identity.Identity(n.RawURI); ok {
				key = k
			}
		}
		// 防删锁：locked 节点永不作为重复项被移除；同时占据其身份组，
		// 组内后续未锁定节点仍视为重复移除（保持"locked 先到则后来者删"）。
		if n.Locked {
			lockedKept[i] = true
			if _, ok := groups[key]; !ok {
				groups[key] = &groupInfo{repIdx: i}
			}
			continue
		}
		if g, ok := groups[key]; ok {
			if m.dedupBetter(n, m.entryList[g.repIdx]) {
				g.repIdx = i
			}
		} else {
			groups[key] = &groupInfo{repIdx: i}
		}
	}
	keepIdx := make(map[int]bool)
	var removedURIs []string
	for _, g := range groups {
		keepIdx[g.repIdx] = true
	}
	for i := range m.entryList {
		if lockedKept[i] || keepIdx[i] {
			keepIdx[i] = true
			continue
		}
		removedURIs = append(removedURIs, m.entryList[i].RawURI)
	}
	kept := make([]Node, 0, len(m.entryList))
	for i := range m.entryList {
		if keepIdx[i] {
			kept = append(kept, m.entryList[i])
		}
	}
	removed := len(removedURIs)
	if removed == 0 {
		m.mu.Unlock()
		return 0
	}
	// 保存先行：以 kept 列表构造行集做全量替换，失败则内存保持原状
	//（saveEntryNodesUnsafe 读的是 m.entryList，此处尚未替换）。
	if m.database != nil && !m.entryDirty {
		rows := make([]nodestore.NodeRow, 0, len(kept))
		for _, n := range kept {
			byHealth := false
			if h, ok := m.entryHealthMap[n.RawURI]; ok {
				byHealth = h.DisabledByHealth
			}
			rows = append(rows, nodestore.NodeRow{
				RawURI: n.RawURI, Type: n.Type, Name: n.Name, Disabled: n.Disabled, Locked: n.Locked, DisabledByHealth: byHealth,
			})
		}
		if err := nodestore.SaveNodes(m.database, "entry_nodes", rows); err != nil {
			log.Printf("[EntryNodes] DedupNodes 保存失败: %v", err)
			m.mu.Unlock()
			return 0
		}
	}
	for _, u := range removedURIs {
		delete(m.entryHealthMap, u)
	}
	m.entryList = kept
	m.saveEntryHealthUnsafe()
	hooks := m.hooks
	m.mu.Unlock()

	if hooks.InvalidateParsed != nil {
		for _, u := range removedURIs {
			hooks.InvalidateParsed(u)
		}
	}
	return removed
}

// dedupBetter 判定 candidate 是否比 incumbent 更值得作为同身份组的保留代表：
// 优先未禁用，其次最近成功更近。全部持平返回 false（保持原顺序）。
// 调用方须持 m.mu。
func (m *EntryManager) dedupBetter(candidate, incumbent Node) bool {
	if candidate.Disabled != incumbent.Disabled {
		return incumbent.Disabled
	}
	ch := m.entryHealthMap[candidate.RawURI]
	ih := m.entryHealthMap[incumbent.RawURI]
	cSucc, iSucc := int64(0), int64(0)
	if ch != nil {
		cSucc = ch.LastSuccessAt
	}
	if ih != nil {
		iSucc = ih.LastSuccessAt
	}
	return cSucc > iSucc
}

// DeleteDisabled 清空已禁用的前置节点，返回删除数量。
func (m *EntryManager) DeleteDisabled() int {
	m.mu.Lock()
	m.ensureEntryLoaded()
	var kept []Node
	removed := 0
	var removedURIs []string
	for _, n := range m.entryList {
		// 防删锁：locked 节点即使处于禁用态也不被清空。
		if !n.Disabled || n.Locked {
			kept = append(kept, n)
		} else {
			removed++
			removedURIs = append(removedURIs, n.RawURI)
			delete(m.entryHealthMap, n.RawURI)
		}
	}
	m.entryList = kept
	m.saveEntryNodesUnsafe()
	m.saveEntryHealthUnsafe()
	hooks := m.hooks
	m.mu.Unlock()

	if hooks.InvalidateParsed != nil {
		for _, u := range removedURIs {
			hooks.InvalidateParsed(u)
		}
	}
	return removed
}

// BatchUpdateNodesDisabled 批量启用/禁用前置节点（**人工禁用语义**）：禁用时把来源
// 记为人工（空串）并清空审计原因，此后任何成功测速都不得把这些节点自动放开。
// 防删保护：禁用操作跳过 locked 节点（启用不受限）。
func (m *EntryManager) BatchUpdateNodesDisabled(uris []string, disabled bool) {
	m.batchSetDisabled(uris, disabled, nodestore.DisableSourceManual, "", "")
}

// BatchDisableWithSource 按指定来源批量禁用前置节点：语义与人工禁用相同（不会被自动
// 恢复），只是把"谁禁的、为什么禁、当时什么故障"写进审计。用于 admin 测试失败
// （admin_test）与协议不支持（unsupported）。
func (m *EntryManager) BatchDisableWithSource(uris []string, source, reason, class string) {
	m.batchSetDisabled(uris, true, source, reason, class)
}

func (m *EntryManager) batchSetDisabled(uris []string, disabled bool, source, reason, class string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	// 防删保护：禁用操作跳过 locked 节点（启用不受限）。
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
	}
	if disabled {
		for _, n := range m.entryList {
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
	st := nodestore.DisableState{
		Disabled:         disabled,
		DisabledByHealth: disabled && nodestore.AutoRecoverable(source),
		Source:           source,
		Reason:           reason,
		FailureClass:     class,
	}
	if disabled {
		st.DisabledAt = now
	}
	// 有数据库：事务提交成功后才应用内存状态（含冷却清零与持久化），
	// 失败则内存保持原状，避免伪成功状态。
	if m.database != nil {
		if err := nodestore.UpdateDisabledBatch(m.database, "entry_nodes", effURIs, st); err != nil {
			log.Printf("[EntryNodes] BatchUpdateNodesDisabled 更新失败: %v", err)
			return
		}
	}
	m.applyEntryToggleUnsafe(targets, disabled, source, reason, class, now)
}

// BatchUpdateNodesLocked 批量设置/解除前置节点防删锁（locked）。
func (m *EntryManager) BatchUpdateNodesLocked(uris []string, locked bool) {
	if len(uris) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureEntryLoaded()
	targets := make(map[string]bool, len(uris))
	for _, u := range uris {
		targets[u] = true
	}
	if m.database == nil {
		for i, n := range m.entryList {
			if targets[n.RawURI] {
				m.entryList[i].Locked = locked
			}
		}
		return
	}
	if err := nodestore.UpdateLockedBatch(m.database, "entry_nodes", uris, locked); err != nil {
		log.Printf("[EntryNodes] BatchUpdateNodesLocked 更新失败: %v", err)
		return
	}
	for i, n := range m.entryList {
		if targets[n.RawURI] {
			m.entryList[i].Locked = locked
		}
	}
}

// applyEntryToggleUnsafe 在 DB 提交成功后应用内存状态：切换 disabled 标志与禁用审计，
// 启用时清零冷却、自动探测连败并持久化健康度（DB 失败路径不得调用，保证内存与 DB 一致）。
func (m *EntryManager) applyEntryToggleUnsafe(targets map[string]bool, disabled bool, source, reason, class string, now int64) {
	for i, n := range m.entryList {
		if !targets[n.RawURI] {
			continue
		}
		m.entryList[i].Disabled = disabled
		if disabled {
			m.entryList[i].DisableSource = source
			m.entryList[i].DisableReason = reason
			m.entryList[i].DisabledAt = now
			m.entryList[i].DisableFailureClass = class
		} else {
			m.clearEntryDisableAuditUnsafe(i)
		}
		if h, exists := m.entryHealthMap[n.RawURI]; exists {
			h.DisabledByHealth = disabled && nodestore.AutoRecoverable(source)
		}
	}
	if !disabled {
		for u := range targets {
			if h, exists := m.entryHealthMap[u]; exists {
				h.CooldownUntil = 0
				// 人工启用同时重置自动探测连败，避免刚启用就被上一轮连败再次判失效。
				h.AutoProbeFailStreak = 0
			}
		}
		m.saveEntryHealthUnsafe()
	}
}

// BatchDeleteNodes 批量删除前置节点。
func (m *EntryManager) BatchDeleteNodes(uris []string) {
	m.mu.Lock()
	m.ensureEntryLoaded()
	// 防删保护：locked 节点永不参与删除。
	locked := make(map[string]bool, len(m.entryList))
	for _, n := range m.entryList {
		if n.Locked {
			locked[n.RawURI] = true
		}
	}
	targets := make(map[string]bool)
	effURIs := make([]string, 0, len(uris))
	for _, u := range uris {
		if locked[u] {
			continue
		}
		targets[u] = true
		effURIs = append(effURIs, u)
		delete(m.entryHealthMap, u)
	}
	if len(effURIs) == 0 {
		m.mu.Unlock()
		return
	}
	var kept []Node
	for _, n := range m.entryList {
		if !targets[n.RawURI] {
			kept = append(kept, n)
		}
	}
	m.entryList = kept
	m.saveEntryNodesUnsafe()
	m.saveEntryHealthUnsafe()
	hooks := m.hooks
	m.mu.Unlock() // 先解锁再通知外部清理，避免卡死死锁

	if hooks.InvalidateParsed != nil {
		for _, u := range effURIs {
			hooks.InvalidateParsed(u)
		}
	}
}
