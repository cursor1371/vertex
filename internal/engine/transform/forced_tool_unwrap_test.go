package transform

import (
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

const testForcedName = "vertex_emit_0123456789abcdef01234567"

func forcedCall(name string, args any) Part {
	return Part{FunctionCall: &FunctionCall{Name: name, Args: args}}
}

func partTexts(parts []Part) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Text)
	}
	return out
}

func TestExtractForcedContent(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{name: "完整对象", raw: `{"content":"你好"}`, want: "你好", wantOK: true},
		{name: "对象含其它字段", raw: `{"meta":1,"content":"答案"}`, want: "答案", wantOK: true},
		{name: "content 非字符串-数字", raw: `{"content":42}`, want: "42", wantOK: true},
		{name: "content 非字符串-对象", raw: `{"content":{"a":1}}`, want: `{"a":1}`, wantOK: true},
		{name: "content 为 null", raw: `{"content":null}`, wantOK: false},
		{name: "缺少 content", raw: `{"other":"x"}`, wantOK: false},
		{name: "空串", raw: ``, wantOK: false},
		{name: "纯空白", raw: "   ", wantOK: false},
		{name: "Markdown 围栏", raw: "```json\n{\"content\":\"围栏内\"}\n```", want: "围栏内", wantOK: true},
		{name: "截断 JSON", raw: `{"content":"截断正文"`, want: "截断正文", wantOK: true},
		{name: "转义引号", raw: `{"content":"他说\"你好\""}`, want: `他说"你好"`, wantOK: true},
		{name: "转义换行", raw: `{"content":"第一行\n第二行"}`, want: "第一行\n第二行", wantOK: true},
		{name: "Unicode 转义", raw: `{"content":"\u4e2d\u6587"}`, want: "中文", wantOK: true},
		{name: "JSON 特有转义斜杠", raw: `{"content":"a\/b"}`, want: "a/b", wantOK: true},
		{name: "完全不可解析", raw: "not json at all", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractForcedContent(c.raw)
			if ok != c.wantOK {
				t.Fatalf("extractForcedContent(%q) ok=%v, 期望 %v (got=%q)", c.raw, ok, c.wantOK, got)
			}
			if ok && got != c.want {
				t.Fatalf("extractForcedContent(%q) = %q, 期望 %q", c.raw, got, c.want)
			}
		})
	}
}

func TestUnwrapForcedToolCall_SingleCall(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			forcedCall(testForcedName, map[string]any{"content": "正文"}),
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "正文" || parts[0].FunctionCall != nil {
		t.Fatalf("解包结果不符: %+v", parts)
	}
}

// TestUnwrapForcedToolCall_PartOrder 锁定"保持原 part 顺序"：
// thought, 合成调用, 真实调用 → thought, 正文, 真实调用（正文回填到原位而非追加到末尾）。
func TestUnwrapForcedToolCall_PartOrder(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{Text: "思考中", Thought: true},
			forcedCall(testForcedName, map[string]any{"content": "正文"}),
			forcedCall("real_tool", map[string]any{"q": "1"}),
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 3 {
		t.Fatalf("part 数应为 3，实际 %d: %+v", len(parts), parts)
	}
	if !parts[0].Thought || parts[0].Text != "思考中" {
		t.Fatalf("thought 未被保留在原位: %+v", parts[0])
	}
	if parts[1].Text != "正文" {
		t.Fatalf("正文未回填到合成调用的原位: %+v", parts[1])
	}
	if parts[2].FunctionCall == nil || parts[2].FunctionCall.Name != "real_tool" {
		t.Fatalf("真实 functionCall 未被保留: %+v", parts[2])
	}
}

func TestUnwrapForcedToolCall_ContentConflict(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{Text: "契约外的普通文本"},
			forcedCall(testForcedName, map[string]any{"content": "权威正文"}),
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "权威正文" {
		t.Fatalf("普通文本应被丢弃、只留权威正文: %+v", parts)
	}
}

// TestUnwrapForcedToolCall_FallbackToPlainText 解包失败时必须回退普通文本，绝不交付空响应。
func TestUnwrapForcedToolCall_FallbackToPlainText(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{Text: "可交付的普通文本"},
			forcedCall(testForcedName, "完全不可解析的 args"),
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "可交付的普通文本" {
		t.Fatalf("应回退普通文本: %+v", parts)
	}
}

func TestUnwrapForcedToolCall_NoForcedCallIsZeroMutation(t *testing.T) {
	orig := []Part{{Text: "普通回答"}, {Text: "思考", Thought: true}}
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: append([]Part(nil), orig...)},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)
	got := resp.Candidates[0].Content.Parts
	if len(got) != len(orig) {
		t.Fatalf("未出现合成调用时不应改动 parts: %+v", got)
	}
	for i := range orig {
		if got[i].Text != orig[i].Text || got[i].Thought != orig[i].Thought {
			t.Fatalf("part[%d] 被改动: %+v", i, got[i])
		}
	}
}

func TestUnwrapForcedToolCall_DisabledIsNoOp(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			forcedCall(testForcedName, map[string]any{"content": "正文"}),
		}},
	}}}
	UnwrapForcedToolCall(resp, "")
	if resp.Candidates[0].Content.Parts[0].FunctionCall == nil {
		t.Fatal("name 为空时不应解包")
	}
}

func TestUnwrapForcedToolCall_MultiCandidate(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{
		{Index: 0, Content: &Content{Role: "model", Parts: []Part{
			forcedCall(testForcedName, map[string]any{"content": "候选零"})}}},
		{Index: 1, Content: &Content{Role: "model", Parts: []Part{
			forcedCall(testForcedName, map[string]any{"content": "候选一"})}}},
	}}
	UnwrapForcedToolCall(resp, testForcedName)
	if resp.Candidates[0].Content.Parts[0].Text != "候选零" {
		t.Fatalf("候选 0 解包错误: %+v", resp.Candidates[0].Content.Parts)
	}
	if resp.Candidates[1].Content.Parts[0].Text != "候选一" {
		t.Fatalf("候选 1 解包错误: %+v", resp.Candidates[1].Content.Parts)
	}
}

func TestForcedToolUnwrapper_NilSafe(t *testing.T) {
	u := NewForcedToolUnwrapper("", true)
	if u != nil {
		t.Fatal("name 为空时应返回 nil")
	}
	chunk := &GeminiChunk{Candidates: []*Candidate{{Index: 0, Content: &Content{Parts: []Part{{Text: "x"}}}}}}
	u.Rewrite(chunk)
	if chunk.Candidates[0].Content.Parts[0].Text != "x" {
		t.Fatal("nil 接收者不应改动 chunk")
	}
	if u.Flush() != nil {
		t.Fatal("nil 接收者 Flush 应返回 nil")
	}
}

func TestForcedToolUnwrapper_SingleFrame(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	chunk := &GeminiChunk{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			forcedCall(testForcedName, map[string]any{"content": "完整正文"}),
		}},
	}}}
	u.Rewrite(chunk)
	parts := chunk.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "完整正文" || parts[0].FunctionCall != nil {
		t.Fatalf("单帧解包结果不符: %+v", parts)
	}
}

// TestForcedToolUnwrapper_CrossFrameArgs 覆盖上游把 args 拆成多帧字符串的场景：
// 增量解码逐帧产出已完整解码的字符（流式体验），累积拼接完整、无重复、
// 不泄漏半截转义序列或半截 JSON。
func TestForcedToolUnwrapper_CrossFrameArgs(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)

	frame1 := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, `{"content":"hel`)}},
	}}}
	u.Rewrite(frame1)
	// 增量语义：首帧即产出已解码前缀（{"content":"hel → "hel"）。
	if got := partTexts(frame1.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "hel" {
		t.Fatalf("首帧应增量产出 hel: %v", got)
	}

	frame2 := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, `lo"}`)}},
	}}}
	u.Rewrite(frame2)
	// 第二帧只补增量，拼接后恰好是完整正文。
	if got := partTexts(frame2.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "lo" {
		t.Fatalf("第二帧应只补增量 lo: %v", got)
	}
}

// TestForcedToolUnwrapper_NoDuplicateEmission 覆盖 args 帧 + finish 帧：正文只能发一次。
func TestForcedToolUnwrapper_NoDuplicateEmission(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)

	argsFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, map[string]any{"content": "正文"})}},
	}}}
	u.Rewrite(argsFrame)
	if got := partTexts(argsFrame.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "正文" {
		t.Fatalf("args 帧应发出正文: %v", got)
	}

	finishFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:        0,
		Content:      &Content{Role: "model", Parts: []Part{}},
		FinishReason: "STOP",
	}}}
	u.Rewrite(finishFrame)
	if got := partTexts(finishFrame.Candidates[0].Content.Parts); len(got) != 0 {
		t.Fatalf("finish 帧不得重复发出正文: %v", got)
	}
}

// TestForcedToolUnwrapper_MetadataPreserved 锁定"就地改写"：元数据必须全部保留。
func TestForcedToolUnwrapper_MetadataPreserved(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	usage := &UsageMetadata{PromptTokenCount: 7, TotalTokenCount: 9}
	chunk := &GeminiChunk{
		UsageMetadata: usage,
		ModelVersion:  "gemini-3.7",
		Candidates: []*Candidate{{
			Index:         0,
			FinishReason:  "STOP",
			SafetyRatings: []any{map[string]any{"category": "HARM_CATEGORY_HATE_SPEECH"}},
			Content: &Content{Role: "model", Parts: []Part{
				forcedCall(testForcedName, map[string]any{"content": "正文"}),
			}},
		}},
	}
	u.Rewrite(chunk)
	if chunk.UsageMetadata == nil || chunk.UsageMetadata.TotalTokenCount != 9 {
		t.Fatalf("UsageMetadata 丢失: %+v", chunk.UsageMetadata)
	}
	if chunk.ModelVersion != "gemini-3.7" {
		t.Fatalf("ModelVersion 丢失: %q", chunk.ModelVersion)
	}
	if chunk.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("FinishReason 丢失: %q", chunk.Candidates[0].FinishReason)
	}
	if len(chunk.Candidates[0].SafetyRatings) != 1 {
		t.Fatalf("SafetyRatings 丢失: %+v", chunk.Candidates[0].SafetyRatings)
	}
}

// TestForcedToolUnwrapper_PerCandidateBuckets 多候选的 args 必须分桶，不得互相污染。
// 增量解码器状态同样按候选分桶（含跨帧的转义/代理项状态）。
func TestForcedToolUnwrapper_PerCandidateBuckets(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	chunk := &GeminiChunk{Candidates: []*Candidate{
		{Index: 0, Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, `{"content":"hel`)}}},
		{Index: 1, Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, `{"content":"wor`)}}},
	}}
	u.Rewrite(chunk)
	// 首帧即各自产出前缀，且互不串桶。
	if got := partTexts(chunk.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "hel" {
		t.Fatalf("候选 0 首帧前缀错误: %v", got)
	}
	if got := partTexts(chunk.Candidates[1].Content.Parts); len(got) != 1 || got[0] != "wor" {
		t.Fatalf("候选 1 首帧前缀错误（可能被候选 0 污染）: %v", got)
	}

	chunk2 := &GeminiChunk{Candidates: []*Candidate{
		{Index: 0, Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, `lo"}`)}}},
		{Index: 1, Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, `ld"}`)}}},
	}}
	u.Rewrite(chunk2)

	if got := partTexts(chunk2.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "lo" {
		t.Fatalf("候选 0 增量错误（可能被候选 1 污染）: %v", got)
	}
	if got := partTexts(chunk2.Candidates[1].Content.Parts); len(got) != 1 || got[0] != "ld" {
		t.Fatalf("候选 1 增量错误: %v", got)
	}
}

func TestForcedToolUnwrapper_PassthroughParts(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	chunk := &GeminiChunk{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{Text: "思考", Thought: true},
			forcedCall("real_tool", map[string]any{"q": "1"}),
		}},
	}}}
	u.Rewrite(chunk)
	parts := chunk.Candidates[0].Content.Parts
	if len(parts) != 2 {
		t.Fatalf("thought 与真实 functionCall 均应保留: %+v", parts)
	}
	if !parts[0].Thought || parts[1].FunctionCall == nil {
		t.Fatalf("透传内容不符: %+v", parts)
	}
}

// TestForcedToolUnwrapper_RequiredBuffersPlainText 强制档：普通文本先缓冲，
// 合成调用出现即按单一真源丢弃。
func TestForcedToolUnwrapper_RequiredBuffersPlainText(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)

	plainFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{{Text: "契约外普通文本"}}},
	}}}
	u.Rewrite(plainFrame)
	if got := partTexts(plainFrame.Candidates[0].Content.Parts); len(got) != 0 {
		t.Fatalf("强制档下普通文本应先缓冲、不立即下发: %v", got)
	}

	argsFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, map[string]any{"content": "权威正文"})}},
	}}}
	u.Rewrite(argsFrame)
	if got := partTexts(argsFrame.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "权威正文" {
		t.Fatalf("应只下发权威正文: %v", got)
	}
}

// TestForcedToolUnwrapper_RequiredFlushesPlainTextOnFinish 强制档下模型未遵守契约
// （只回普通文本）时，终结帧必须补发缓冲文本，不得丢内容。
func TestForcedToolUnwrapper_RequiredFlushesPlainTextOnFinish(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)

	plainFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{{Text: "模型未遵守契约的正文"}}},
	}}}
	u.Rewrite(plainFrame)

	finishFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:        0,
		Content:      &Content{Role: "model", Parts: []Part{}},
		FinishReason: "STOP",
	}}}
	u.Rewrite(finishFrame)
	if got := partTexts(finishFrame.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "模型未遵守契约的正文" {
		t.Fatalf("终结帧应补发缓冲文本: %v", got)
	}
}

// TestForcedToolUnwrapper_PreferStreamsPlainTextImmediately 优先档：普通文本立即透传以保留流式体验。
func TestForcedToolUnwrapper_PreferStreamsPlainTextImmediately(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, false)
	chunk := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{{Text: "流式增量"}}},
	}}}
	u.Rewrite(chunk)
	if got := partTexts(chunk.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "流式增量" {
		t.Fatalf("优先档应立即透传普通文本: %v", got)
	}
}

func TestForcedToolUnwrapper_Flush(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	plainFrame := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{{Text: "异常结束前的内容"}}},
	}}}
	u.Rewrite(plainFrame)

	extra := u.Flush()
	if extra == nil || len(extra.Candidates) != 1 {
		t.Fatalf("Flush 应补发缓冲文本: %+v", extra)
	}
	if extra.Candidates[0].Content.Parts[0].Text != "异常结束前的内容" {
		t.Fatalf("Flush 内容不符: %+v", extra.Candidates[0].Content.Parts)
	}
	if extra.Candidates[0].FinishReason != "" {
		t.Fatalf("Flush 不得伪造 finishReason: %q", extra.Candidates[0].FinishReason)
	}
	if u.Flush() != nil {
		t.Fatal("Flush 不得重复补发")
	}
}

// ---- 请求侧注入 ----

func forcedTestCfg(mode string) config.ConfigProvider {
	return &dummyConfig{forcedMode: mode}
}

func TestBuildVariables_ForcedRequiredInjection(t *testing.T) {
	const model = "gemini-3.7-flash"
	req := &GeminiRequest{
		Contents:       []Content{{Role: "user", Parts: []Part{{Text: "帮我写个快排"}}}},
		Tools:          []Tool{{FunctionDeclarations: []FunctionDeclaration{{Name: "client_tool"}}}},
		ForcedToolName: testForcedName,
		ForcedToolMode: config.ForcedModeRequired,
	}
	out := SharedModelFamilyRouter().For(model).BuildVariables(model, req, forcedTestCfg(config.ForcedModeRequired))

	if out.ToolConfig == nil || out.ToolConfig.FunctionCallingConfig == nil ||
		out.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("强制档应设置 mode=ANY: %+v", out.ToolConfig)
	}
	if out.ToolConfig.FunctionCallingConfig.AllowedFunctionNames != nil {
		t.Fatalf("allowedFunctionNames 应保持 nil: %v", out.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
	}

	names := CollectToolNames(out.Tools)
	if !names["client_tool"] {
		t.Fatalf("客户端工具被删除: %v", names)
	}
	if !names[testForcedName] {
		t.Fatalf("合成工具未注入: %v", names)
	}
	if len(names) != 2 {
		t.Fatalf("应恰好有 2 个函数声明: %v", names)
	}

	// 控制指令已与相邻 user 回合合并，且点名合成工具。
	last := out.Contents[len(out.Contents)-1]
	if last.Role != "user" {
		t.Fatalf("控制指令应为 user 角色: %q", last.Role)
	}
	joined := ""
	for _, p := range last.Parts {
		joined += p.Text
	}
	if !strings.Contains(joined, testForcedName) {
		t.Fatalf("控制指令未点名合成工具: %q", joined)
	}
	if !strings.Contains(joined, "帮我写个快排") {
		t.Fatalf("原始用户内容丢失: %q", joined)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("控制指令应与相邻 user 回合合并为 1 个回合，实际 %d", len(out.Contents))
	}
}

func TestBuildVariables_ForcedPreferKeepsToolConfigAbsent(t *testing.T) {
	const model = "gemini-3.7-flash"
	req := &GeminiRequest{
		Contents:       []Content{{Role: "user", Parts: []Part{{Text: "hi"}}}},
		ForcedToolName: testForcedName,
		ForcedToolMode: config.ForcedModePrefer,
	}
	out := SharedModelFamilyRouter().For(model).BuildVariables(model, req, forcedTestCfg(config.ForcedModePrefer))

	if out.ToolConfig != nil {
		t.Fatalf("优先档不得覆盖客户端 toolConfig（nil 时上游 payload 中整个字段不出现）: %+v", out.ToolConfig)
	}
	if !CollectToolNames(out.Tools)[testForcedName] {
		t.Fatal("优先档仍应注入合成工具")
	}
}

// TestBuildVariables_NotForcedIsZeroChange 关闭档下不得引入任何注入。
func TestBuildVariables_NotForcedIsZeroChange(t *testing.T) {
	const model = "gemini-3.7-flash"
	req := &GeminiRequest{
		Contents: []Content{{Role: "user", Parts: []Part{{Text: "hi"}}}},
	}
	out := SharedModelFamilyRouter().For(model).BuildVariables(model, req, forcedTestCfg(config.ForcedModeOff))

	if out.Tools != nil {
		t.Fatalf("关闭档不得注入工具: %+v", out.Tools)
	}
	if out.ToolConfig != nil {
		t.Fatalf("关闭档不得设置 toolConfig: %+v", out.ToolConfig)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("关闭档不得注入控制指令，回合数应为 1，实际 %d", len(out.Contents))
	}
	if out.Contents[0].Parts[0].Text != "hi" {
		t.Fatalf("关闭档用户内容被改动: %+v", out.Contents[0].Parts)
	}
}
