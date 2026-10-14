package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestTracker_TableHeaderAlignsWithRows 回归测试：表头必须与数据行按同一"显示宽度"补齐。
// 历史上表头用 fmt 的 %-*s（按 rune 数补空格），CJK 表头（"请求回应"4 rune / 8 列宽）
// 会被少补一半，导致表头与数据行错位。
func TestTracker_TableHeaderAlignsWithRows(t *testing.T) {
	m := TuiModel{width: 160, height: 40, activeReqs: make(map[string]*ReqState)}
	model, _ := m.Update(startReqMsg{id: "r1", streaming: true})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqUsageMsg{id: "r1", prompt: 10, cand: 20, total: 30})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqTTFBMsg{id: "r1", upstreamMs: 1200})
	m = model.(TuiModel)

	content := stripANSI(m.buildContent(160))
	var header, sep, row string
	for _, ln := range strings.Split(content, "\n") {
		switch {
		case strings.Contains(ln, "请求回应") && strings.Contains(ln, "Model"):
			header = ln
		case strings.Contains(ln, "┼"):
			sep = ln
		case strings.Contains(ln, "r1"):
			row = ln
		}
	}
	if header == "" || sep == "" || row == "" {
		t.Fatalf("未找到表头/分隔行/数据行:\n%s", content)
	}
	// 三者显示宽度必须一致，且竖线列位置逐字符对齐。
	hw, sw, rw := stringWidth(header), stringWidth(sep), stringWidth(row)
	if hw != sw || hw != rw {
		t.Errorf("表头/分隔行/数据行宽度不一致: header=%d sep=%d row=%d\n%s\n%s\n%s",
			hw, sw, rw, header, sep, row)
	}
	// 逐列比较竖线位置（按显示列号，而非 rune 下标——CJK 会令两者不一致）。
	// 表头与分隔行的竖线列号必须完全一致，错位会在此暴露。
	if diff := barColumns(header); !reflect.DeepEqual(diff, barColumns(sep)) {
		t.Errorf("表头/分隔行竖线列号不一致:\nheader=%v\nsep   =%v\n%s\n%s",
			barColumns(header), barColumns(sep), header, sep)
	}
	// Time 列应呈现"首字/总耗时"
	if !strings.Contains(row, "1.2s/") {
		t.Errorf("Time 列应为 首字/总耗时 形式, 实际行:\n%s", row)
	}
}

// barColumns 返回一行中所有竖线/十字所在的显示列号（按 uniseg 宽度累加）。
func barColumns(line string) []int {
	var cols []int
	w := 0
	for _, ch := range line {
		if ch == '│' || ch == '┼' || ch == '├' || ch == '┤' {
			cols = append(cols, w)
		}
		w += stringWidth(string(ch))
	}
	return cols
}

// TestTimeText 覆盖 timeText 的取值与未采到时的占位。
func TestTimeText(t *testing.T) {
	cases := []struct {
		name string
		r    ReqState
		want string
	}{
		{"有上游首字", ReqState{UpstreamTTFBMs: 1200}, "1.2s/3.0s"},
		{"未采到首字", ReqState{}, "-/3.0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timeText(&tc.r, 3.0); got != tc.want {
				t.Errorf("timeText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStringWidth(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int
	}{
		{"ASCII empty", "", 0},
		{"ASCII word", "hello", 5},
		{"Chinese standard", "你好", 4},
		{"Chinese mixed", "hello你好", 9},
		{"Emoji rocket", "🚀", 2},
		{"Emoji trophy", "🏆", 2},
		{"Emoji memo", "📝", 2},
		{"Emoji flag HK", "🇭🇰", 2}, // let's see what uniseg yields
		{"Emoji flag CN", "🇨🇳", 2}, // let's see what uniseg yields
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stringWidth(tt.s)
			if got != tt.want {
				t.Errorf("stringWidth(%q) = %d; want %d", tt.s, got, tt.want)
			}
		})
	}
}

func TestPadOrTrunc(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxCol int
		want   string
	}{
		{"Empty string pad 5", "", 5, "     "},
		{"ASCII pad", "abc", 5, "abc  "},
		{"ASCII exact", "abcde", 5, "abcde"},
		{"ASCII trunc to 5", "abcdefg", 5, "abc.."},
		{"Chinese pad", "你好", 6, "你好  "},
		{"Chinese exact", "你好", 4, "你好"},
		{"Chinese trunc to 5", "你好啊", 5, "你.. "}, // "你好啊" is width 6. "你" is width 2. "你" + ".." is width 4 <= 5-2? Yes, 4 > 3, breaks! Returns "你" + ".." + pad = "你.. " (width 5).
		{"Chinese trunc to 4", "你好啊", 4, "你.."},  // cur=2. Next "好" (2). cur+gw = 4 > maxCol-2 (2)? Yes, 4 > 2, breaks. Returns "你" + ".." (width 4).
		{"Chinese trunc to 3", "你好啊", 3, ".. "},  // cur=0. Next "你" (2). cur+gw = 2 > maxCol-2 (1)? Yes, 2 > 1, breaks. Returns ".." + " " (width 3).
		{"Emoji pad", "🚀", 4, "🚀  "},
		{"Emoji trunc to 3", "🚀🏆", 3, ".. "}, // cur=0. Next "🚀" (2). cur+gw = 2 > maxCol-2 (1)? Yes, breaks. Returns ".." + " " (width 3).
		{"Emoji flag HK pad", "🇭🇰", 3, "🇭🇰 "},
		{"Emoji flag HK trunc to 3", "🇭🇰🏆", 3, ".. "},
		{"maxCol <= 2 (2)", "hello", 2, ".."},
		{"maxCol <= 2 (1)", "hello", 1, ".."},
		{"maxCol <= 2 (0)", "hello", 0, ".."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := padOrTrunc(tt.s, tt.maxCol)
			if got != tt.want {
				t.Errorf("padOrTrunc(%q, %d) = %q (width %d); want %q (width %d)",
					tt.s, tt.maxCol, got, stringWidth(got), tt.want, stringWidth(tt.want))
			}
		})
	}
}

func TestSetInitialAdminPassword(t *testing.T) {
	m := TuiModel{
		width:      80,
		height:     24,
		activeReqs: make(map[string]*ReqState),
	}
	model, _ := m.Update(setAdminPasswordMsg("testPw123"))
	updated, ok := model.(TuiModel)
	if !ok {
		t.Fatalf("Update 应返回 TuiModel")
	}
	if updated.initialAdminPassword != "testPw123" {
		t.Fatalf("initialAdminPassword 应为 %q，实际 %q", "testPw123", updated.initialAdminPassword)
	}

	content := updated.buildContent(80)
	if !strings.Contains(content, "testPw123") {
		t.Fatalf("Banner 应包含初始管理员密码")
	}
	if !strings.Contains(content, "初始管理员密码") {
		t.Fatalf("Banner 应包含密码提示文案")
	}

	plain := updated.buildContent(80)
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "testPw123") {
			strip := stripANSI(line)
			if !strings.HasPrefix(strip, "│ ") || !strings.HasSuffix(strip, " │") {
				t.Fatalf("密码行应保持 Banner 边框对齐: %q", strip)
			}
		}
	}
}

func TestNoAdminPasswordLineByDefault(t *testing.T) {
	m := TuiModel{
		width:      80,
		height:     24,
		activeReqs: make(map[string]*ReqState),
	}
	content := m.buildContent(80)
	if strings.Contains(content, "初始管理员密码") {
		t.Fatalf("未设置初始密码时 Banner 不应包含密码提示")
	}
}

func TestIsTUIEnabledDefaultsFalse(t *testing.T) {
	if IsTUIEnabled() {
		t.Fatalf("非 TTY 测试环境下 TUI 应处于禁用状态")
	}
}

func TestWordWrap(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxCol int
		want   []string
	}{
		{
			name:   "ASCII simple wrapping",
			text:   "hello world",
			maxCol: 5,
			want:   []string{"hello", "world"}, // Wait! Let's trace how the code handles "hello world" with maxCol=5.
			// Text: "hello world" (width 11).
			// i=0: "h" -> cur="h", curW=1
			// i=1: "e" -> cur="he", curW=2
			// i=2: "l" -> cur="hel", curW=3
			// i=3: "l" -> cur="hell", curW=4
			// i=4: "o" -> cur="hello", curW=5
			// i=5: " " (space). curW > 0.
			//   nextW = width of "world" = 5.
			//   curW+1+nextW = 5+1+5 = 11 > maxCol (5).
			//   Appends cur.String() + padding = "hello" + "" -> "hello"
			//   cur.Reset(), curW=0. i++ (skips space). continue.
			// i=6: "w" -> cur="w", curW=1
			// ... "world" -> cur="world", curW=5.
			// End of loop. cur.Len() > 0 -> appends "world" + "".
			// Result: []string{"hello", "world"}. Perfectly matches!
		},
		{
			name:   "ASCII with multiple spaces result",
			text:   "hello   world",
			maxCol: 5,
			want:   []string{"hello", "     ", "world"},
		},
		{
			name:   "Chinese wrap",
			text:   "你好世界",
			maxCol: 4,
			want:   []string{"你好", "世界"}, // "你好" width is 4, "世界" width is 4. Perfect fit.
		},
		{
			name:   "Chinese wrap odd width",
			text:   "你好世界",
			maxCol: 5,
			want:   []string{"你好 ", "世界 "}, // cur="你好" (width 4). Next "世" (width 2). 4+2=6 > 5 -> wraps to "你好 " (width 5). Then cur="世界" (width 4), wraps to "世界 " (width 5).
		},
		{
			name:   "Emoji wrap",
			text:   "🚀🏆📝",
			maxCol: 4,
			want:   []string{"🚀🏆", "📝  "}, // "🚀🏆" is width 4. "📝" is width 2, gets padded to 4.
		},
		{
			name:   "Mixed text wrap",
			text:   "Go🚀语言", // "Go" (2), "🚀" (2), "语言" (4). Total width 8.
			maxCol: 4,
			want:   []string{"Go🚀", "语言"}, // "Go🚀" width is 4. "语言" width is 4.
		},
		{
			name:   "Zero or negative maxCol",
			text:   "hello",
			maxCol: 0,
			want:   []string{"hello"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wordWrap(tt.text, tt.maxCol)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("wordWrap(%q, %d) = %q; want %q", tt.text, tt.maxCol, got, tt.want)
			}
			// Verify that every line in the output (except if maxCol <= 0) has exactly width maxCol
			if tt.maxCol > 0 {
				for i, line := range got {
					w := stringWidth(line)
					if w != tt.maxCol {
						t.Errorf("line %d %q has width %d; want %d", i, line, w, tt.maxCol)
					}
				}
			}
		})
	}
}

// TestTracker_ShowsStreamingAndTokens 锁定追踪器的流式列与输入/输出 token 列。
func TestTracker_ShowsStreamingAndTokens(t *testing.T) {
	m := TuiModel{
		width:      100,
		height:     40,
		activeReqs: make(map[string]*ReqState),
	}
	m.activeReqs["req1"] = &ReqState{
		ID:           "req1",
		Model:        "gemini-3-flash",
		State:        "🟢 数据传输",
		Color:        "\033[32m",
		StartTime:    time.Now(),
		Streaming:    true,
		ResponseMode: "增量",
		PromptTok:    12300,
		CandTok:      340,
	}
	plain := stripANSI(m.buildContent(100))
	if !strings.Contains(plain, "请求回应") {
		t.Errorf("追踪器应包含请求回应列表头, 实际:\n%s", plain)
	}
	if !strings.Contains(plain, "抗截断") {
		t.Errorf("追踪器应包含抗截断列表头, 实际:\n%s", plain)
	}
	if !strings.Contains(plain, "Tokens") {
		t.Error("追踪器应包含 Tokens 列表头")
	}
	if !strings.Contains(plain, "12.3k/340") {
		t.Errorf("应显示输入/输出 token, 实际内容:\n%s", plain)
	}
	if !strings.Contains(plain, "流式") {
		t.Errorf("流式请求应显示流式标记, 实际:\n%s", plain)
	}
}

// TestTracker_RequestResponseCells 锁定模型列与请求回应列的"不同时才并列"渲染口径。
func TestTracker_RequestResponseCells(t *testing.T) {
	cases := []struct {
		name           string
		req            *ReqState
		wantModelCell  string
		wantStreamCell string
		wantAntiCell   string
	}{
		{
			name:           "模型一致则单显",
			req:            &ReqState{Model: "gemini-3-flash", RawModel: "gemini-3-flash", ResponseMode: "单次"},
			wantModelCell:  "gemini-3-flash",
			wantStreamCell: "非流式→单次",
			wantAntiCell:   "-",
		},
		{
			name:           "抗截断变体拼接",
			req:            &ReqState{Model: "gemini-3-flash", RawModel: "抗截断-gemini-3-flash", ResponseMode: "单次", AntiTruncate: true},
			wantModelCell:  "抗截断-gemini-3-flash→gemini-3-flash",
			wantStreamCell: "非流式→单次",
			wantAntiCell:   "抗截断",
		},
		{
			name:           "流式请求增量交付",
			req:            &ReqState{Model: "gemini-3-flash", Streaming: true, ResponseMode: "增量"},
			wantModelCell:  "gemini-3-flash",
			wantStreamCell: "流式→增量",
			wantAntiCell:   "-",
		},
		{
			name:           "流式端点被聚合降级",
			req:            &ReqState{Model: "gemini-3-flash", Streaming: true, ResponseMode: "聚合"},
			wantModelCell:  "gemini-3-flash",
			wantStreamCell: "流式→聚合",
			wantAntiCell:   "-",
		},
		{
			name:           "回应形态未采集时只显请求侧",
			req:            &ReqState{Model: "gemini-3-flash", Streaming: true},
			wantModelCell:  "gemini-3-flash",
			wantStreamCell: "流式",
			wantAntiCell:   "-",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelCell(c.req); got != c.wantModelCell {
				t.Errorf("modelCell = %q, want %q", got, c.wantModelCell)
			}
			if got := requestResponseCell(c.req); got != c.wantStreamCell {
				t.Errorf("requestResponseCell = %q, want %q", got, c.wantStreamCell)
			}
			if got := antiTruncateCell(c.req); got != c.wantAntiCell {
				t.Errorf("antiTruncateCell = %q, want %q", got, c.wantAntiCell)
			}
		})
	}
}

// TestUpdateReqTrace 追踪三元组须经 TUI 消息落到行上，且模型名走 stripANSI 清洗。
func TestUpdateReqTrace(t *testing.T) {
	m := TuiModel{width: 100, height: 40, activeReqs: make(map[string]*ReqState)}
	model, _ := m.Update(startReqMsg{id: "r1", streaming: false})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqModelMsg{id: "r1", model: "gemini-3-flash"})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqTraceMsg{
		id: "r1", rawModel: "抗截断-gemini-3-flash", responseMode: "单次", antiTruncate: true,
	})
	m = model.(TuiModel)

	r := m.activeReqs["r1"]
	if r.RawModel != "抗截断-gemini-3-flash" {
		t.Errorf("RawModel = %q", r.RawModel)
	}
	if r.ResponseMode != "单次" {
		t.Errorf("ResponseMode = %q", r.ResponseMode)
	}
	if !r.AntiTruncate {
		t.Error("AntiTruncate 应为 true")
	}
	if r.Model != "gemini-3-flash" {
		t.Errorf("Model 不应被追踪消息覆盖, 实际 %q", r.Model)
	}
}

// TestTracker_PoolPanel 锁定 RT 预取池面板：池汇总按"预取总开关"展示，与是否有在途请求无关。
func TestTracker_PoolPanel(t *testing.T) {
	m := TuiModel{
		width:       100,
		height:      40,
		activeReqs:  make(map[string]*ReqState),
		poolEnabled: true,
		poolStats:   PoolStats{Size: 3, Depth: 5, Hits: 12, Misses: 4, Expired: 1, Failures: 2, PrefetchEnabled: true},
	}
	plain := stripANSI(m.buildContent(100))
	if !strings.Contains(plain, "RT 补货池") {
		t.Fatal("启用预取时应收录 RT 补货池面板")
	}
	if !strings.Contains(plain, "池内 3/5") {
		t.Errorf("面板应显示池内深度, 实际:\n%s", plain)
	}
	if !strings.Contains(plain, "命中 12") || !strings.Contains(plain, "失败 2") {
		t.Errorf("面板应显示命中/失败计数, 实际:\n%s", plain)
	}

	// 预取关闭时池恒为空，汇总无意义 → 不展示（记录流仍可展示）。
	m.poolStats.PrefetchEnabled = false
	if strings.Contains(stripANSI(m.buildContent(100)), "RT 补货池") {
		t.Error("未启用预取时不应出现补货池汇总面板")
	}

	// provider 整体不可用（未装配）时同样不展示。
	m.poolEnabled = false
	if strings.Contains(stripANSI(m.buildContent(100)), "RT 补货池") {
		t.Error("未装配 provider 时不应出现补货池面板")
	}
}

// TestTracker_RTTokenEventsPanel rT 抓取记录流面板：展示来源/节点/耗时/成败，
// 空记录时不渲染。该面板与预取开关无关——预取关闭时请求路径照样抓 token。
func TestTracker_RTTokenEventsPanel(t *testing.T) {
	now := time.Now()
	m := TuiModel{
		width:       120,
		height:      40,
		activeReqs:  make(map[string]*ReqState),
		poolEnabled: true,
		poolStats: PoolStats{
			PrefetchEnabled: false,
			Recent: []TokenEvent{
				{At: now, Source: "池命中", Node: "", LatencyMs: 0, OK: true},
				{At: now, Source: "实时抓取", Node: "hk-01", LatencyMs: 320, OK: true, RTRank: 0},
				{At: now, Source: "预取", Node: "sg-01", LatencyMs: 180, OK: false, Err: "dial timeout", RTRank: 2},
				{At: now, Source: "实时抓取", Node: "", LatencyMs: 1000, OK: false, Canceled: true},
			},
		},
	}
	plain := stripANSI(m.buildContent(120))
	if !strings.Contains(plain, "RT Token 实时记录") {
		t.Fatalf("应收录 rT 记录面板, 实际:\n%s", plain)
	}
	for _, want := range []string{"池命中", "实时抓取", "预取", "hk-01", "sg-01", "320ms", "✗", "⊘ 已取消", "档0健", "档2冷"} {
		if !strings.Contains(plain, want) {
			t.Errorf("记录面板应包含 %q, 实际:\n%s", want, plain)
		}
	}
	// 窄终端下整行被截断，绝不撑破面板宽度（失败原因可能很长）。
	for _, line := range strings.Split(stripANSI(m.buildContent(60)), "\n") {
		if w := stringWidth(line); w > 60 {
			t.Fatalf("窄终端下行宽 %d 超过面板宽度 60: %q", w, line)
		}
	}

	// 无记录时不渲染该面板。
	m.poolStats.Recent = nil
	if strings.Contains(stripANSI(m.buildContent(120)), "RT Token 实时记录") {
		t.Error("无记录时不应渲染 rT 记录面板")
	}
}

// TestTracker_PoolPanelPaused 自停状态必须可见：池内恒 0 时若没有暂停标记，
// 无法区分"无收益主动暂停补货"与"抓取一直失败"。
func TestTracker_PoolPanelPaused(t *testing.T) {
	m := TuiModel{
		width:       100,
		height:      40,
		activeReqs:  make(map[string]*ReqState),
		poolEnabled: true,
		poolStats:   PoolStats{Size: 0, Depth: 5, Misses: 30, Expired: 10, Paused: true, Pauses: 3, PrefetchEnabled: true},
	}
	plain := stripANSI(m.buildContent(100))
	if !strings.Contains(plain, "已暂停补货") {
		t.Errorf("自停时应显示暂停标记, 实际:\n%s", plain)
	}
	if !strings.Contains(plain, "第 3 次") {
		t.Errorf("应显示累计暂停次数, 实际:\n%s", plain)
	}

	// 未暂停时不得出现该标记（避免正常补货时误读）。
	m.poolStats.Paused = false
	if strings.Contains(stripANSI(m.buildContent(100)), "已暂停补货") {
		t.Error("未暂停时不应出现暂停标记")
	}
}

// TestTracker_KeepsFinishedRows 完成后短暂保留：token 用量与最终成败只在收尾时可知，
// 立即删除会让这些信息根本来不及看。
func TestTracker_KeepsFinishedRows(t *testing.T) {
	m := TuiModel{width: 100, height: 40, activeReqs: make(map[string]*ReqState)}
	model, _ := m.Update(startReqMsg{id: "r1", streaming: true})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqUsageMsg{id: "r1", prompt: 10, cand: 20, total: 30})
	m = model.(TuiModel)
	model, _ = m.Update(finishReqMsg{id: "r1", success: true})
	m = model.(TuiModel)

	if _, ok := m.activeReqs["r1"]; !ok {
		t.Fatal("完成后应立即删除会导致 token/成败不可见，应保留")
	}
	plain := stripANSI(m.buildContent(100))
	if !strings.Contains(plain, "✅ 完成") {
		t.Errorf("应显示完成状态, 实际:\n%s", plain)
	}
	if !strings.Contains(plain, "10/20") {
		t.Errorf("应显示用量, 实际:\n%s", plain)
	}

	// 超过保留时长后由 pruneFinished 清理。
	m.activeReqs["r1"].FinishAt = time.Now().Add(-2 * finishedRetention)
	m.pruneFinished()
	if _, ok := m.activeReqs["r1"]; ok {
		t.Fatal("超过保留时长应被清理")
	}
}

// TestStateCell 锁定截断标注只追加、不改判成败：
// 上游撞输出上限仍是 HTTP 成功，语义为「✅ 完成(输出上限截断)」。
// 列宽不足时退化为紧凑告警标记，保证"有截断"在任何终端宽度下都可见。
func TestStateCell(t *testing.T) {
	const wide = 40
	const narrow = 10
	cases := []struct {
		name  string
		req   *ReqState
		width int
		want  string
	}{
		{name: "nil 行不 panic", req: nil, width: wide, want: ""},
		{name: "无标注时原样", req: &ReqState{State: "✅ 完成"}, width: wide, want: "✅ 完成"},
		{name: "宽终端显示完整标注", req: &ReqState{State: "✅ 完成", Truncation: "输出上限截断"}, width: wide, want: "✅ 完成(输出上限截断)"},
		{name: "工具调用畸形", req: &ReqState{State: "✅ 完成", Truncation: "工具调用畸形"}, width: wide, want: "✅ 完成(工具调用畸形)"},
		{name: "窄终端退化为告警标记", req: &ReqState{State: "✅ 完成", Truncation: "输出上限截断"}, width: narrow, want: "✅ 完成⚠"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stateCell(c.req, c.width); got != c.want {
				t.Fatalf("stateCell(width=%d) = %q, want %q", c.width, got, c.want)
			}
		})
	}
}

// TestUpdateReqTruncation 标注经 TUI 消息落到行上，并出现在 State 列。
// 用宽终端（200 列）以便 State 列容下完整标注；窄终端行为由 TestStateCell 覆盖。
func TestUpdateReqTruncation(t *testing.T) {
	const w = 200
	m := TuiModel{width: w, height: 40, activeReqs: make(map[string]*ReqState)}
	model, _ := m.Update(startReqMsg{id: "r1", streaming: false})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqModelMsg{id: "r1", model: "gemini-3.7-flash"})
	m = model.(TuiModel)
	model, _ = m.Update(updateReqTruncationMsg{id: "r1", label: "输出上限截断"})
	m = model.(TuiModel)

	if got := m.activeReqs["r1"].Truncation; got != "输出上限截断" {
		t.Fatalf("Truncation = %q", got)
	}
	// 收尾后仍应保留标注（FinishReq 会改写 State，标注在渲染期拼接，故不受影响）。
	model, _ = m.Update(finishReqMsg{id: "r1", success: true})
	m = model.(TuiModel)
	plain := stripANSI(m.buildContent(w))
	if !strings.Contains(plain, "输出上限截断") {
		t.Fatalf("State 列应带截断标注, 实际:\n%s", plain)
	}
	if !strings.Contains(plain, "✅ 完成") {
		t.Fatalf("成败判定不得被标注改写, 实际:\n%s", plain)
	}
}
