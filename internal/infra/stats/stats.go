// Package stats 提供全进程统一的请求统计收集器：
// 内存实时计数器 + 节点桥接注册表 + 异步批量落库到 db.UsageStore。
// 记录侧采用包级单例（与 internal/infra/cli 一致），供深层引擎与 API 层轻量调用；
// 查询侧则由 API 层通过注入的 *db.UsageStore 完成。
package stats

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

const (
	recordBuffer   = 8192
	flushBatchSize = 200
	flushInterval  = 3 * time.Second
	// retentionInterval 是保留窗口清理的触发周期（清理强度低，故与落库节奏解耦）。
	retentionInterval = time.Hour
	// retentionVacuumPages 是每轮保留清理后归还的空闲页上限（4KB/页 → 约 4MB/次）。
	// 取小值是为了让回收摊平到每小时，避免一次大回收长时间占用唯一连接。
	retentionVacuumPages = 1000
	// nodeTTL 是 reqID->winner node 映射的最大存活时间；
	// 超过该时长未取走的条目在后续写入时被惰性清理。
	nodeTTL = 10 * time.Minute
)

var (
	mu      sync.RWMutex
	usage   *db.UsageStore
	recCh   chan db.UsageRecord
	raceCh  chan db.RaceRecord
	stop    chan struct{}
	stopped chan struct{}
	started atomic.Bool
	// closeOnce 只保护"单次 Init→Close 周期"内的重复 Close。
	// 每次 Init 重置为新的 Once，使 Init/Close 可成对重复调用（测试常用；
	// 旧实现用包级 sync.Once，导致首个 Close 之后的 Close 恒为 no-op，
	// 后续 Init 起的 flushLoop 永不停机、started 也永远置不回 false）。
	closeOnce *sync.Once

	// retentionDays 是请求历史保留天数（默认 90，可由配置覆盖）。
	retentionDays atomic.Int64

	// raceLogRetentionDays 是 race_log 保留天数（默认 7，可由配置覆盖）。
	// 竞速明细量远大于 usage_log，单独用更短窗口控制库体积增长。
	raceLogRetentionDays atomic.Int64

	// dbMaintenance 是可选注入的数据库维护器（main 装配时注入）。保留清理后
	// 调用一次增量回收，把空闲页归还文件系统；未注入时整段跳过。
	dbMaintenance atomic.Pointer[db.DBMaintenance]

	// 实时计数器（Snapshot 即时返回，不触库）
	rtCounters struct {
		requests         atomic.Int64
		success          atomic.Int64
		errors           atomic.Int64
		promptTokens     atomic.Int64
		candidatesTokens atomic.Int64
		totalTokens      atomic.Int64
		dropped          atomic.Int64
		// 竞速实时计数（每次竞速候选结局累加）
		raceAttempts atomic.Int64
		raceWins     atomic.Int64
		raceTimeouts atomic.Int64
		raceErrors   atomic.Int64
		// raceRateLimits 是 429 正常竞争限流的独立计数：不算节点失败，不进 race_errors。
		raceRateLimits atomic.Int64
	}
	rtStarted time.Time

	// 节点桥接：reqID -> winner node name（竞速胜出时写入，handler 结束时取走）
	nodeMu sync.Mutex
	nodes  map[string]nodeEntry

	// 在途请求注册表：实时监控"当前连接中的每条请求"。
	activeMu    sync.RWMutex
	activeReqs  map[string]*ActiveRequest
	activeIDSeq int64
	// reqControls 是 reqID -> 取消控制块：管理页"强制停止"用。与 activeReqs 同生命周期，
	// 在 StopReq 里一并删除，避免两份状态不同步。
	reqControls map[string]*reqControl
)

// reqControl 是一条在途请求的取消控制块。
type reqControl struct {
	cancel context.CancelFunc
	// stopped 标记"已被管理员强制停止"：仅内存，用于把管理员停止与客户端断开区分开
	// （前者要给客户端一个明确错误，后者按既有语义静默结束）。
	stopped bool
}

// ActiveRequest 是实时监控中一条在途请求的状态快照。
type ActiveRequest struct {
	ID    int64  `json:"id"`
	ReqID string `json:"req_id"`
	Model string `json:"model"`
	// RawModel 是客户端请求的原始模型名（含 抗截断- / 假非流- 等变体前缀），
	// 与 Model（剥离前缀并解析别名后、实际发往上游的名字）可能不同。
	RawModel   string `json:"raw_model"`
	UseID      string `json:"useid"`
	APIKeyName string `json:"api_key_name"`
	NodeName   string `json:"node_name"`
	Status     string `json:"status"`
	Streaming  bool   `json:"streaming"`
	// ResponseMode 是客户端实际接收形态：增量 / 聚合 / 单次。
	// 上游侧恒为流式，故只有客户端侧才存在该维度。
	ResponseMode string `json:"response_mode"`
	// AntiTruncate 标记是否走 StreamChatCollect 竞速链路（首包锁定胜出）。
	AntiTruncate bool  `json:"anti_truncate"`
	StartedAt    int64 `json:"started_at"` // Unix 秒
	ElapsedMs    int64 `json:"elapsed_ms"`
	TTFBMs       int64 `json:"ttfb_ms"`
	// UpstreamTTFBMs 是上游首字耗时（请求开始 → 上游首个网络字节）。
	// 上游侧恒为流式，故该值对三个端点都有效；与 ttfb_ms（客户端首帧）之差即代理自身开销。
	UpstreamTTFBMs int64 `json:"upstream_ttfb_ms"`
	PromptTok      int   `json:"prompt_tokens"`
	CandTok        int   `json:"candidates_tokens"`
	TotalTok       int   `json:"total_tokens"`
	// success 用 *bool 表达"尚未结束"（null）。
	Success *bool `json:"success"`
}

type nodeEntry struct {
	name    string
	uri     string
	written time.Time
}

func init() {
	nodes = make(map[string]nodeEntry)
	activeReqs = make(map[string]*ActiveRequest)
	reqControls = make(map[string]*reqControl)
}

// Init 启动统计收集器：绑定持久化仓库并拉起后台批量落库 goroutine。
//
// 幂等：同一轮内多次调用只生效第一次。
// 生命周期契约：Close 之后可以再次 Init 并重新绑定到新的仓储（每次 Init 都会重建
// 通道与当轮的 closeOnce）。因此**需要重新绑定仓储时，必须先 Close 再 Init**——
// 直接 Init 会被这里的 started 判定挡掉并静默沿用旧仓储。
func Init(s *db.UsageStore) {
	mu.Lock()
	defer mu.Unlock()
	if started.Load() {
		return
	}
	usage = s
	recCh = make(chan db.UsageRecord, recordBuffer)
	raceCh = make(chan db.RaceRecord, recordBuffer)
	stop = make(chan struct{})
	stopped = make(chan struct{})
	closeOnce = &sync.Once{}
	rtStarted = time.Now()
	retentionDays.Store(90)
	raceLogRetentionDays.Store(7)
	started.Store(true)
	go flushLoop()
}

// SetRetentionDays 覆盖请求历史保留天数（默认 90），超出自动清理。
func SetRetentionDays(days int) {
	if days < 1 {
		return
	}
	retentionDays.Store(int64(days))
}

// RetentionDays 返回当前保留天数。
func RetentionDays() int {
	d := retentionDays.Load()
	if d < 1 {
		return 90
	}
	return int(d)
}

// SetRaceLogRetentionDays 覆盖 race_log 保留天数（默认 7，<1 时忽略）。
func SetRaceLogRetentionDays(days int) {
	if days < 1 {
		return
	}
	raceLogRetentionDays.Store(int64(days))
}

// RaceLogRetentionDays 返回当前 race_log 保留天数。
func RaceLogRetentionDays() int {
	d := raceLogRetentionDays.Load()
	if d < 1 {
		return 7
	}
	return int(d)
}

// SetDBMaintenance 注入数据库维护器（可选）。注入后，每轮保留清理会顺带归还
// 一小批空闲页，使库文件体积能随清理回落；未注入时不执行回收。
func SetDBMaintenance(m *db.DBMaintenance) {
	dbMaintenance.Store(m)
}

// Enabled 返回收集器是否已初始化。
func Enabled() bool {
	return started.Load()
}

// Record 记录一次请求。非阻塞：缓冲满时丢弃该条（不阻塞请求路径）。
// 仅当记录真正进入缓冲后才累加实时计数器，保证 Snapshot 与落库口径一致。
func Record(rec db.UsageRecord) {
	if !started.Load() {
		return
	}
	select {
	case recCh <- rec:
		rtCounters.requests.Add(1)
		if rec.Success {
			rtCounters.success.Add(1)
		} else {
			rtCounters.errors.Add(1)
		}
		rtCounters.promptTokens.Add(int64(rec.PromptTokens))
		rtCounters.candidatesTokens.Add(int64(rec.CandidatesTokens))
		rtCounters.totalTokens.Add(int64(rec.TotalTokens))
	default:
		rtCounters.dropped.Add(1)
	}
}

// RecordRace 记录一次竞速候选结局。非阻塞：缓冲满时丢弃该条（不阻塞竞速路径）。
// Outcome 取值：win / collect / timeout / error。
func RecordRace(rec db.RaceRecord) {
	if !started.Load() {
		return
	}
	select {
	case raceCh <- rec:
		rtCounters.raceAttempts.Add(1)
		switch rec.Outcome {
		case "win":
			rtCounters.raceWins.Add(1)
		case "timeout":
			rtCounters.raceTimeouts.Add(1)
		case "error":
			rtCounters.raceErrors.Add(1)
		case "ratelimit":
			// 429 正常竞争限流：计入 attempts 便于观测频次，但不进失败计数。
			rtCounters.raceRateLimits.Add(1)
		}
	default:
		rtCounters.dropped.Add(1)
	}
}

// RecordRaceOutcome 以原语形式记录一次竞速候选结局，供引擎层调用而不引入 db 依赖。
// outcome 取值：win / collect / timeout / error。
func RecordRaceOutcome(reqID, nodeName, nodeURI, outcome string, elapsedMs int64) {
	if !started.Load() || nodeName == "" {
		return
	}
	RecordRace(db.RaceRecord{
		TS:        time.Now().Unix(),
		ReqID:     reqID,
		NodeName:  nodeName,
		NodeURI:   nodeURI,
		Outcome:   outcome,
		ElapsedMs: elapsedMs,
	})
}

// RecordNode 由竞速引擎在胜出/归属时写入该请求的节点展示名与原始 URI。
// nodeURI 可为空（直连或前置代理无出口节点身份），此时按名称统计。
func RecordNode(reqID, nodeName, nodeURI string) {
	if !started.Load() || reqID == "" || nodeName == "" {
		return
	}
	nodeMu.Lock()
	defer nodeMu.Unlock()
	now := time.Now()
	// 惰性清理过期条目，防止长时间运行的 reqID 集合无限膨胀
	for k, e := range nodes {
		if now.Sub(e.written) > nodeTTL {
			delete(nodes, k)
		}
	}
	nodes[reqID] = nodeEntry{name: nodeName, uri: nodeURI, written: now}
}

// TakeNodeInfo 取走（并删除）某请求的获胜节点名与原始 URI，供 handler 汇总使用。
func TakeNodeInfo(reqID string) (name, uri string) {
	if !started.Load() || reqID == "" {
		return "", ""
	}
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if e, ok := nodes[reqID]; ok {
		delete(nodes, reqID)
		return e.name, e.uri
	}
	return "", ""
}

// StartReq 在请求开始时登记到在途注册表（供请求监控实时展示）。
func StartReq(reqID, model, useid, apiKeyName string, streaming bool) {
	if !started.Load() || reqID == "" {
		return
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	activeIDSeq++
	activeReqs[reqID] = &ActiveRequest{
		ID:         activeIDSeq,
		ReqID:      reqID,
		Model:      model,
		UseID:      useid,
		APIKeyName: apiKeyName,
		Status:     "处理中",
		Streaming:  streaming,
		StartedAt:  time.Now().Unix(),
	}
}

// UpdateReq 在请求进行中更新在途状态：可更新模型/节点/TTFB/token/状态。
func UpdateReq(reqID string, fn func(a *ActiveRequest)) {
	if !started.Load() || reqID == "" || fn == nil {
		return
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	if a, ok := activeReqs[reqID]; ok {
		fn(a)
	}
}

// StopReq 在请求结束时从在途注册表移除（成功/失败均移除），并清掉取消控制块。
func StopReq(reqID string) {
	if !started.Load() || reqID == "" {
		return
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	delete(activeReqs, reqID)
	delete(reqControls, reqID)
}

// SetReqCancel 登记一条在途请求的取消函数，供管理页"强制停止"使用。
// 必须在 StartReq 之后调用；未登记的请求无法被停止（CancelReq 返回 false）。
func SetReqCancel(reqID string, cancel context.CancelFunc) {
	if !started.Load() || reqID == "" || cancel == nil {
		return
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	if _, ok := activeReqs[reqID]; !ok {
		return
	}
	reqControls[reqID] = &reqControl{cancel: cancel}
}

// CancelReq 强制停止一条在途请求：先标记"被管理员停止"再取消其上下文，
// 使请求返回时能给出可区分的明确错误。请求已结束（或未登记取消函数）时返回 false。
func CancelReq(reqID string) bool {
	if !started.Load() || reqID == "" {
		return false
	}
	activeMu.Lock()
	ctl, ok := reqControls[reqID]
	if ok {
		ctl.stopped = true
	}
	activeMu.Unlock()
	if !ok || ctl.cancel == nil {
		return false
	}
	ctl.cancel()
	return true
}

// ReqStoppedByAdmin 报告一条在途请求是否已被管理员强制停止。
// 请求结束时条目被移除，故只在处理期间（即需要决定返回什么错误时）有意义。
func ReqStoppedByAdmin(reqID string) bool {
	if !started.Load() || reqID == "" {
		return false
	}
	activeMu.RLock()
	defer activeMu.RUnlock()
	ctl, ok := reqControls[reqID]
	return ok && ctl.stopped
}

// ActiveRequests 返回在途请求的快照列表（按开始时间倒序，供请求监控实时展示）。
func ActiveRequests() []ActiveRequest {
	activeMu.RLock()
	defer activeMu.RUnlock()
	out := make([]ActiveRequest, 0, len(activeReqs))
	now := time.Now().Unix()
	for _, a := range activeReqs {
		cp := *a
		cp.ElapsedMs = (now - cp.StartedAt) * 1000
		out = append(out, cp)
	}
	// 按 StartedAt 倒序（最近开始在前）；同秒内按登记 ID 倒序保证确定性。
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt > out[j].StartedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Snapshot 返回实时计数器快照（自 Init 以来的累计值）。
func Snapshot() map[string]any {
	return map[string]any{
		"requests":          rtCounters.requests.Load(),
		"success":           rtCounters.success.Load(),
		"errors":            rtCounters.errors.Load(),
		"prompt_tokens":     rtCounters.promptTokens.Load(),
		"candidates_tokens": rtCounters.candidatesTokens.Load(),
		"total_tokens":      rtCounters.totalTokens.Load(),
		"dropped":           rtCounters.dropped.Load(),
		"started_at":        rtStarted.Unix(),
		"race_attempts":     rtCounters.raceAttempts.Load(),
		"race_wins":         rtCounters.raceWins.Load(),
		"race_timeouts":     rtCounters.raceTimeouts.Load(),
		"race_errors":       rtCounters.raceErrors.Load(),
		"race_rate_limits":  rtCounters.raceRateLimits.Load(),
	}
}

// Close 停止收集器并排空剩余缓冲。
//
// 幂等：同一轮（同一次 Init 之后）内重复调用只生效一次。
// 与 Init 成对可重复：Close 完成后 started 归位 false，可再次 Init 绑定新仓储。
// 注意 Once 必须是"每轮一份"（closeOnce 指针，由 Init 重置）——若用包级 sync.Once，
// 首个 Close 之后的 Close 会恒为 no-op，started 再也置不回 false，单例就此不可重绑。
func Close() {
	mu.RLock()
	once := closeOnce
	stopCh := stop
	stoppedCh := stopped
	mu.RUnlock()
	if !started.Load() || once == nil {
		return
	}
	once.Do(func() {
		close(stopCh)
		<-stoppedCh
		started.Store(false)
	})
}

// flushLoop 周期性地将缓冲中的记录批量落库；Close 时排空剩余数据。
// 同时周期清理超出保留窗口的旧记录，防止 usage_log 无界增长。
func flushLoop() {
	buf := make([]db.UsageRecord, 0, flushBatchSize)
	raceBuf := make([]db.RaceRecord, 0, flushBatchSize)
	flush := func() {
		if len(buf) > 0 {
			batch := buf
			buf = make([]db.UsageRecord, 0, flushBatchSize)
			if usage != nil {
				_ = usage.InsertBatch(batch) // 落库失败静默丢弃，避免阻塞与日志风暴
			}
		}
		if len(raceBuf) > 0 {
			rbatch := raceBuf
			raceBuf = make([]db.RaceRecord, 0, flushBatchSize)
			if usage != nil {
				_ = usage.InsertRaceBatch(rbatch)
			}
		}
	}
	// retentionTicker 触发保留窗口清理（与落库节奏解耦，避免高频扫描）。
	retentionTicker := time.NewTicker(retentionInterval)
	defer retentionTicker.Stop()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case rec := <-recCh:
			buf = append(buf, rec)
			if len(buf) >= flushBatchSize {
				flush()
			}
		case rec := <-raceCh:
			raceBuf = append(raceBuf, rec)
			if len(raceBuf) >= flushBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-retentionTicker.C:
			now := time.Now().Unix()
			if usage != nil {
				_, _ = usage.ResetOlderThan(now - int64(RetentionDays())*86400)
				_, _ = usage.ResetRaceOlderThan(now - int64(RaceLogRetentionDays())*86400)
			}
			// 故障记录固定保留 1 天（评审要求），与日志表的可配置保留期分开清理。
			// 有意放在 usage 判空之外：它由 DBMaintenance 持有句柄，与统计仓储是否装配无关——
			// 否则"没启用统计"的部署会静默地永不清理故障表。
			if m := dbMaintenance.Load(); m != nil {
				_, _ = m.PruneFaultLog(now)
				// DELETE 只把页放进 freelist，文件不会自行收缩。库的
				// auto_vacuum=INCREMENTAL 时，主动归还一小批空闲页（约 4MB/次），
				// 让体积随清理回落；未开启该模式时 SQLite 忽略本指令，无副作用。
				_, _ = m.IncrementalVacuum(retentionVacuumPages)
			}
		case <-stop:
			// 排空剩余
			for {
				select {
				case rec := <-recCh:
					buf = append(buf, rec)
				case rec := <-raceCh:
					raceBuf = append(raceBuf, rec)
				default:
					flush()
					close(stopped)
					return
				}
			}
		}
	}
}
