package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
)

var ansiRegexp = regexp.MustCompile(`\033\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

var spinnerFrames = []rune(`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`)

type TuiModel struct {
	logBuffer            []string
	activeReqs           map[string]*ReqState
	appVersion           string
	buildInfo            string
	platformInfo         string
	initialAdminPassword string
	spinnerIdx           int
	width                int
	height               int
	scrollOffset         int
	// poolStats/poolEnabled 是 RT 预取池的展示快照与开关态（每帧拉取）。
	poolStats   PoolStats
	poolEnabled bool
}

type (
	logLineMsg  string
	startReqMsg struct {
		id        string
		streaming bool
	}
	finishReqMsg struct {
		id      string
		success bool
	}
	updateReqModelMsg struct{ id, model string }
	updateReqTraceMsg struct {
		id           string
		rawModel     string
		responseMode string
		antiTruncate bool
	}
	updateReqStateMsg  struct{ id, state, color, detail string }
	updateReqWinnerMsg struct{ id, nodeName string }
	updateReqUsageMsg  struct {
		id                  string
		prompt, cand, total int
	}
	updateReqStreamMsg struct {
		id        string
		streaming bool
	}
	updateReqTTFBMsg struct {
		id         string
		upstreamMs int64
	}
	updateReqTruncationMsg struct {
		id    string
		label string
	}
	poolStatsMsg struct {
		stats PoolStats
		ok    bool
	}
	spinnerTickMsg      struct{}
	setAppInfoMsg       struct{ version, buildInfo, platformInfo string }
	setAdminPasswordMsg string
)

func (m TuiModel) Init() tea.Cmd {
	return tea.Batch(
		tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg {
			return spinnerTickMsg{}
		}),
	)
}

func (m TuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinnerTickMsg:
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		m.pruneFinished()
		return m, tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg {
			return spinnerTickMsg{}
		})

	case logLineMsg:
		text := strings.TrimSpace(string(msg))
		if text == "" {
			return m, nil
		}
		m.logBuffer = append(m.logBuffer, text)
		if len(m.logBuffer) > maxLogs*3 {
			m.logBuffer = m.logBuffer[1:]
		}
		return m, nil

	case startReqMsg:
		m.activeReqs[msg.id] = &ReqState{
			ID:        msg.id,
			Model:     "连接中...",
			State:     "🔗 连接中",
			Color:     "\033[90m",
			StartTime: time.Now(),
			Streaming: msg.streaming,
		}
		return m, nil

	case finishReqMsg:
		// 不立即删除：token 用量与最终成败只在收尾时才可知，保留一段时间才看得见。
		if req, ok := m.activeReqs[msg.id]; ok {
			req.Finished = true
			req.Success = msg.success
			req.FinishAt = time.Now()
			if msg.success {
				req.State = "✅ 完成"
				req.Color = "\033[32m"
			} else {
				req.State = "❌ 失败"
				req.Color = "\033[31m"
			}
		}
		return m, nil

	case updateReqModelMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.Model = stripANSI(msg.model)
		}
		return m, nil

	case updateReqTraceMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.RawModel = stripANSI(msg.rawModel)
			req.ResponseMode = stripANSI(msg.responseMode)
			req.AntiTruncate = msg.antiTruncate
		}
		return m, nil

	case updateReqStateMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.State = stripANSI(msg.state)
			req.Color = msg.color
			if msg.detail != "" {
				req.Detail = stripANSI(msg.detail)
			}
		}
		return m, nil

	case updateReqWinnerMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.WinnerNode = msg.nodeName
		}
		return m, nil

	case updateReqUsageMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.PromptTok = msg.prompt
			req.CandTok = msg.cand
			req.TotalTok = msg.total
		}
		return m, nil

	case updateReqTTFBMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.UpstreamTTFBMs = msg.upstreamMs
		}
		return m, nil

	case updateReqTruncationMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.Truncation = stripANSI(msg.label)
		}
		return m, nil

	case updateReqStreamMsg:
		if req, ok := m.activeReqs[msg.id]; ok {
			req.Streaming = msg.streaming
		}
		return m, nil

	case poolStatsMsg:
		// 预取池快照由后台采样器推送，绝不在事件循环内读配置（见 startPoolStatsSampler）。
		m.poolStats = msg.stats
		m.poolEnabled = msg.ok
		return m, nil

	case tea.KeyPressMsg:
		switch {
		case msg.Mod == tea.ModCtrl && msg.Code == 'c':
			return m, tea.Quit
		case msg.Code == tea.KeyUp:
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
		case msg.Code == tea.KeyDown:
			m.scrollOffset++
		case msg.Code == tea.KeyPgUp:
			m.scrollOffset -= m.height / 2
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
		case msg.Code == tea.KeyPgDown:
			m.scrollOffset += m.height / 2
		case msg.Code == tea.KeyHome:
			m.scrollOffset = 0
		case msg.Code == tea.KeyEnd:
			m.scrollOffset = 1 << 30
		}
		return m, nil

	case setAppInfoMsg:
		m.appVersion = msg.version
		m.buildInfo = msg.buildInfo
		m.platformInfo = msg.platformInfo
		return m, nil

	case setAdminPasswordMsg:
		m.initialAdminPassword = string(msg)
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}

	return m, nil
}

var (
	cyanStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	yellowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	grayStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	redStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func ansiToColor(code string) lipgloss.Color {
	switch code {
	case "\033[33m":
		return lipgloss.Color("3")
	case "\033[32m":
		return lipgloss.Color("2")
	case "\033[36m":
		return lipgloss.Color("6")
	case "\033[31m":
		return lipgloss.Color("1")
	case "\033[90m":
		return lipgloss.Color("8")
	default:
		return lipgloss.Color("7")
	}
}

func replaceFirstBorder(s string, repl rune) string {
	rs := []rune(s)
	for i, r := range rs {
		if r == '│' {
			rs[i] = repl
			return string(rs)
		}
	}
	return s
}

func replaceLastBorder(s string, repl rune) string {
	rs := []rune(s)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == '│' {
			rs[i] = repl
			return string(rs)
		}
	}
	return s
}

func (m TuiModel) buildContent(bw int) string {
	biw := bw - 4
	if biw < minTermWidth-4 {
		biw = minTermWidth - 4
	}

	var sb strings.Builder

	bottomBorder := func() string {
		return "╰" + dashBar(bw-2) + "╯"
	}

	// ── Banner ──
	{
		prefix := "╭── 📢 Vertex AI Proxy "
		pw := stringWidth(prefix)
		d := bw - pw - 1
		if d < 0 {
			d = 0
		}
		sb.WriteString(yellowStyle.Render(prefix + dashBar(d) + "╮"))
		sb.WriteByte('\n')

		line1 := fmt.Sprintf("Version: %s | %s", m.appVersion, m.platformInfo)
		sb.WriteString(yellowStyle.Render("│"))
		sb.WriteByte(' ')
		sb.WriteString(padOrTrunc(line1, biw))
		sb.WriteByte(' ')
		sb.WriteString(yellowStyle.Render("│"))
		sb.WriteByte('\n')
		sb.WriteString(yellowStyle.Render("│"))
		sb.WriteByte(' ')
		sb.WriteString(padOrTrunc(m.buildInfo, biw))
		sb.WriteByte(' ')
		sb.WriteString(yellowStyle.Render("│"))
		sb.WriteByte('\n')
		if m.initialAdminPassword != "" {
			pwLine := fmt.Sprintf("🔑 初始管理员密码: %s (登录后可在面板修改)", m.initialAdminPassword)
			sb.WriteString(yellowStyle.Render("│"))
			sb.WriteByte(' ')
			sb.WriteString(cyanStyle.Render(padOrTrunc(pwLine, biw)))
			sb.WriteByte(' ')
			sb.WriteString(yellowStyle.Render("│"))
			sb.WriteByte('\n')
		}

		warn := "⚠️  本软件完全免费！付费即被骗，请退款。"
		sb.WriteString(yellowStyle.Render("│"))
		sb.WriteByte(' ')
		sb.WriteString(redStyle.Render(padOrTrunc(warn, biw)))
		sb.WriteByte(' ')
		sb.WriteString(yellowStyle.Render("│"))
		sb.WriteByte('\n')
		sb.WriteString(yellowStyle.Render(bottomBorder()))
		sb.WriteByte('\n')
	}

	// ── 最近系统日志 ──
	{
		prefix := "╭── 📝 最近系统日志 "
		pw := stringWidth(prefix)
		d := bw - pw - 1
		if d < 0 {
			d = 0
		}
		sb.WriteString(cyanStyle.Render(prefix + dashBar(d) + "╮"))
		sb.WriteByte('\n')

		var visualLines []string
		for i := len(m.logBuffer) - 1; i >= 0 && len(visualLines) < maxLogs*5; i-- {
			wrapped := wordWrap(m.logBuffer[i], biw)
			visualLines = append(wrapped, visualLines...)
		}
		if len(visualLines) > maxLogs {
			visualLines = visualLines[len(visualLines)-maxLogs:]
		}
		for i := 0; i < maxLogs; i++ {
			if i < len(visualLines) {
				sb.WriteString(cyanStyle.Render("│"))
				sb.WriteByte(' ')
				sb.WriteString(visualLines[i])
				sb.WriteByte(' ')
				sb.WriteString(cyanStyle.Render("│"))
				sb.WriteByte('\n')
			} else {
				sb.WriteString(cyanStyle.Render("│"))
				sb.WriteByte(' ')
				sb.WriteString(strings.Repeat(" ", biw))
				sb.WriteByte(' ')
				sb.WriteString(cyanStyle.Render("│"))
				sb.WriteByte('\n')
			}
		}
		sb.WriteString(cyanStyle.Render(bottomBorder()))
		sb.WriteByte('\n')
	}

	// ── 请求追踪器 ──
	if len(m.activeReqs) > 0 {
		reqs := make([]*ReqState, 0, len(m.activeReqs))
		for _, r := range m.activeReqs {
			reqs = append(reqs, r)
		}
		// 未完成在前、已完成在后；组内按开始时间升序。
		sort.Slice(reqs, func(i, j int) bool {
			if reqs[i].Finished != reqs[j].Finished {
				return !reqs[i].Finished
			}
			return reqs[i].StartTime.Before(reqs[j].StartTime)
		})

		prefix := "╭── 🚀 请求追踪器 "
		pw := stringWidth(prefix)
		d := bw - pw - 1
		if d < 0 {
			d = 0
		}
		sb.WriteString(cyanStyle.Render(prefix + dashBar(d) + "╮"))
		sb.WriteByte('\n')

		// 8 列：9 个竖线 + 16 个间隔空格 = 25 列固定开销。
		const separatorOverhead = 25
		totalColsWidth := bw - separatorOverhead

		// Time 列改为"首字/总耗时"，需要容纳两个耗时文本；但窄终端下它会挤掉
		// State（"✅ 完成"需 7 列宽），故按终端宽度自适应：窄屏用紧凑档。
		idW := 8
		timeW := 15
		if bw < 120 {
			timeW = 11
		}
		streamW := 10
		antiW := 6
		tokW := 13
		remaining := totalColsWidth - idW - timeW - streamW - antiW - tokW
		if remaining < 20 {
			remaining = 20
		}
		modelW := remaining * 30 / 100
		if modelW < 8 {
			modelW = 8
		}
		stateW := remaining * 28 / 100
		if stateW < 6 {
			stateW = 6
		}
		detailW := remaining - modelW - stateW
		if detailW < 6 {
			detailW = 6
		}

		// 表头：必须用与数据行一致的"显示宽度"补齐（padOrTrunc 基于 uniseg 显示宽度）。
		// 不能用 fmt 的 %-*s——它按 rune 数补空格，而 CJK 表头（如"请求回应"4 rune /
		// 8 列宽）会被少补一半，导致表头与数据行错位。
		sb.WriteString(fmt.Sprintf("%s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s\n",
			cyanStyle.Render("│"), padOrTrunc("ID", idW), cyanStyle.Render("│"),
			padOrTrunc("Model", modelW), cyanStyle.Render("│"),
			padOrTrunc("State", stateW), cyanStyle.Render("│"),
			padOrTrunc("请求回应", streamW), cyanStyle.Render("│"),
			padOrTrunc("抗截断", antiW), cyanStyle.Render("│"),
			padOrTrunc("Tokens≈", tokW), cyanStyle.Render("│"),
			padOrTrunc(timeHeader(timeW), timeW), cyanStyle.Render("│"),
			padOrTrunc("Details", detailW), cyanStyle.Render("│")))

		// 分隔行：9 个十字/边角 + 8 段 dash = 17 段。
		sep := fmt.Sprintf("%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s\n",
			cyanStyle.Render("├"), dashBar(idW+2), cyanStyle.Render("┼"),
			dashBar(modelW+2), cyanStyle.Render("┼"),
			dashBar(stateW+2), cyanStyle.Render("┼"),
			dashBar(streamW+2), cyanStyle.Render("┼"),
			dashBar(antiW+2), cyanStyle.Render("┼"),
			dashBar(tokW+2), cyanStyle.Render("┼"),
			dashBar(timeW+2), cyanStyle.Render("┼"),
			dashBar(detailW+2), cyanStyle.Render("┤"))
		sb.WriteString(sep)

		for _, r := range reqs {
			end := time.Now()
			if r.Finished {
				end = r.FinishAt
			}
			elapsed := end.Sub(r.StartTime).Seconds()
			idVal := r.ID
			if len(idVal) > idW-2 {
				idVal = idVal[:idW-2]
			}

			detailStr := r.Detail
			if r.WinnerNode != "" {
				detailStr = "🏆 " + r.WinnerNode
				if r.Detail != "" {
					detailStr += " | " + r.Detail
				}
			}

			idCol := padOrTrunc(idVal, idW-2)
			modelCol := padOrTrunc(modelCell(r), modelW)
			stateCol := padOrTrunc(stateCell(r, stateW), stateW)
			streamCol := padOrTrunc(requestResponseCell(r), streamW)
			antiCol := padOrTrunc(antiTruncateCell(r), antiW)
			tokCol := padOrTrunc(tokensText(r), tokW)
			// Time 列：首字/总耗时并列。首字优先取上游首字节（恒有意义，
			// 上游侧恒为流式），缺失时回退客户端首字；未采到则显示 "-"。
			timeCol := timeText(r, elapsed)
			timeCol = padOrTrunc(timeCol, timeW)
			detailCol := padOrTrunc(detailStr, detailW)

			spinnerCh := string(spinnerFrames[m.spinnerIdx])
			if r.Finished {
				spinnerCh = " "
			}
			stateRendered := lipgloss.NewStyle().Foreground(ansiToColor(r.Color)).Render(stateCol)

			// 数据行：9 个竖线 + 9 个内容段（含 spinner）= 18 段。
			sb.WriteString(fmt.Sprintf("%s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s\n",
				cyanStyle.Render("│"),
				cyanStyle.Render(spinnerCh), idCol, cyanStyle.Render("│"),
				modelCol, cyanStyle.Render("│"),
				stateRendered, cyanStyle.Render("│"),
				streamCol, cyanStyle.Render("│"),
				antiCol, cyanStyle.Render("│"),
				tokCol, cyanStyle.Render("│"),
				timeCol, cyanStyle.Render("│"),
				grayStyle.Render(detailCol), cyanStyle.Render("│")))
		}
		sb.WriteString(cyanStyle.Render(bottomBorder()))
		sb.WriteByte('\n')
	}

	// ── RT 补货池（开启预取后显示；与是否有在途请求无关）──
	if m.poolEnabled && m.poolStats.PrefetchEnabled {
		prefix := "╭── ♻️  RT 补货池 "
		pw := stringWidth(prefix)
		d := bw - pw - 1
		if d < 0 {
			d = 0
		}
		sb.WriteString(grayStyle.Render(prefix + dashBar(d) + "╮"))
		sb.WriteByte('\n')
		// 自停标记紧跟"池内"：窄终端下 padOrTrunc 会截尾，暂停状态必须留在可见区，
		// 否则"池内恒 0"看不出是主动暂停还是抓取故障。
		paused := ""
		if m.poolStats.Paused {
			paused = fmt.Sprintf(" | ⏸ 已暂停补货(第 %d 次)", m.poolStats.Pauses)
		}
		line := fmt.Sprintf("池内 %d/%d%s | 命中 %d | 未命中 %d | 过期 %d | 失败 %d",
			m.poolStats.Size, m.poolStats.Depth, paused, m.poolStats.Hits,
			m.poolStats.Misses, m.poolStats.Expired, m.poolStats.Failures)
		sb.WriteString(grayStyle.Render("│"))
		sb.WriteByte(' ')
		sb.WriteString(padOrTrunc(line, biw))
		sb.WriteByte(' ')
		sb.WriteString(grayStyle.Render("│"))
		sb.WriteByte('\n')
		sb.WriteString(grayStyle.Render(bottomBorder()))
		sb.WriteByte('\n')
	}

	// ── RT Token 实时记录（请求路径 + 后台预取的抓取事件；预取关闭时同样有效）──
	if m.poolEnabled && len(m.poolStats.Recent) > 0 {
		// rows 只是防御上限（防止装配侧给来一大串把面板撑爆）；实际展示条数由装配侧
		// 的 tuiTokenEventRows 决定，改那一个常量即可。
		const rows = 20
		prefix := "╭── ♻️  RT Token 实时记录 "
		pw := stringWidth(prefix)
		d := bw - pw - 1
		if d < 0 {
			d = 0
		}
		sb.WriteString(grayStyle.Render(prefix + dashBar(d) + "╮"))
		sb.WriteByte('\n')
		for i, ev := range m.poolStats.Recent {
			if i >= rows {
				break
			}
			sb.WriteString(grayStyle.Render("│"))
			sb.WriteByte(' ')
			sb.WriteString(padOrTrunc(tokenEventLine(ev), biw))
			sb.WriteByte(' ')
			sb.WriteString(grayStyle.Render("│"))
			sb.WriteByte('\n')
		}
		sb.WriteString(grayStyle.Render(bottomBorder()))
		sb.WriteByte('\n')
	}

	return sb.String()
}

// rtRankLabel 渲染 rT 降权层：0 健康 / 1 对 rT 差或慢 / 2 rT 冷却中（与管理页口径一致）。
// 该层只影响 rT 抓取选点：降权节点只在好节点不足时才被用到。
func rtRankLabel(rank int) string {
	switch rank {
	case 1:
		return "档1差"
	case 2:
		return "档2冷"
	default:
		return "档0健"
	}
}

// tokenEventLine 渲染一条 rT 抓取记录：时间 来源 耗时 节点 结果。
// 各列先按固定显示宽度补齐再整行截断，保证窄终端下列仍能对齐。
// 耗时口径与管理页一致：>=1s 用秒，否则毫秒（预算是 15s，秒级很常见）。
func tokenEventLine(ev TokenEvent) string {
	lat := fmt.Sprintf("%dms", ev.LatencyMs)
	if ev.LatencyMs >= 1000 {
		lat = fmt.Sprintf("%.2fs", float64(ev.LatencyMs)/1000)
	}
	result := "✓"
	switch {
	case ev.Canceled:
		// 被取消不是失败（竞速其它候选胜出 / 请求中断），中性呈现。
		result = "⊘ 已取消"
	case !ev.OK:
		result = "✗"
		if ev.Err != "" {
			result += " " + ev.Err
		}
	}
	return fmt.Sprintf("%s %s %s %s %s %s",
		padOrTrunc(ev.At.Format("15:04:05"), 8),
		padOrTrunc(ev.Source, 10),
		padOrTrunc(lat, 8),
		padOrTrunc(ev.Node, 14),
		padOrTrunc(rtRankLabel(ev.RTRank), 6),
		result)
}

// pruneFinished 清理超期或超量的已完成行；未完成行永不清理。
func (m TuiModel) pruneFinished() {
	now := time.Now()
	var done []*ReqState
	for id, r := range m.activeReqs {
		if !r.Finished {
			continue
		}
		if now.Sub(r.FinishAt) > finishedRetention {
			delete(m.activeReqs, id)
			continue
		}
		done = append(done, r)
	}
	if len(done) <= maxFinishedRows {
		return
	}
	sort.Slice(done, func(i, j int) bool { return done[i].FinishAt.Before(done[j].FinishAt) })
	for _, r := range done[:len(done)-maxFinishedRows] {
		delete(m.activeReqs, r.ID)
	}
}

// modelCell 渲染模型列：请求模型（含变体前缀）与实际发往上游的模型不同时才并列，
// 相同则只显示一个，避免窄终端被无意义重复占满。
//
// 连接中（RawModel 尚未补齐）时 RawModel 为空串，此时按"相同"处理只显示实际模型。
func modelCell(r *ReqState) string {
	if r.RawModel == "" || r.RawModel == r.Model {
		return r.Model
	}
	return r.RawModel + "→" + r.Model
}

// requestResponseCell 渲染"请求回应"列：左侧是客户端请求的形态，右侧是实际接收形态。
//
// 两侧恒为"请求→回应"的并列形式：请求侧是 流式/非流式、回应侧是 增量/聚合/单次，
// 属两套词汇，不能做相等比较（早期实现用相等判断是否降级，条件恒不成立、降级永远显示不出来）。
// 降级与否由人从"流式→聚合"这样的组合直接读出。回应形态尚未采集到时只显示请求侧。
func requestResponseCell(r *ReqState) string {
	req := "非流式"
	if r.Streaming {
		req = "流式"
	}
	if r.ResponseMode == "" {
		return req
	}
	return req + "→" + r.ResponseMode
}

// stateCell 渲染 State 列。上游主动异常终止时（撞输出上限 / 工具调用畸形）
// 在收尾状态后追加标注，例如 "✅ 完成(输出上限截断)"——成败判定不变，
// 只是把"这次虽然 HTTP 成功、但内容被上游切断了"这一事实显式化。
//
// 自适应列宽：State 列宽度随终端与其余列挤压（80 列下仅 6~14 宽），
// 完整文案放不下时退化为紧凑告警标记 "⚠"，保证"有截断"这件事在任何宽度下都可见
// （完整原因在历史记录页与 http_log 中可查）。
func stateCell(r *ReqState, width int) string {
	if r == nil {
		return ""
	}
	if r.Truncation == "" {
		return r.State
	}
	full := r.State + "(" + r.Truncation + ")"
	if stringWidth(full) <= width {
		return full
	}
	return r.State + "⚠"
}

// antiTruncateCell 渲染"抗截断"列：是否走 StreamChatCollect 竞速链路。
// 未采集到时为 "-"（与"确认未启用"区分开）。
func antiTruncateCell(r *ReqState) string {
	if r.AntiTruncate {
		return "抗截断"
	}
	return "-"
}

// timeHeader 按 Time 列可用宽度选表头：窄列放不下完整"首字/总耗时"时用短表头。
func timeHeader(colW int) string {
	if colW >= 11 {
		return "首字/总耗时"
	}
	return "耗时"
}

// timeText 渲染 Time 列的"首字/总耗时"。
// 首字取上游首字节耗时；未采到显示 "-"（与"耗时确实为 0"区分开）。
func timeText(r *ReqState, elapsed float64) string {
	total := fmt.Sprintf("%.1fs", elapsed)
	if r.UpstreamTTFBMs <= 0 {
		return "-/" + total
	}
	return fmt.Sprintf("%.1fs/%s", float64(r.UpstreamTTFBMs)/1000, total)
}

// tokensText 返回 token 列内容（输入/输出）。
//
// 进行中且尚无用量时显示占位符：流式的 usageMetadata 只在终帧出现。
// 已结束仍是全 0 则显示 0/0 —— 明确区分"上游没回报用量"与"还不知道"。
func tokensText(r *ReqState) string {
	if !r.Finished && r.PromptTok == 0 && r.CandTok == 0 && r.TotalTok == 0 {
		return "—"
	}
	return humanTok(r.PromptTok) + "/" + humanTok(r.CandTok)
}

// humanTok 把 token 数格式化为紧凑文本（1.2k / 1.2M）。
func humanTok(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
}

func (m TuiModel) View() tea.View {
	bw := m.width
	if bw < minTermWidth {
		bw = minTermWidth
	}

	content := m.buildContent(bw)

	if m.height <= 0 {
		v := tea.NewView(content)
		v.AltScreen = true
		return v
	}

	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	if totalLines <= m.height {
		v := tea.NewView(content)
		v.AltScreen = true
		return v
	}

	maxStart := totalLines - m.height
	if m.scrollOffset > maxStart {
		m.scrollOffset = maxStart
	}
	start := m.scrollOffset

	visible := make([]string, m.height)
	copy(visible, lines[start:start+m.height])

	if start > 0 && len(visible[0]) > 0 {
		visible[0] = replaceFirstBorder(visible[0], '↑')
	}

	if start+m.height < totalLines {
		lastIdx := m.height - 1
		visible[lastIdx] = replaceLastBorder(visible[lastIdx], '↓')
	}

	// 当最后一行是底部 border 时嵌入位置文本
	if start+m.height >= totalLines {
		lastVis := visible[m.height-1]
		plain := stripANSI(lastVis)
		if strings.HasPrefix(plain, "╰") && strings.HasSuffix(plain, "╯") {
			pos := fmt.Sprintf(" %d/%d ", start+m.height, totalLines)
			innerRunes := []rune(plain)
			innerWidth := len(innerRunes) - 2
			posW := len([]rune(pos))
			if posW < innerWidth && posW > 0 {
				padL := (innerWidth - posW) / 2
				padR := innerWidth - posW - padL
				newInner := strings.Repeat("─", padL) + pos + strings.Repeat("─", padR)
				visible[m.height-1] = reconstructStyled(lastVis, "╰"+newInner+"╯")
			}
		}
	}

	v := tea.NewView(strings.Join(visible, "\n"))
	v.AltScreen = true
	return v
}

func reconstructStyled(original, plainText string) string {
	rs := []rune(original)
	var prefix, suffix string
	for i, r := range rs {
		if r == '\x1b' {
			j := i + 1
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			if j < len(rs) {
				prefix = string(rs[:j+1])
				break
			}
		}
	}
	// Plain text starts after ANSI prefix
	startPlain := len([]rune(prefix))
	// After plain text, there might be more ANSI
	for i := startPlain; i < len(rs); i++ {
		if rs[i] == '\x1b' {
			suffix = string(rs[i:])
			break
		}
	}
	return prefix + plainText + suffix
}
