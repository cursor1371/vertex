package cli

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/rivo/uniseg"
)

type ReqState struct {
	ID    string
	Model string
	// RawModel 是客户端请求的原始模型名（含 抗截断- / 假非流- 等变体前缀），
	// 与实际发往上游的 Model（剥离前缀并解析别名后）可能不同。
	RawModel   string
	State      string
	Color      string
	WinnerNode string
	Detail     string
	StartTime  time.Time
	// Streaming 标记该请求是否走流式端点；PromptTok/CandTok/TotalTok 为用量
	// （流式的 usageMetadata 只在终帧出现，故须保留已完成行才看得到）。
	Streaming bool
	// ResponseMode 是客户端实际接收形态：增量 / 聚合 / 单次。
	// 上游侧恒为流式，故只有客户端侧才存在该维度。
	ResponseMode string
	// AntiTruncate 标记是否走 StreamChatCollect 竞速链路（首包锁定胜出）。
	AntiTruncate bool
	PromptTok    int
	CandTok      int
	TotalTok     int
	// UpstreamTTFBMs 是上游首字节耗时（毫秒）：上游侧恒为流式，故三个端点都有效。
	// 不再记录客户端首帧耗时——非流式/聚合流下它与总耗时相同，并列只会冗余。
	// 为 0 表示尚未采到（Time 列首字显示 "-"）。
	UpstreamTTFBMs int64
	// Truncation 是上游主动异常终止的展示标注（如"输出上限截断"/"工具调用畸形"）。
	// 空串表示正常收尾。由 handler 传入已映射好的文案，故 cli 包无需依赖 transform。
	// 只做标注、不改判 Success：上游撞输出上限仍是 HTTP 成功。
	Truncation string
	// Finished/Success/FinishAt 支撑"完成后短暂保留"：请求结束时不再立即删除，
	// 否则 token 用量与最终成败会在同一瞬间消失、根本来不及看。
	Finished bool
	Success  bool
	FinishAt time.Time
}

// PoolStats 是 RT 预取池的展示快照。由 main 装配的 provider 提供，
// 使 cli 包无需反向依赖引擎层。
type PoolStats struct {
	Size     int
	Depth    int
	Hits     int64
	Misses   int64
	Expired  int64
	Failures int64
	// Paused 是"无收益自停"状态：池满后出现过期 token 即暂停补货，
	// 新的连接请求一到达即立刻恢复；Pauses 为累计暂停次数。
	Paused bool
	Pauses int64
	// PrefetchEnabled 是预取总开关：关闭时不展示池汇总（池恒为空无意义），
	// 但 Recent 记录流仍然有效——请求路径照样抓 token。
	PrefetchEnabled bool
	// Recent 是最近的 rT token 抓取记录（newest first），含请求路径与后台预取。
	Recent []TokenEvent
}

// TokenEvent 是 rT token 抓取记录流的展示视图。
type TokenEvent struct {
	At        time.Time
	Source    string
	Node      string
	LatencyMs int64
	OK        bool
	Err       string
	// Canceled 表示本次抓取被 ctx 取消（竞速其它候选胜出 / 请求中断），不是失败。
	Canceled bool
	// RTRank 是选点时刻该节点的 rT 降权层（0 健康 / 1 差 / 2 冷却中）。
	RTRank int
}

const (
	// finishedRetention 是已完成请求在追踪器中的保留时长。
	finishedRetention = 60 * time.Second
	// maxFinishedRows 是保留的已完成行数上限（超出时淘汰最早的）。
	maxFinishedRows = 20
)

const (
	minTermWidth = 60
	maxLogs      = 10
	// poolStatsSampleInterval 是预取池面板的采样间隔（仅 TUI 启用时生效）。
	poolStatsSampleInterval = 500 * time.Millisecond
)

var (
	//nolint:gochecknoglobals
	program atomic.Pointer[tea.Program]
	//nolint:gochecknoglobals
	programDone chan struct{}
	//nolint:gochecknoglobals
	enabled atomic.Bool
	//nolint:gochecknoglobals
	additionalLogWriter io.Writer
)

func sendMsg(msg tea.Msg) {
	if !enabled.Load() {
		return
	}
	p := program.Load()
	if p == nil {
		return
	}
	p.Send(msg)
}

// logQueueSize 是日志异步队列容量；满时丢弃（仅在事件循环已异常时发生）。
const logQueueSize = 256

// logQueue 是"日志 → TUI"的异步投递队列。
//
// 日志调用方可能正持有任意锁（例如 config.Load() 在持有配置锁时打日志），而
// Program.Send 对无缓冲的 msgs 通道是阻塞的。一旦在 TUI 事件循环内触发日志，
// 就会形成"事件循环等锁、锁等事件循环"的自锁，并把该锁永久占住——整个进程随之冻结
// （实测症状：在设置页修改预取深度并保存后卡死）。
// 因此日志一律走本队列：写方永不阻塞，队列满则丢弃。
//
//nolint:gochecknoglobals // 进程级单例，与 TUI program 同生命周期
var logQueue = make(chan tea.Msg, logQueueSize)

// sendLog 非阻塞投递一条日志消息。
func sendLog(msg tea.Msg) {
	select {
	case logQueue <- msg:
	default:
	}
}

// drainLogQueue 把日志队列投递给 TUI。独立 goroutine，可安全阻塞在 p.Send。
func drainLogQueue(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case msg := <-logQueue:
			sendMsg(msg)
		}
	}
}

func SetAppInfo(ver, commit, bTime, goos, goarch string) {
	sendMsg(setAppInfoMsg{
		version:      ver,
		buildInfo:    fmt.Sprintf("Build: %s / %s", commit, bTime),
		platformInfo: fmt.Sprintf("Platform: %s/%s", goos, goarch),
	})
	sendMsg(logLineMsg(fmt.Sprintf("[vproxy] 启动成功: Version=%s, Commit=%s, Built=%s", ver, commit, bTime)))
	sendMsg(logLineMsg(fmt.Sprintf("[vproxy] 运行平台: %s/%s", goos, goarch)))
}

func SetInitialAdminPassword(pw string) {
	sendMsg(setAdminPasswordMsg(pw))
}

func IsTUIEnabled() bool {
	return enabled.Load()
}

func InitTracker(fileLogger io.Writer) {
	additionalLogWriter = fileLogger
	programDone = make(chan struct{})

	fileInfo, err := os.Stdout.Stat()
	if err == nil && (fileInfo.Mode()&os.ModeCharDevice) != 0 {
		enabled.Store(true)
		p := tea.NewProgram(
			TuiModel{
				width:      80,
				height:     24,
				activeReqs: make(map[string]*ReqState),
			},
			tea.WithoutSignalHandler(),
		)
		program.Store(p)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[TUI] Bubble Tea 运行崩溃: %v", r)
				}
			}()
			_, _ = p.Run()
			close(programDone)
		}()

		log.SetOutput(logInterceptor{})
		startPoolStatsSampler()
		go drainLogQueue(programDone)
	} else {
		if fileLogger != nil {
			log.SetOutput(io.MultiWriter(os.Stderr, fileLogger))
		}
	}
}

// startPoolStatsSampler 启动预取池快照采样器（仅 TUI 启用时生效）。
//
// 采样必须由独立 goroutine 完成：TUI 事件循环内绝不能读取配置——在设置页保存会
// 让配置缓存失效，随后的 config.Load() 会在持有配置锁时打日志，而日志又要发消息给
// TUI，于是"事件循环等配置锁、配置锁等事件循环"互相等待，整个进程冻结且配置锁永不
// 释放（实测症状：修改预取深度保存后卡死）。采样器在事件循环之外，p.Send 可正常投递。
func startPoolStatsSampler() {
	done := programDone
	if done == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(poolStatsSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				st, ok := currentPoolStats()
				sendMsg(poolStatsMsg{stats: st, ok: ok})
			}
		}
	}()
}

func StopTUI() {
	if !enabled.Load() {
		return
	}
	p := program.Load()
	if p == nil {
		return
	}
	p.Quit()
	<-programDone
	enabled.Store(false)
}

func TUIDone() <-chan struct{} {
	return programDone
}

func StartReq(id string, streaming bool) {
	sendMsg(startReqMsg{id: id, streaming: streaming})
}

// UpdateReqModel 更新该请求实际发往上游的模型名。
func UpdateReqModel(id, model string) {
	sendMsg(updateReqModelMsg{id: id, model: model})
}

// UpdateReqTrace 补齐请求的追踪三元组：客户端原始模型名、客户端实际接收形态、
// 是否走 StreamChatCollect 竞速链路。
//
// 与 UpdateReqModel 分开：三个值在 pipeline 不同阶段才可知（抗截断/强制工具调用
// 需在请求体解析并固化上下文后才确定），合并成一条消息反而无法表达时序。
func UpdateReqTrace(id, rawModel, responseMode string, antiTruncate bool) {
	sendMsg(updateReqTraceMsg{
		id: id, rawModel: rawModel, responseMode: responseMode, antiTruncate: antiTruncate,
	})
}

func UpdateReqState(id, state, color, detail string) {
	sendMsg(updateReqStateMsg{id: id, state: state, color: color, detail: detail})
}

func UpdateReqWinner(id, nodeName string) {
	sendMsg(updateReqWinnerMsg{id: id, nodeName: nodeName})
}

func FinishReq(id string, success bool) {
	sendMsg(finishReqMsg{id: id, success: success})
}

// UpdateReqUsage 更新该请求的输入/输出 token 用量（请求收尾拿到 usageMetadata 时调用）。
func UpdateReqUsage(id string, prompt, cand, total int) {
	sendMsg(updateReqUsageMsg{id: id, prompt: prompt, cand: cand, total: total})
}

// UpdateReqTTFB 更新该请求的上游首字节耗时（毫秒）。
// 与 UpdateReqUsage 分开：首字在首个 chunk 到达时即可知，早于收尾用量。
func UpdateReqTTFB(id string, upstreamMs int64) {
	sendMsg(updateReqTTFBMsg{id: id, upstreamMs: upstreamMs})
}

// UpdateReqTruncation 记录该请求上游主动异常终止的展示标注（空串表示正常收尾）。
// 只做标注、不改判成败：上游撞输出上限仍是 HTTP 成功，语义为"成功(输出上限截断)"。
// 必须在中间件的 FinishReq 之前调用（handler 的 defer 先于中间件的 defer 执行），
// 否则收尾状态会把它覆盖掉。
func UpdateReqTruncation(id, label string) {
	sendMsg(updateReqTruncationMsg{id: id, label: label})
}

// UpdateReqStreaming 修正该请求的流式标记：handler 内已知端点，比中间件的路径
// 推断更权威（中间件处 body 尚未解析）。
func UpdateReqStreaming(id string, streaming bool) {
	sendMsg(updateReqStreamMsg{id: id, streaming: streaming})
}

// poolStatsProvider 由 main 装配：返回 RT 预取池快照与是否启用（未装配=不展示）。
// 采用"拉取"而非"推送"：TUI 每帧读取一次，避免预取池为展示而写日志刷屏。
//
//nolint:gochecknoglobals // 进程级单例，与 TUI program 同生命周期
var poolStatsProvider atomic.Value

// SetPoolStatsProvider 注册预取池快照提供者；传 nil 表示关闭展示。
func SetPoolStatsProvider(fn func() (PoolStats, bool)) {
	if fn == nil {
		poolStatsProvider.Store(func() (PoolStats, bool) { return PoolStats{}, false })
		return
	}
	poolStatsProvider.Store(fn)
}

// currentPoolStats 返回预取池快照；未注册或未启用时 ok=false。
func currentPoolStats() (PoolStats, bool) {
	v := poolStatsProvider.Load()
	if v == nil {
		return PoolStats{}, false
	}
	fn, ok := v.(func() (PoolStats, bool))
	if !ok || fn == nil {
		return PoolStats{}, false
	}
	return fn()
}

type logInterceptor struct{}

func (logInterceptor) Write(p []byte) (int, error) {
	if additionalLogWriter != nil {
		_, _ = additionalLogWriter.Write(p)
	}
	text := strings.TrimSpace(string(p))
	if text != "" {
		// 异步投递：绝不阻塞可能正持有锁的日志调用方（见 logQueue 说明）。
		sendLog(logLineMsg(stripANSI(text)))
	}
	return len(p), nil
}

func stringWidth(s string) int {
	return uniseg.StringWidth(s)
}

func padOrTrunc(s string, maxCol int) string {
	w := uniseg.StringWidth(s)
	if w <= maxCol {
		return s + strings.Repeat(" ", maxCol-w)
	}
	if maxCol <= 2 {
		return ".."
	}
	var sb strings.Builder
	cur := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		gw := g.Width()
		if cur+gw > maxCol-2 {
			break
		}
		sb.WriteString(g.Str())
		cur += gw
	}
	sb.WriteString("..")
	cur += 2
	if cur < maxCol {
		sb.WriteString(strings.Repeat(" ", maxCol-cur))
	}
	return sb.String()
}

func wordWrap(text string, maxCol int) []string {
	if maxCol <= 0 {
		return []string{text}
	}
	w := uniseg.StringWidth(text)
	if w <= maxCol {
		return []string{text + strings.Repeat(" ", maxCol-w)}
	}

	g := uniseg.NewGraphemes(text)
	type graphemeItem struct {
		str   string
		width int
	}
	var items []graphemeItem
	for g.Next() {
		items = append(items, graphemeItem{
			str:   g.Str(),
			width: g.Width(),
		})
	}

	var lines []string
	var cur strings.Builder
	curW := 0

	i := 0
	for i < len(items) {
		item := items[i]
		gw := item.width

		if item.str == " " && curW > 0 {
			nextW := 0
			j := i + 1
			for j < len(items) && items[j].str != " " {
				nextW += items[j].width
				j++
			}
			if curW+1+nextW > maxCol {
				lines = append(lines, cur.String()+strings.Repeat(" ", maxCol-curW))
				cur.Reset()
				curW = 0
				i++
				continue
			}
		}

		if curW+gw > maxCol {
			lines = append(lines, cur.String()+strings.Repeat(" ", maxCol-curW))
			cur.Reset()
			curW = 0
		}

		cur.WriteString(item.str)
		curW += gw
		i++
	}

	if cur.Len() > 0 {
		lines = append(lines, cur.String()+strings.Repeat(" ", maxCol-curW))
	}
	if len(lines) == 0 {
		lines = []string{strings.Repeat(" ", maxCol)}
	}
	return lines
}

func dashBar(w int) string {
	if w <= 0 {
		return ""
	}
	return strings.Repeat("─", w)
}
