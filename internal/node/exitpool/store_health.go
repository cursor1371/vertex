package exitpool

import (
	"log"
	"math"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// ---- 健康度更新与测速结果落库 ----

// rT 健康度：只影响 **rT 抓取的选点**（SelectForTokenFetch），不参与普通请求选点、
// 也不禁用节点——"对 rT 坏"不等于"节点坏"，普通请求的候选池不该被 rT 问题侵蚀。
const (
	// rtSlowMsThreshold 是"对 rT 慢"的判定阈值（毫秒）：最近一次 rT 抓取耗时超过它即在
	// rT 选点里降权（排到同层最后），好节点不足时仍会被用到。
	rtSlowMsThreshold = 5000
	// rtHangCooldownBaseSec/rtHangCooldownMaxSec 是挂住型的 rT 专属冷却：按挂住连败升级
	// （30s→60s→120s→…→封顶 600s），一次成功即复位。只避让 rT 抓取，不禁用节点。
	rtHangCooldownBaseSec = 30
	rtHangCooldownMaxSec  = 600
)

// TokenFetchOutcome 是 rT 抓取结果的归因类别（决定 rT 健康度如何更新）。
type TokenFetchOutcome int

const (
	// TokenFetchOK 抓取成功：rT 连败清零、记录耗时、清 rT 冷却。
	TokenFetchOK TokenFetchOutcome = iota
	// TokenFetchFailed 节点级失败（连接被拒 / 解析失败 / HTTP 非 200 等）：rT 连败累加，
	// 使该节点在 rT 选点里降权。
	TokenFetchFailed
	// TokenFetchHang 挂住型失败（预算到点且一个尝试都没跑完）：rT 连败累加，并按连败
	// 升级 rT 专属冷却——只让 rT 抓取避开它，普通请求照旧可用。
	TokenFetchHang
	// TokenFetchSlow 慢到超时（预算到点但尝试有完成）：同样计入 rT 健康度（记耗时、
	// 连败），但不算"挂住"、不升级冷却——它是预算不够或节点慢，不是卡死。
	TokenFetchSlow
)

// rtHangCooldownSec 按挂住连败返回 rT 冷却秒数（30s 起、逐次翻倍、封顶 600s）。
func rtHangCooldownSec(streak int) int {
	if streak < 1 {
		streak = 1
	}
	sec := rtHangCooldownBaseSec << (streak - 1)
	if sec <= 0 || sec > rtHangCooldownMaxSec {
		sec = rtHangCooldownMaxSec
	}
	return sec
}

type healthEvent struct {
	uri string
	// rh 只携带落库字段：拷贝发生在 m.mu 内。此前 healthEvent{h: *h} 整体拷贝
	// NodeHealth，对 InFlight（锁外 atomic 写，见 store_mem.go Inc/DecInFlight）
	// 构成非原子读，-race 下真实可报；InFlight 本就不落库，绝不拷它。
	rh nodestore.HealthRow
	// hasHealth 区分"只写健康行"与"只写故障记录"两类事件：两者共用同一条 worker 与
	// 队列，避免在请求热路径上再开一条写库通道。
	hasHealth bool
	// fault 非 nil 时随本次事件落一条故障记录（已脱敏，见 nodestore.MaskSecrets）。
	fault *nodestore.FaultRow
}

// ProbeResult 是一次节点探测/失败上报的入参。统一入口的价值在于：分类、记账、故障记录、
// 禁用决策在同一处持锁完成，不会出现"某个测试入口另起一套规则"的口径分裂。
type ProbeResult struct {
	OK     bool
	Ms     float64
	ErrStr string
	// Class 是调用方已知的失败类别（如请求路径从 VertexError.Kind 得出）。零值时由 ErrStr 分类。
	Class nodestore.FailureClass
	// Target 是探测目标（域名/主机），进故障记录用于定位；不知道时留空。
	Target string
	// Source 是探测来源（nodestore.ProbeSource*），进故障记录用于区分发现渠道。
	Source string
}

// StartHealthWorker 启动健康度异步写 worker（每 Manager 一条）。幂等：重复调用
// 是空操作；进程关闭前须调用 StopHealthWorker 排空。
func (m *Manager) StartHealthWorker() {
	if !m.workerStarted.CompareAndSwap(false, true) {
		return
	}
	m.healthWG.Add(1)
	go func() {
		defer m.healthWG.Done()
		for {
			select {
			case ev := <-m.queue:
				m.applyHealthEvent(ev)
			case <-m.healthStop:
				// 排空残留事件再退出：优雅关闭不丢最后一批测速/竞速健康记账。
				for {
					select {
					case ev := <-m.queue:
						m.applyHealthEvent(ev)
					default:
						return
					}
				}
			}
		}
	}()
}

// applyHealthEvent 消费一条健康事件：健康行与故障记录互不依赖，各自按需落库。
func (m *Manager) applyHealthEvent(ev healthEvent) {
	if ev.hasHealth {
		m.upsertHealthRow(ev.uri, ev.rh)
	}
	if ev.fault != nil {
		m.upsertFaultRow(*ev.fault)
	}
}

// StopHealthWorker 停止健康写 worker 并等待其排空队列。幂等；未启动时也安全
// （只关 stop 通道、Wait 立即返回）。须在 database.Close 之前调用。
func (m *Manager) StopHealthWorker() {
	m.workerStopOnce.Do(func() { close(m.healthStop) })
	m.healthWG.Wait()
}

func (m *Manager) pushHealthEvent(uri string, h *NodeHealth) {
	if h == nil {
		return
	}
	select {
	case m.queue <- healthEvent{uri: uri, rh: healthRowOf(h), hasHealth: true}:
	default:
		log.Printf("[Health] 警告: 健康度异步写队列已满，丢弃事件 %s", uri)
	}
}

// pushFaultEvent 入队一条故障记录（复用健康写队列，队满即丢弃并记日志）。
func (m *Manager) pushFaultEvent(f nodestore.FaultRow) {
	select {
	case m.queue <- healthEvent{uri: f.NodeURI, fault: &f}:
	default:
		log.Printf("[Health] 警告: 健康度异步写队列已满，丢弃故障记录 %s/%s", f.NodeURI, f.FailureClass)
	}
}

// healthRowOf 提取落库字段。调用方须持 m.mu：NodeHealth 除 InFlight（锁外 atomic、
// 不落库）外的字段全部由 mu 保护。
func healthRowOf(h *NodeHealth) nodestore.HealthRow {
	return nodestore.HealthRow{
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
	}
}

// upsertHealthRow 把一条健康快照落库（worker 专用；uri 为真源，rh.RawURI 忽略）。
//
// URI 存活校验：事件入队与消费之间节点可能已被删除/去重，而 UpsertHealth 是
// INSERT OR REPLACE（SQLite 未启用外键，DELETE 不级联）——不校验会把已删节点的
// 健康行复活成孤儿。以内存 healthMap 为准（删除路径同步移除条目）；同 URI 重新
// 导入的场景旧事件写到新条目上属可接受的最终一致。
func (m *Manager) upsertHealthRow(uri string, rh nodestore.HealthRow) {
	if m.database == nil {
		return
	}
	m.mu.RLock()
	_, alive := m.healthMap[uri]
	m.mu.RUnlock()
	if !alive {
		return
	}
	rh.RawURI = uri
	_ = nodestore.UpsertHealth(m.database, "node_health", rh)
}

// upsertFaultRow 落一条故障记录（worker 专用）。与健康行不同，故障记录不做存活校验：
// 节点已被删除时，保留"它当初为什么坏"的记录恰恰是排查所需（且 1 天后自动清理）。
func (m *Manager) upsertFaultRow(f nodestore.FaultRow) {
	if m.database == nil {
		return
	}
	_ = nodestore.RecordFault(m.database, f)
}

// nodeByURISeek 在 nodeList 中找 uri 对应节点的下标（调用方须持锁）；找不到返回 -1。
func (m *Manager) nodeByURISeek(uri string) int {
	for i := range m.nodeList {
		if m.nodeList[i].RawURI == uri {
			return i
		}
	}
	return -1
}

// syncDisableAuditUnsafe 把一次禁用/启用写进内存节点的审计字段，并同步落库。
// 调用方须持锁。所有改变 disabled 状态的路径都必须经过它，否则只有
// Node.Disabled 变化而 DisableSource 仍是旧值，面板会显示错误来源（例如把人工禁用
// 显示成"健康禁用、可自动恢复"）。
func (m *Manager) syncDisableAuditUnsafe(uri string, disabled bool, source, reason, class string, now int64) {
	idx := m.nodeByURISeek(uri)
	if idx < 0 {
		return
	}
	m.nodeList[idx].Disabled = disabled
	if disabled {
		m.nodeList[idx].DisableSource = source
		m.nodeList[idx].DisableReason = reason
		m.nodeList[idx].DisabledAt = now
		m.nodeList[idx].DisableFailureClass = class
	} else {
		// 启用即清空审计：残留的"上次为何被禁"比没有更误导。
		m.nodeList[idx].DisableSource = nodestore.DisableSourceManual
		m.nodeList[idx].DisableReason = ""
		m.nodeList[idx].DisabledAt = 0
		m.nodeList[idx].DisableFailureClass = ""
	}
	if h, ok := m.healthMap[uri]; ok {
		h.DisabledByHealth = disabled && nodestore.AutoRecoverable(source)
	}
	m.persistDisableUnsafe(uri)
}

// persistDisableUnsafe 按当前内存态落库节点的禁用状态与审计字段。调用方须持锁。
func (m *Manager) persistDisableUnsafe(uri string) {
	idx := m.nodeByURISeek(uri)
	if idx < 0 {
		return
	}
	m.persistDisableAtUnsafe(idx)
}

// persistDisableAtUnsafe 是 persistDisableUnsafe 的下标版本：选点循环里已有下标，
// 再按 URI 线性搜一遍会让选点退化成 O(n²)。调用方须持锁。
func (m *Manager) persistDisableAtUnsafe(idx int) {
	n := m.nodeList[idx]
	byHealth := false
	if h, ok := m.healthMap[n.RawURI]; ok {
		byHealth = h.DisabledByHealth
	}
	_ = nodestore.UpsertDisabled(m.database, "nodes", n.RawURI, nodestore.DisableState{
		Disabled:         n.Disabled,
		DisabledByHealth: byHealth,
		Source:           n.DisableSource,
		Reason:           n.DisableReason,
		DisabledAt:       n.DisabledAt,
		FailureClass:     n.DisableFailureClass,
	})
}

// EnableNode 人工启用节点：清健康/冷却并把禁用审计归零（启用后不再有"为何被禁"）。
// 返回是否找到该节点。
func (m *Manager) EnableNode(uri string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	if m.nodeByURISeek(uri) < 0 {
		return false
	}
	if h, exists := m.healthMap[uri]; exists {
		h.CooldownUntil = 0
		h.LastSubHealthyAt = 0
		// 人工启用同时重置自动探测连败：否则节点刚启用就被"上一轮探测的连败"再次判永久。
		h.AutoProbeFailStreak = 0
		m.pushHealthEvent(uri, h)
	}
	m.syncDisableAuditUnsafe(uri, false, nodestore.DisableSourceManual, "", "", time.Now().Unix())
	return true
}

// RecordTokenFetch 记录一次 rT token 抓取结果（由 recaptcha 抓取路径上报，
// 见 recaptcha.NodeSource）。uri 为空（直连/前置）时调用方不上报，此处防御性忽略。
// ms 是本次抓取（该节点这次尝试）的耗时，供 rT 选点判断"对 rT 慢"。
//
// **只写 rT 专属字段**（TokenFailStreak / RTHangStreak / RTLastMs / RTCooldownUntil）：
//   - 不写 Disabled：rT 坏 ≠ 节点坏。禁用是全局的，会让一个 GraphQL 可用的节点白白退出
//     普通请求的候选池，还要人工恢复；rT 的问题应该只让 rT 抓取避开它。
//   - 不写 LastSubHealthyAt / CooldownUntil：那是普通请求选点（Tier 2 与冷却）的输入，
//     写它会顺带影响普通请求。
//   - 用独立的 TokenFailStreak 计数、成功一次即清零：不能复用 ConsecutiveFailures，
//     否则"节点对 GraphQL 正常、只对 rT 端点坏"会被请求路径的成功反复重置。
//
// 降权与冷却由 SelectForTokenFetch 读取（见 rtRankOf）。
func (m *Manager) RecordTokenFetch(uri string, outcome TokenFetchOutcome, ms int64, errStr string) {
	if uri == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	h, exists := m.healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		m.healthMap[uri] = h
	}
	now := time.Now().Unix()
	h.RTLastMs = ms
	if outcome == TokenFetchOK {
		h.TokenFailStreak = 0
		h.RTHangStreak = 0
		h.RTCooldownUntil = 0
		h.LastSuccessAt = now
		m.pushHealthEvent(uri, h)
		return
	}

	h.TokenFailStreak++
	h.FailCount++
	h.LastFailAt = now
	h.LastTestError = nodestore.MaskSecrets("rT 抓取失败: " + errStr)
	if outcome == TokenFetchHang {
		// 挂住是"节点卡死"：按连败升级 rT 专属冷却（30s 起、封顶 600s）。
		h.RTHangStreak++
		cool := rtHangCooldownSec(h.RTHangStreak)
		h.RTCooldownUntil = now + int64(cool)
		log.Printf("[Health] 节点 %s rT 抓取挂住（第 %d 次），rT 冷却 %d 秒（仅 rT 选点避让，不禁用）",
			uri, h.RTHangStreak, cool)
	} else if outcome == TokenFetchSlow {
		// 慢到超时：只记连败与耗时，让它在 rT 选点里降权；不按挂住升级冷却。
		log.Printf("[Health] 节点 %s rT 抓取超时（%dms，第 %d 次连败），rT 选点降权（不禁用）",
			uri, ms, h.TokenFailStreak)
	}
	m.pushHealthEvent(uri, h)
}

// TokenFetchRank 返回该节点当前的 rT 降权层（0 健康 / 1 差 / 2 冷却中），
// 供 recaptcha 记录流展示"这次尝试时它是否已被降权"。
func (m *Manager) TokenFetchRank(uri string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	return m.rtRankOf(uri, time.Now().Unix())
}

// RTRanks 返回全部节点的 rT 降权层快照（uri -> rank，与 rtRankOf 同判据）。
// 批量接口：管理页节点列表需要全量 rank 做筛选/排序，逐 URI 调 TokenFetchRank
// 会对每个节点各抢一次锁。
func (m *Manager) RTRanks() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	now := time.Now().Unix()
	out := make(map[string]int, len(m.healthMap))
	for uri := range m.healthMap {
		out[uri] = m.rtRankOf(uri, now)
	}
	return out
}

// rtRankOf 返回节点在 rT 选点里的降权层：
//
//	0 = rT 健康（无连败、无挂住、最近耗时未超阈值）
//	1 = rT 差（有连败/挂住，或最近耗时超 rtSlowMsThreshold）
//	2 = 处于 rT 专属冷却中
//
// 层号只影响**排序**（排到同层最后），好节点不足时仍会回退到它们——保证不会因为 rT
// 质量问题彻底抓不到 token。调用方须持锁。
func (m *Manager) rtRankOf(uri string, now int64) int {
	h := m.healthMap[uri]
	if h == nil {
		return 0
	}
	if h.RTCooldownUntil > now {
		return 2
	}
	if h.TokenFailStreak > 0 || h.RTHangStreak > 0 || h.RTLastMs > rtSlowMsThreshold {
		return 1
	}
	return 0
}

// RecordTest 记录一次节点测速结果（兼容入口，等价于来源为 request 的 ReportProbe）。
// 保留它是因为请求路径、竞速引擎与既有测试都按此签名调用。
func (m *Manager) RecordTest(uri string, ok bool, ms float64, errStr string) {
	m.ReportProbe(uri, ProbeResult{OK: ok, Ms: ms, ErrStr: errStr, Source: nodestore.ProbeSourceRequest})
}

// ReportProbe 是统一的上报入口：分类 → 记账 → 全局故障保护 → 阈值禁用 → 故障记录，
// 全部在一次持锁内完成。返回本次是否真的触发了禁用。
//
// 分类来源优先级：调用方显式给出的 Class（请求路径从 VertexError.Kind 得知，比文本可靠）
// 优先于对 ErrStr 的文本分类。
func (m *Manager) ReportProbe(uri string, r ProbeResult) (disabled bool) {
	if uri == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	h, exists := m.healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		m.healthMap[uri] = h
	}
	now := time.Now().Unix()
	h.LastTestMs = r.Ms
	// 落库/下发前统一脱敏：这段文本既会进健康表，也会原样显示在管理面板的状态列，
	// 而上游错误常带完整 URL（query 里可能有 key/token）。域名、状态码、错误类型等
	// 定位信息在 MaskSecrets 里全部保留，只替换凭据本身。
	h.LastTestError = nodestore.MaskSecrets(r.ErrStr)
	h.LastTestAt = now

	if r.OK {
		h.SuccessCount++
		h.ConsecutiveFailures = 0
		h.LastSuccessAt = now
		wasSubHealthy := h.LastSubHealthyAt > 0
		h.LastSubHealthyAt = 0
		h.CooldownUntil = 0
		h.Last429At = 0
		h.RateLimitCount = 0
		// 探测成功即清零自动探测连败：这是"达阈值转永久禁用"唯一的复位途径。
		h.AutoProbeFailStreak = 0
		h.LastFailureClass = ""
		h.LastHTTPStatus = 0
		if wasSubHealthy {
			log.Printf("[Health] 节点 %s 恢复为健康 (延迟: %.0fms)", uri, r.Ms)
		}
		// 自动恢复仅限"健康判定触发"的禁用（DisableSource == health）；人工禁用、
		// admin 测试禁用、永久禁用都不会被自动放开。
		if idx := m.nodeByURISeek(uri); idx >= 0 && m.nodeList[idx].Disabled &&
			nodestore.AutoRecoverable(m.nodeList[idx].DisableSource) {
			m.syncDisableAuditUnsafe(uri, false, nodestore.DisableSourceManual, "", "", now)
			log.Printf("[Health] 节点 %s 探测成功，自动解除健康触发的禁用", uri)
		}
		m.pushHealthEvent(uri, h)
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

	// 故障记录（脱敏原文 + 聚合签名）无条件入队：即使该类失败不参与禁用，
	// 管理员也需要能看到"到底在报什么错"，这正是评审要求的可展开故障详情。
	m.pushFaultEvent(nodestore.FaultRow{
		Scope:        "exit",
		NodeURI:      uri,
		NodeName:     m.nodeNameMap[uri],
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

	pol := m.policyFor(info.Class)

	// 全局故障保护：多个不同节点同因失败时判定为疑似公共依赖/环境故障。
	// 语义锁死为"只防、不治"——命中**只阻止后续节点被摘除**（照常记账），
	// 不解除、不恢复任何已存在的禁用状态。
	globalFault := false
	if info.Class.GuardsAgainstGlobalFault() && m.globalFault != nil {
		globalFault = m.globalFault.Observe(info.Class, uri, now)
		if globalFault {
			log.Printf("[Health] 疑似公共依赖/环境故障（类别 %s），节点 %s 本次不摘除；继续记账，已禁用节点不受影响",
				info.Class, m.nodeNameMap[uri])
		}
	}

	// 首次移出可用池由**策略**决定，不受任何全局阈值门控：
	//   - record_only        → 只记账（顺带写亚健康降权），不摘出；
	//   - single_shot        → 摘出且不自动恢复（来源 single_shot，人工启用）；
	//   - disable_then_probe → 摘出并进入自动探测（来源 health，探测成功恢复）。
	// 防删锁不豁免——防删保护的是误删误禁用，而"这个节点实际连不通"必须摘出轮换。
	if pol.DisablesOnFirstFailure() && !globalFault {
		reason := disableReasonOf(pol, info, r)
		if m.disableByPolicyLocked(uri, reason, string(info.Class), pol, now) {
			disabled = true
		}
	} else {
		h.LastSubHealthyAt = now
	}

	// 自动探测的失败升级：同一节点被自动探测连续判失败达阈值 → 转永久禁用。
	// 只对"策略仍要求探测恢复"的类别升级；阈值 0 表示无限探测（永不因探测失败转永久）。
	// 也不累加非探测来源的失败——请求路径的抖动不该把节点推向永久禁用。
	if r.Source == nodestore.ProbeSourceAutoProbe && pol.ProbesForRecovery() {
		h.AutoProbeFailStreak++
		if pol.ProbeFailThreshold > 0 && h.AutoProbeFailStreak >= pol.ProbeFailThreshold {
			m.markPermanentLocked(uri, "自动探测连续 "+strconv.Itoa(h.AutoProbeFailStreak)+
				" 次失败（"+string(info.Class)+"，阈值 "+strconv.Itoa(pol.ProbeFailThreshold)+
				"），判定为持续不可恢复", now)
		}
	}
	m.pushHealthEvent(uri, h)
	return disabled
}

// disableReasonOf 拼装可读的禁用原因（面板直接展示，不含凭据——原文已脱敏）。
//
// 有意不含"当前连败次数"：那个数字每次都变，写进原因会让禁用后的每次失败都重写
// 一次库（幂等失效）。原因描述的是"为什么禁"，连败次数在健康记录的
// consecutive_failures 里另有一份权威值。
func disableReasonOf(pol nodestore.FaultPolicy, info nodestore.FailureInfo, r ProbeResult) string {
	reason := "命中策略「" + nodestore.PolicyActionLabel(pol.Action) + "」（" + string(info.Class) + "）"
	if pol.ProbesForRecovery() {
		if pol.ProbeFailThreshold > 0 {
			reason += "，探测失败 " + strconv.Itoa(pol.ProbeFailThreshold) + " 次后转永久禁用"
		} else {
			reason += "，无限探测（不因探测失败转永久）"
		}
	}
	if info.Code != "" {
		reason += "（错误码 " + info.Code + "）"
	}
	if r.Target != "" {
		reason += " 目标 " + r.Target
	}
	if info.Detail != "" {
		reason += "：" + info.Detail
	}
	return reason
}

// disableByPolicyLocked 按策略执行一次禁用：置 Disabled、写审计来源与原因。
// 必须持有 m.mu。返回是否真的发生了状态变更（供 ReportProbe 的返回值使用）。
//
// 来源由动作决定：disable_then_probe → health（可被探测恢复）；
// single_shot → single_shot（不自动恢复，只能人工启用）。
//
// **不回溯原则**（评审定稿：策略变更不隐式改变存量节点状态）：
//   - 已由人工 / admin 测试 / 永久 / 单次禁用：完全不动——自动路径既不得把不可恢复的来源
//     降级成可恢复（那样会悄悄放开永久禁用），也不得把单次禁用升级成可恢复；
//   - 已是 health 且策略仍为 disable_then_probe：仅刷新原因（保留"最近一次真实原因"）；
//   - 已是 health 但策略已改为不禁用 / 单次禁用：不动。
//
// 幂等：状态无变化时跳过落库，避免反复失败产生成串重复 UPDATE。
func (m *Manager) disableByPolicyLocked(uri, reason, class string, pol nodestore.FaultPolicy, now int64) bool {
	idx := m.nodeByURISeek(uri)
	if idx < 0 {
		return false
	}
	if m.nodeList[idx].Locked {
		log.Printf("[Health] 节点 %s 已防删锁定，但策略判定失败仍按规则禁用", uri)
	}
	prevDisabled := m.nodeList[idx].Disabled
	prevSource := m.nodeList[idx].DisableSource
	prevReason := m.nodeList[idx].DisableReason
	if prevDisabled {
		if !nodestore.AutoRecoverable(prevSource) {
			return false
		}
		if !pol.ProbesForRecovery() || prevReason == reason {
			return false
		}
		m.syncDisableAuditUnsafe(uri, true, nodestore.DisableSourceHealth, reason, class, now)
		return true
	}
	source := nodestore.DisableSourceSingleShot
	if pol.ProbesForRecovery() {
		source = nodestore.DisableSourceHealth
	}
	m.syncDisableAuditUnsafe(uri, true, source, reason, class, now)
	log.Printf("[Health] 节点 %s 命中策略「%s」，已移出可用池（来源 %s）: %s",
		uri, nodestore.PolicyActionLabel(pol.Action), source, reason)
	return true
}

// DisableByHealth 按**策略**禁用一批节点（供竞速引擎等已明确归因的路径调用）。
//
// 与 BatchUpdateNodesDisabled（人工禁用语义）相对：本入口按该类别的策略决定来源——
// disable_then_probe → health（探测成功自动恢复）；single_shot → 不自动恢复。
// 因此用户把 auth 配成"单次禁用"时，这里的语义随之改变（策略统一生效，不在代码里分叉）。
// reasonClass 为归因类别，用于查策略与写审计。
func (m *Manager) DisableByHealth(uris []string, reasonClass nodestore.FailureClass) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	now := time.Now().Unix()
	pol := m.policyFor(reasonClass)
	if !pol.DisablesOnFirstFailure() {
		return
	}
	reason := "命中策略「" + nodestore.PolicyActionLabel(pol.Action) + "」（" + string(reasonClass) + "）"
	for _, uri := range uris {
		m.disableByPolicyLocked(uri, reason, string(reasonClass), pol, now)
	}
}

// MarkPermanentDisabled 把节点从"健康禁用"升级为**永久禁用**（自动探测连败达阈值）：
// 记 permanent 来源、不再进入自动探测名单，只能人工启用。
// 返回是否真的执行了升级（false 表示节点不存在，或已是非健康来源而不该被本路径改写）。
func (m *Manager) MarkPermanentDisabled(uri, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	return m.markPermanentLocked(uri, reason, time.Now().Unix())
}

// markPermanentLocked 是 MarkPermanentDisabled 的持锁实现。只升级"健康禁用"来源：
// 人工禁用、admin 测试禁用与协议不支持的禁用不允许被自动探测改写来源。
func (m *Manager) markPermanentLocked(uri, reason string, now int64) bool {
	idx := m.nodeByURISeek(uri)
	if idx < 0 {
		return false
	}
	if m.nodeList[idx].DisableSource != nodestore.DisableSourceHealth {
		return false
	}
	m.syncDisableAuditUnsafe(uri, true, nodestore.DisableSourcePermanent, reason, string(nodestore.FailureNodeNetwork), now)
	log.Printf("[Health] 节点 %s 自动探测连续失败达阈值，转为永久禁用（需人工启用）: %s", uri, reason)
	return true
}

// HealthAutoDisabledURIs 返回当前可由自动探测恢复的节点 URI 集合（加锁快照）：
// 只含来源为 health 的禁用节点。permanent（观测已证明它恢复不了）、admin 测试禁用、
// 人工禁用与协议不支持都不在其中——自动策略不得反复重试已被判定无望的节点，
// 更不得擅自放开管理员有意禁用的节点。
func (m *Manager) HealthAutoDisabledURIs() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	out := make(map[string]bool)
	for _, n := range m.nodeList {
		if n.Disabled && nodestore.AutoRecoverable(n.DisableSource) {
			out[n.RawURI] = true
		}
	}
	return out
}

// DisableSourceOf 返回节点的禁用来源（未禁用或节点不存在时返回空串），
// 供调用方在一次探测前后对比状态变化（例如自动探测据此判断是否发生了恢复/永久化）。
func (m *Manager) DisableSourceOf(uri string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	idx := m.nodeByURISeek(uri)
	if idx < 0 || !m.nodeList[idx].Disabled {
		return nodestore.DisableSourceManual
	}
	return m.nodeList[idx].DisableSource
}

// ---- 故障处理策略的读写与装配（管理面板「处理策略」用）----
//
// 策略表由出口池代为持有：它持有 *sql.DB，且两个池共享同一个 PolicySet 实例，
// 因此这里的每次写入都会即时刷新解析器 —— 用户改完策略下一次失败判定就生效，无需重启。

// PolicySet 返回策略解析器（未注入时返回进程级默认集，调用方无需 nil 判断）。
func (m *Manager) PolicySet() *nodestore.PolicySet {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.policies != nil {
		return m.policies
	}
	return nodestore.DefaultPolicySet()
}

// PolicyRows 读取全部自定义策略行（面板展示"来源：自定义/默认"用）。
func (m *Manager) PolicyRows() ([]nodestore.PolicyRow, error) {
	return nodestore.LoadPolicies(m.database)
}

// UpsertPolicy 写入/更新一条自定义策略并即时刷新解析器。
func (m *Manager) UpsertPolicy(row nodestore.PolicyRow) error {
	if err := nodestore.UpsertPolicy(m.database, row); err != nil {
		return err
	}
	m.reloadPoliciesUnsafe()
	return nil
}

// DeletePolicy 删除某类别的自定义行（回落内置默认）并即时刷新解析器。
func (m *Manager) DeletePolicy(class nodestore.FailureClass) error {
	if err := nodestore.DeletePolicy(m.database, class); err != nil {
		return err
	}
	m.reloadPoliciesUnsafe()
	return nil
}

// ResetPolicies 清空全部自定义策略行并即时刷新解析器，返回删除行数。
func (m *Manager) ResetPolicies() (int64, error) {
	n, err := nodestore.ResetPolicies(m.database)
	if err != nil {
		return 0, err
	}
	m.reloadPoliciesUnsafe()
	return n, nil
}

// reloadPoliciesUnsafe 从库重新载入自定义策略行并替换解析器内容。
// DB 不可用或读取失败时保持现有内容不变（不静默清空用户的策略）。
func (m *Manager) reloadPoliciesUnsafe() {
	m.mu.RLock()
	ps := m.policies
	m.mu.RUnlock()
	if ps == nil {
		return
	}
	rows, err := nodestore.LoadPolicies(m.database)
	if err != nil {
		log.Printf("[Nodes] 重新载入故障处理策略失败，沿用当前生效值: %v", err)
		return
	}
	ps.Replace(nodestore.PolicyRowsToSet(rows))
}

// ---- 故障记录查询（管理面板「故障统计」用）----

// 故障表 node_fault_log 由两个池共享写入（scope 列区分 exit/entry），查询统一走出口池：
// 它持有同一份 *sql.DB，按 scope 过滤交给调用方（面板要一次看到全部故障）。
// 查询路径不加 m.mu：读的是库不是内存态，加锁只会与记账路径互相拖慢。

// NodeFaults 返回按最近发生时间倒序的故障记录（limit<=0 取默认上限）。
func (m *Manager) NodeFaults(limit int) ([]nodestore.FaultRow, error) {
	return nodestore.LoadFaults(m.database, limit)
}

// FaultTotals 返回故障表的全量总量（聚合行数 + 累计发生次数），供面板显示真实总量。
func (m *Manager) FaultTotals() (int64, int64, error) {
	return nodestore.FaultTotals(m.database)
}

// FaultClassStats 按失败类别汇总故障记录（含"其他"分组），供面板分类统计。
func (m *Manager) FaultClassStats() ([]nodestore.FaultClassStats, error) {
	return nodestore.FaultClassCounts(m.database)
}

// ClearFaults 清空故障记录，返回删除行数。
func (m *Manager) ClearFaults() (int64, error) {
	return nodestore.ClearFaultLog(m.database)
}

// GlobalFaultActive 返回当前疑似全局故障的类别与窗口内节点数（供面板展示），
// 无保护器时返回空 map。
func (m *Manager) GlobalFaultActive() map[string]int {
	if m.globalFault == nil {
		return map[string]int{}
	}
	return m.globalFault.Active(time.Now().Unix())
}

// SetGlobalFaultGuard 注入全局故障保护器（装配期调用；为 nil 时该项保护停用）。
func (m *Manager) SetGlobalFaultGuard(g *nodestore.GlobalFaultGuard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.globalFault = g
}

// RecordRateLimit 记录 429 短时冷却：只做避让与独立计次，不写任何失败/降权字段。
//
// 429 是上游正常竞争限流，节点本身没有故障：计入失败（LastFailAt/LastTestError）
// 会污染健康记账并让冷却过期后状态显示错乱；写 LastSubHealthyAt 属惩罚性降权，
// 且只能靠成功测速清除、冷却过期也不恢复。避让由 CooldownUntil 承担（选点严格
// 通道跳过冷却节点），频次观测由 Last429At/RateLimitCount 承担。
// 流式空闲超时（慢/僵死节点）的避让走 RecordSlowAvoid——那是节点劣化信号，语义不同。
func (m *Manager) RecordRateLimit(uri string, cooldownSec int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	h, exists := m.healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		m.healthMap[uri] = h
	}
	h.Last429At = time.Now().Unix()
	h.RateLimitCount++
	h.CooldownUntil = time.Now().Unix() + int64(cooldownSec)
	m.pushHealthEvent(uri, h)
}

// RecordSlowAvoid 记录流式包间空闲超时（慢/僵死节点）的短时避让：除冷却外还写
// 亚健康降权（Tier 2）与失败记账——挂住是节点自身劣化信号，需要排序靠后避免
// 反复选中，与 429 的"正常竞争"语义不同。
func (m *Manager) RecordSlowAvoid(uri string, cooldownSec int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	h, exists := m.healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		m.healthMap[uri] = h
	}
	now := time.Now().Unix()
	h.LastSubHealthyAt = now
	h.LastTestError = "stream idle timeout"
	h.LastFailAt = now
	h.CooldownUntil = now + int64(cooldownSec)
	m.pushHealthEvent(uri, h)
}

// ---- 节点选择与排序 ----

// getNodeTier 返回节点选点分层：1 健康 / 2 亚健康或冷却中 / 3 禁用。
// 冷却中至少 Tier 2：严格与宽松通道都经 pickFromGroupCooldown 跳过冷却节点——
// 该避让此前隐式依赖"429/idle 会写 LastSubHealthyAt"，429 语义纯化（不再写失败/
// 降权字段）后必须显式判 CooldownUntil，否则 429 冷却节点留在 Tier 1 被照常选中。
// 冷却过期自动回 Tier 1：429 属正常竞争，不做冷却之外的持续惩罚。
func getNodeTier(n Node, h *NodeHealth, now int64) int {
	if n.Disabled {
		return 3
	}
	if h != nil {
		if h.CooldownUntil > now {
			return 2
		}
		if h.LastSubHealthyAt > 0 {
			return 2
		}
	}
	return 1
}

type tierCandidate struct {
	node     Node
	inFlight int32
	// rtRank 是 rT 选点专用的降权层（见 rtRankOf）。tokenFetch=false 时恒为 0 且不参与
	// 比较，保证普通请求的选点行为逐字节不变。
	rtRank int
}

// SelectForParallel 按健康分层严格选点：Tier 2 亚健康节点处于 429 冷却期时被跳过。
func (m *Manager) SelectForParallel(k int, debugMode bool) []Node {
	return m.selectForParallelCore(k, debugMode, 0, false)
}

// SelectForTokenFetch 是 **rT 抓取专用**选点：与 SelectForParallel 同一套分层/排序，
// 额外把"对 rT 差/慢"的节点按 rtRankOf 排到同层最后、并在 rT 冷却期内靠后（仍可回退），
// 从而让 rT 抓取自动避开坏节点，而**不影响普通请求的选点**（rT 失败也不再禁用节点）。
func (m *Manager) SelectForTokenFetch(k int, debugMode bool) []Node {
	return m.selectForParallelCore(k, debugMode, 0, true)
}

// SelectForParallelRelaxed 宽松选点：仅忽略"剩余冷却 ≤ 5 秒"的节点——冷却临近结束
// （如 30s 冷却已过 25s）可被提前拉起补位，避免刚 429 的节点被立即重打。
// 仍在长冷却（剩余 > 5s）中的节点一律跳过，防止宽松通道无限重打限流节点。
// Disabled 节点仍然排除；Tier 排序、InFlight/LastSelectedAt 权衡与 Phase 3 保护逻辑与严格路径一致。
func (m *Manager) SelectForParallelRelaxed(k int, debugMode bool) []Node {
	return m.selectForParallelCore(k, debugMode, 5, false)
}

// pickFromGroup 在同一个 (rank, inFlight) 组内按 rrIndex 轮转取节点，直到取满 k。
//
// 关键：轮转游标每次只推进 1，因此**不能**对每个 rank 组各取一个——那会让 rank=2
// （rT 冷却中）的节点每轮都被选中。正确做法是外层按 rank 逐级调用本函数（见
// selectForParallelCore），rank 0 取满 k 之后才轮到 rank 1/2。
func pickFromGroup(selected *[]Node, k int, group []tierCandidate, rrIndex *uint64) {
	offset := int(atomic.AddUint64(rrIndex, 1)) % len(group)
	for l := 0; l < len(group) && len(*selected) < k; l++ {
		idx := (offset + l) % len(group)
		*selected = append(*selected, group[idx].node)
	}
}

// pickFromGroupCooldown 与 pickFromGroup 同，额外跳过仍在冷却中的节点（Tier 2 用）。
func pickFromGroupCooldown(selected *[]Node, k int, group []tierCandidate, rrIndex *uint64, cooldownBefore int64, health map[string]*NodeHealth) {
	offset := int(atomic.AddUint64(rrIndex, 1)) % len(group)
	for l := 0; l < len(group) && len(*selected) < k; l++ {
		idx := (offset + l) % len(group)
		if h := health[group[idx].node.RawURI]; h != nil && h.CooldownUntil > cooldownBefore {
			continue
		}
		*selected = append(*selected, group[idx].node)
	}
}

// selectForParallelCore 是选点核心。tokenFetch=true 时启用 rT 降权层（rtRank）：
// 它作为**第一排序键**，让"对 rT 差/冷却中"的节点排到同层最后（好节点不足时仍会回退），
// 并按 (rtRank, InFlight) 分组轮转；同时跳过 Phase 3 的 5 秒新鲜度替换——那会重新把
// 被降权的节点换回来。tokenFetch=false 时这些分支全部不生效，行为与改动前完全一致。
func (m *Manager) selectForParallelCore(k int, debugMode bool, cooldownGraceSec int, tokenFetch bool) []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	now := time.Now().Unix()

	var tier1 []tierCandidate
	var tier2 []tierCandidate

	for i, n := range m.nodeList {
		if n.Disabled {
			continue
		}
		if m.identity != nil && !m.identity.Supported(n.RawURI) {
			// 不支持的协议不参与选点；防删锁定节点保留其 Disabled 状态不被改写。
			// 来源记 unsupported：它不是健康判定触发的，自动探测不会去恢复它
			//（解析能力没变化时重测也不会变支持），但审计里能看出"为什么被禁"。
			if !n.Locked {
				m.nodeList[i].Disabled = true
				m.nodeList[i].DisableSource = nodestore.DisableSourceUnsupported
				m.nodeList[i].DisableReason = "协议不支持或解析失败，无法参与调度"
				m.nodeList[i].DisabledAt = now
				m.nodeList[i].DisableFailureClass = string(nodestore.FailureUnsupported)
				m.persistDisableAtUnsafe(i)
			}
			continue
		}
		h := m.healthMap[n.RawURI]
		tier := getNodeTier(n, h, now)
		inFlight := int32(0)
		if h != nil {
			inFlight = atomic.LoadInt32(&h.InFlight)
		}
		rtRank := 0
		if tokenFetch {
			rtRank = m.rtRankOf(n.RawURI, now)
		}
		switch tier {
		case 1:
			tier1 = append(tier1, tierCandidate{n, inFlight, rtRank})
		case 2:
			tier2 = append(tier2, tierCandidate{n, inFlight, rtRank})
		}
	}

	// Sort Tier 1 by InFlight ascending, then by LastSelectedAt ascending (least recently used first), then by URI for deterministic round-robin grouping
	// rT 选点额外把 rtRank 作为第一排序键（见 rtRankOf）：差的排后，好节点优先。
	sort.Slice(tier1, func(i, j int) bool {
		if tokenFetch && tier1[i].rtRank != tier1[j].rtRank {
			return tier1[i].rtRank < tier1[j].rtRank
		}
		if tier1[i].inFlight != tier1[j].inFlight {
			return tier1[i].inFlight < tier1[j].inFlight
		}
		hi := m.healthMap[tier1[i].node.RawURI]
		hj := m.healthMap[tier1[j].node.RawURI]
		ti := int64(0)
		if hi != nil {
			ti = hi.LastSelectedAt
		}
		tj := int64(0)
		if hj != nil {
			tj = hj.LastSelectedAt
		}
		if ti != tj {
			return ti < tj
		}
		return tier1[i].node.RawURI < tier1[j].node.RawURI
	})

	var selected []Node
	i := 0
	for i < len(tier1) && len(selected) < k {
		curInFlight := tier1[i].inFlight
		curRank := tier1[i].rtRank
		j := i
		for j < len(tier1) && tier1[j].inFlight == curInFlight &&
			(!tokenFetch || tier1[j].rtRank == curRank) {
			j++
		}
		group := tier1[i:j]
		pickFromGroup(&selected, k, group, &m.rrIndex)
		i = j
	}

	// Phase 2: Tier 2 fallback when Tier 1 is insufficient
	if len(selected) < k {
		sort.Slice(tier2, func(i, j int) bool {
			if tokenFetch && tier2[i].rtRank != tier2[j].rtRank {
				return tier2[i].rtRank < tier2[j].rtRank
			}
			if tier2[i].inFlight != tier2[j].inFlight {
				return tier2[i].inFlight < tier2[j].inFlight
			}
			hi := m.healthMap[tier2[i].node.RawURI]
			hj := m.healthMap[tier2[j].node.RawURI]
			si := int64(0)
			if hi != nil {
				si = hi.LastSelectedAt
			}
			sj := int64(0)
			if hj != nil {
				sj = hj.LastSelectedAt
			}
			if si != sj {
				return si < sj
			}
			ti := int64(0)
			tj := int64(0)
			if hi != nil {
				ti = hi.LastSubHealthyAt
			}
			if hj != nil {
				tj = hj.LastSubHealthyAt
			}
			return ti < tj
		})

		i := 0
		for i < len(tier2) && len(selected) < k {
			curInFlight := tier2[i].inFlight
			curRank := tier2[i].rtRank
			j := i
			for j < len(tier2) && tier2[j].inFlight == curInFlight &&
				(!tokenFetch || tier2[j].rtRank == curRank) {
				j++
			}
			group := tier2[i:j]
			// 冷却剩余超过 grace 的节点跳过（strict grace=0 等价"剩余>0 即跳过"；
			// relaxed grace=5 仅容忍临近结束的节点提前补位）。
			pickFromGroupCooldown(&selected, k, group, &m.rrIndex, now+int64(cooldownGraceSec), m.healthMap)
			i = j
		}
	}

	// Phase 3: 5 秒全局保护缓冲（尽力而为）。
	// rT 选点跳过它：该替换只按"新鲜度"挑 Tier-1 节点，会把刚被 rtRank 降权/冷却的节点
	// 又换回来，正好抵消 rT 排序的意义；rT 的分散由 rtRank + InFlight 记账承担。
	findFreshReplacement := func(selected []Node, tier1 []tierCandidate) *Node {
		if tokenFetch {
			return nil
		}
		selectedSet := make(map[string]bool, len(selected))
		for _, s := range selected {
			selectedSet[s.RawURI] = true
		}
		var best *tierCandidate
		for i := range tier1 {
			tc := &tier1[i]
			if selectedSet[tc.node.RawURI] {
				continue
			}
			h := m.healthMap[tc.node.RawURI]
			if h == nil {
				if best == nil || tc.inFlight < best.inFlight {
					best = tc
				}
				continue
			}
			if h.CooldownUntil > now+int64(cooldownGraceSec) {
				continue
			}
			if h.LastSelectedAt == 0 || now-h.LastSelectedAt >= 5 {
				if best == nil || tc.inFlight < best.inFlight {
					best = tc
				}
			}
		}
		if best != nil {
			return &best.node
		}
		return nil
	}
	for idx := range selected {
		h := m.healthMap[selected[idx].RawURI]
		if h == nil || h.LastSelectedAt == 0 {
			continue
		}
		if now-h.LastSelectedAt >= 5 {
			continue
		}
		replacement := findFreshReplacement(selected, tier1)
		if replacement != nil {
			selected[idx] = *replacement
		}
	}

	for _, s := range selected {
		if h := m.healthMap[s.RawURI]; h != nil {
			h.LastSelectedAt = now
			h.RecentUseCount++
		}
	}

	if debugMode {
		log.Printf("[Nodes] 选择并行节点 (需求: %d, 实际: %d)", k, len(selected))
	}
	return selected
}

func (m *Manager) SortNodesByLatency() {
	m.mu.Lock()
	m.ensureLoaded()

	sort.Slice(m.nodeList, func(i, j int) bool {
		n1 := m.nodeList[i]
		n2 := m.nodeList[j]

		// 禁用的排在最后面
		if n1.Disabled != n2.Disabled {
			return !n1.Disabled
		}

		h1 := m.healthMap[n1.RawURI]
		h2 := m.healthMap[n2.RawURI]

		val1 := math.MaxFloat64
		if h1 != nil {
			if h1.ConsecutiveFailures > 0 {
				val1 = 1e6 + float64(h1.ConsecutiveFailures)*1000
			} else if h1.LastTestMs > 0 {
				val1 = h1.LastTestMs
			}
		}

		val2 := math.MaxFloat64
		if h2 != nil {
			if h2.ConsecutiveFailures > 0 {
				val2 = 1e6 + float64(h2.ConsecutiveFailures)*1000
			} else if h2.LastTestMs > 0 {
				val2 = h2.LastTestMs
			}
		}

		// 延迟一致的按名字自然排序
		if val1 == val2 {
			return n1.Name < n2.Name
		}
		return val1 < val2
	})

	m.saveNodesUnsafe()
	m.mu.Unlock()
}

func (m *Manager) SortNodesByLatencyDesc() {
	m.mu.Lock()
	m.ensureLoaded()

	sort.Slice(m.nodeList, func(i, j int) bool {
		n1 := m.nodeList[i]
		n2 := m.nodeList[j]

		// 禁用的排在最后面
		if n1.Disabled != n2.Disabled {
			return !n1.Disabled
		}

		h1 := m.healthMap[n1.RawURI]
		h2 := m.healthMap[n2.RawURI]

		val1 := math.MaxFloat64
		if h1 != nil {
			if h1.ConsecutiveFailures > 0 {
				val1 = 1e6 + float64(h1.ConsecutiveFailures)*1000
			} else if h1.LastTestMs > 0 {
				val1 = h1.LastTestMs
			}
		}

		val2 := math.MaxFloat64
		if h2 != nil {
			if h2.ConsecutiveFailures > 0 {
				val2 = 1e6 + float64(h2.ConsecutiveFailures)*1000
			} else if h2.LastTestMs > 0 {
				val2 = h2.LastTestMs
			}
		}

		// 延迟一致的按名字自然排序
		if val1 == val2 {
			return n1.Name < n2.Name
		}
		// 这里改为降序，val1 > val2
		return val1 > val2
	})

	m.saveNodesUnsafe()
	m.mu.Unlock()
}
