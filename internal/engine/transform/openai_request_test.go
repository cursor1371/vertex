package transform

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- 请求转换 ----

func TestOpenAIChatToGemini_SystemMerged(t *testing.T) {
	in := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{
			{Role: "system", Content: "你是助手"},
			{Role: "developer", Content: "请简洁"},
			{Role: "user", Content: "你好"},
		},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.SystemInstruction == nil {
		t.Fatal("systemInstruction 未生成")
	}
	if len(out.SystemInstruction.Parts) != 2 {
		t.Fatalf("system 应合并为 2 段，实得 %d", len(out.SystemInstruction.Parts))
	}
	if out.SystemInstruction.Parts[0].Text != "你是助手" || out.SystemInstruction.Parts[1].Text != "请简洁" {
		t.Errorf("system 合并内容不符: %+v", out.SystemInstruction.Parts)
	}
	if len(out.Contents) != 1 || out.Contents[0].Role != "user" {
		t.Fatalf("user 回合不符: %+v", out.Contents)
	}
	if out.Contents[0].Parts[0].Text != "你好" {
		t.Errorf("user 文本不符: %+v", out.Contents[0].Parts)
	}
}

func TestOpenAIChatToGemini_RejectsN(t *testing.T) {
	n := 3
	in := &OpenAIChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		N:        &n,
	}
	if _, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil); err != ErrUnsupportedN {
		t.Fatalf("n>1 应被拒绝，实得 err=%v", err)
	}
}

func TestOpenAIChatToGemini_RejectsEmptyMessages(t *testing.T) {
	if _, _, err := OpenAIChatToGemini(&OpenAIChatRequest{Model: "m"}, "m", nil); err != ErrEmptyMessages {
		t.Fatalf("空 messages 应被拒绝，实得 err=%v", err)
	}
}

func TestOpenAIChatToGemini_ToolCallRoundTrip(t *testing.T) {
	in := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "今天天气"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_abc", Type: "function", Function: OpenAIFunctionCall{Name: "get_weather", Arguments: `{"city":"上海"}`}},
			}},
			{Role: "tool", ToolCallID: "call_abc", Content: "晴 25℃"},
		},
	}
	out, ids, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out.Contents) != 3 {
		t.Fatalf("应有 3 个回合（user/model/user），实得 %d", len(out.Contents))
	}
	// 第二回合：assistant → model + functionCall
	modelTurn := out.Contents[1]
	if modelTurn.Role != "model" {
		t.Fatalf("assistant 应映射为 model，实得 %q", modelTurn.Role)
	}
	if len(modelTurn.Parts) != 1 || modelTurn.Parts[0].FunctionCall == nil {
		t.Fatalf("assistant 应产出 functionCall: %+v", modelTurn.Parts)
	}
	if modelTurn.Parts[0].FunctionCall.Name != "get_weather" {
		t.Errorf("函数名不符: %q", modelTurn.Parts[0].FunctionCall.Name)
	}
	args, _ := modelTurn.Parts[0].FunctionCall.Args.(map[string]any)
	if args["city"] != "上海" {
		t.Errorf("参数解析不符: %+v", args)
	}
	// id → 函数名映射必须建立，供 role=tool 反查
	if ids.nameOf("call_abc") != "get_weather" {
		t.Errorf("tool_call_id 映射未建立: %+v", ids.idToName)
	}
	// 第三回合：tool → user + functionResponse（名字由 id 反查得到）
	toolTurn := out.Contents[2]
	if toolTurn.Role != "user" {
		t.Fatalf("tool 结果应由 user 回合承载，实得 %q", toolTurn.Role)
	}
	if len(toolTurn.Parts) != 1 || toolTurn.Parts[0].FunctionResponse == nil {
		t.Fatalf("tool 应产出 functionResponse: %+v", toolTurn.Parts)
	}
	if toolTurn.Parts[0].FunctionResponse.Name != "get_weather" {
		t.Errorf("functionResponse 名字反查失败: %q", toolTurn.Parts[0].FunctionResponse.Name)
	}
}

func TestOpenAIChatToGemini_ToolResultFallsBackToName(t *testing.T) {
	// tool_call_id 查不到映射时（如客户端未回传 assistant 回合），回退消息自带 name。
	in := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "查天气"},
			{Role: "tool", ToolCallID: "call_unknown", Name: "get_weather", Content: "晴"},
		},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	last := out.Contents[len(out.Contents)-1]
	if last.Parts[0].FunctionResponse == nil || last.Parts[0].FunctionResponse.Name != "get_weather" {
		t.Fatalf("应按 name 回退: %+v", last.Parts)
	}
}

func TestOpenAIChatToGemini_UnresolvableToolResultDropped(t *testing.T) {
	// 既无 id 映射也无 name：无法构造合法 FunctionResponse，应丢弃而非带脏数据上行。
	in := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "查天气"},
			{Role: "tool", ToolCallID: "call_x", Content: "晴"},
		},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("无名 tool 结果应被丢弃，实得 %d 回合", len(out.Contents))
	}
}

func TestOpenAIChatToGemini_ImageDataURI(t *testing.T) {
	// 1x1 透明 PNG
	b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AAAwAB/wFbVwAAAAAASUVORK5CYII="
	in := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{{
			Role: "user",
			Content: []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64," + b64},
			}},
		}},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	p := out.Contents[0].Parts[0]
	if p.InlineData == nil {
		t.Fatalf("图片应转为 inlineData: %+v", p)
	}
	if p.InlineData.MimeType != "image/png" || p.InlineData.Data != b64 {
		t.Errorf("inlineData 不符: %+v", p.InlineData)
	}
}

func TestOpenAIChatToGemini_MalformedDataURIRejected(t *testing.T) {
	in := &OpenAIChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []OpenAIMessage{{
			Role:    "user",
			Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,!!!notbase64!!!"}}},
		}},
	}
	if _, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil); err == nil {
		t.Fatal("畸形 base64 应报错")
	}
}

func TestOpenAIChatToGemini_SamplingAndStop(t *testing.T) {
	temp := 0.7
	topP := 0.9
	maxTok := 1234
	in := &OpenAIChatRequest{
		Model:       "gemini-2.5-flash",
		Messages:    []OpenAIMessage{{Role: "user", Content: "hi"}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTok,
		Stop:        []any{"END", "STOP"},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	gc := out.GenerationConfig
	if gc == nil {
		t.Fatal("GenerationConfig 未生成")
	}
	if gc.Temperature == nil || *gc.Temperature != 0.7 {
		t.Errorf("temperature 不符: %v", gc.Temperature)
	}
	if gc.TopP == nil || *gc.TopP != 0.9 {
		t.Errorf("topP 不符: %v", gc.TopP)
	}
	if gc.MaxOutputTokens == nil || *gc.MaxOutputTokens != 1234 {
		t.Errorf("maxOutputTokens 不符: %v", gc.MaxOutputTokens)
	}
	if len(gc.StopSequences) != 2 || gc.StopSequences[0] != "END" {
		t.Errorf("stopSequences 不符: %v", gc.StopSequences)
	}
}

func TestOpenAIChatToGemini_MaxCompletionTokensPreferred(t *testing.T) {
	mt := 100
	ct := 200
	in := &OpenAIChatRequest{
		Model:               "gemini-2.5-flash",
		Messages:            []OpenAIMessage{{Role: "user", Content: "hi"}},
		MaxTokens:           &mt,
		MaxCompletionTokens: &ct,
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if *out.GenerationConfig.MaxOutputTokens != 200 {
		t.Errorf("应优先 max_completion_tokens，实得 %d", *out.GenerationConfig.MaxOutputTokens)
	}
}

func TestOpenAIChatToGemini_ResponseFormatJSON(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:          "gemini-2.5-flash",
		Messages:       []OpenAIMessage{{Role: "user", Content: "hi"}},
		ResponseFormat: &OpenAIResponseFormat{Type: "json_object"},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("responseMimeType 不符: %q", out.GenerationConfig.ResponseMimeType)
	}
}

func TestOpenAIChatToGemini_ThinkingLevelRespectsWhitelist(t *testing.T) {
	// gemini-3.7-flash 的 AllowedLevels 为 LOW/MEDIUM/HIGH（无 MINIMAL）：
	// 请求 minimal 必须降级到白名单内档位，否则下游 Validate 会返 400。
	in := &OpenAIChatRequest{
		Model:           "gemini-3.7-flash",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: "minimal",
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-3.7-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	tc := out.GenerationConfig.ThinkingConfig
	if tc == nil {
		t.Fatal("ThinkingConfig 未生成")
	}
	spec := TextSpecFor("gemini-3.7-flash")
	if !spec.AllowedLevels[tc.ThinkingLevel] {
		t.Errorf("档位 %q 不在白名单 %v 内", tc.ThinkingLevel, spec.AllowedLevels)
	}
}

func TestOpenAIChatToGemini_ThinkingLevelHigh(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:           "gemini-3.6-flash",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-3.6-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.GenerationConfig.ThinkingConfig.ThinkingLevel != "HIGH" {
		t.Errorf("档位不符: %+v", out.GenerationConfig.ThinkingConfig)
	}
}

func TestOpenAIChatToGemini_ReasoningEffortNoneSkips(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:           "gemini-3.6-flash",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: "none",
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-3.6-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.GenerationConfig != nil && out.GenerationConfig.ThinkingConfig != nil {
		t.Errorf("none 应不注入 thinkingConfig: %+v", out.GenerationConfig.ThinkingConfig)
	}
}

func TestOpenAIChatToGemini_InvalidReasoningEffort(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:           "gemini-3.6-flash",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: "ultra",
	}
	if _, _, err := OpenAIChatToGemini(in, "gemini-3.6-flash", nil); err == nil {
		t.Fatal("非法 reasoning_effort 应报错")
	}
}

func TestOpenAIChatToGemini_ToolsAndChoice(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools: []OpenAITool{{
			Type: "function",
			Function: OpenAIFunctionDef{
				Name:        "get_weather",
				Description: "查天气",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		}},
		ToolChoice: "required",
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools 转换不符: %+v", out.Tools)
	}
	if out.Tools[0].FunctionDeclarations[0].Name != "get_weather" {
		t.Errorf("函数名不符: %+v", out.Tools[0].FunctionDeclarations[0])
	}
	if out.ToolConfig == nil || out.ToolConfig.FunctionCallingConfig == nil ||
		out.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("tool_choice=required 应映射为 ANY: %+v", out.ToolConfig)
	}
}

func TestOpenAIChatToGemini_ToolChoiceNone(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:      "gemini-2.5-flash",
		Messages:   []OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools:      []OpenAITool{{Type: "function", Function: OpenAIFunctionDef{Name: "f"}}},
		ToolChoice: "none",
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.ToolConfig.FunctionCallingConfig.Mode != "NONE" {
		t.Errorf("tool_choice=none 应映射为 NONE: %+v", out.ToolConfig)
	}
}

func TestOpenAIChatToGemini_ToolChoiceSpecificFunction(t *testing.T) {
	in := &OpenAIChatRequest{
		Model:      "gemini-2.5-flash",
		Messages:   []OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools:      []OpenAITool{{Type: "function", Function: OpenAIFunctionDef{Name: "f"}}},
		ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
	}
	out, _, err := OpenAIChatToGemini(in, "gemini-2.5-flash", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	fcc := out.ToolConfig.FunctionCallingConfig
	if fcc.Mode != "ANY" || len(fcc.AllowedFunctionNames) != 1 || fcc.AllowedFunctionNames[0] != "get_weather" {
		t.Errorf("指定函数 tool_choice 不符: %+v", fcc)
	}
}

func TestParseDataURI_NonBase64Encoded(t *testing.T) {
	data, mime, err := parseDataURI("data:text/plain,hello")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mime != "text/plain" {
		t.Errorf("mime 不符: %q", mime)
	}
	// 应被编码为 base64
	if data == "" || data == "hello" {
		t.Errorf("非 base64 data URI 应编码后返回，实得 %q", data)
	}
}

func TestParseDataURI_HTTPSkipped(t *testing.T) {
	data, _, err := parseDataURI("https://example.com/a.png")
	if err != nil {
		t.Fatalf("远端 URL 不应报错: %v", err)
	}
	if data != "" {
		t.Errorf("远端 URL 不应产生 inlineData，实得 %q", data)
	}
}

func TestParseToolArgs_InvalidJSONWrapped(t *testing.T) {
	v := parseToolArgs("not-json")
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("非法 JSON 应包装为 map，实得 %T", v)
	}
	if m["value"] != "not-json" {
		t.Errorf("包装不符: %+v", m)
	}
}

func TestParseToolArgs_ValidJSON(t *testing.T) {
	v := parseToolArgs(`{"a":1}`)
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("合法 JSON 对象应保持 map，实得 %T", v)
	}
	if m["a"] != float64(1) {
		t.Errorf("解析不符: %+v", m)
	}
}

// ---- 响应转换 ----

func TestOpenAIFinishReason_Mapping(t *testing.T) {
	cases := map[string]string{
		"STOP":                      "stop",
		"MAX_TOKENS":                "length",
		"SAFETY":                    "content_filter",
		"PROHIBITED_CONTENT":        "content_filter",
		"RECITATION":                "content_filter",
		"MALFORMED_FUNCTION_CALL":   "tool_calls",
		"FINISH_REASON_UNSPECIFIED": "",
		"":                          "",
		"WHATEVER":                  "",
	}
	for in, want := range cases {
		if got := OpenAIFinishReason(in); got != want {
			t.Errorf("OpenAIFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// testToolCallResolver 是测试用的工具调用 ID 解析器：按函数名稳定分配。
func testToolCallResolver(name string) (string, int) {
	r := NewOpenAIToolCallRegistry()
	return r.Resolve(name)
}

func TestOpenAICompleteResponse_TextAndUsage(t *testing.T) {
	resp := &GeminiResponse{
		Candidates: []*Candidate{{
			Index:        0,
			Content:      &Content{Role: "model", Parts: []Part{{Text: "你好"}}},
			FinishReason: "STOP",
		}},
	}
	out := OpenAICompleteResponse(resp, "gemini-2.5-flash", "chatcmpl-x", 100, 10, 5)
	if out.Object != OpenAIObjectComplete {
		t.Errorf("object 不符: %q", out.Object)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices 数量不符: %d", len(out.Choices))
	}
	c0 := out.Choices[0]
	if c0.Message.Content != "你好" {
		t.Errorf("content 不符: %v", c0.Message.Content)
	}
	if c0.Message.Role != "assistant" {
		t.Errorf("role 不符: %q", c0.Message.Role)
	}
	if c0.FinishReason == nil || *c0.FinishReason != "stop" {
		t.Errorf("finish_reason 不符: %v", c0.FinishReason)
	}
	if out.Usage == nil || out.Usage.PromptTokens != 10 || out.Usage.CompletionTokens != 5 || out.Usage.TotalTokens != 15 {
		t.Errorf("usage 不符: %+v", out.Usage)
	}
}

func TestOpenAICompleteResponse_ThoughtToReasoningContent(t *testing.T) {
	resp := &GeminiResponse{
		Candidates: []*Candidate{{
			Index: 0,
			Content: &Content{Role: "model", Parts: []Part{
				{Text: "让我想想", Thought: true},
				{Text: "答案是 42"},
			}},
			FinishReason: "STOP",
		}},
	}
	out := OpenAICompleteResponse(resp, "m", "id", 1, 1, 1)
	msg := out.Choices[0].Message
	if msg.ReasoningContent != "让我想想" {
		t.Errorf("reasoning_content 不符: %q", msg.ReasoningContent)
	}
	if msg.Content != "答案是 42" {
		t.Errorf("content 不应含思考内容: %v", msg.Content)
	}
}

func TestOpenAICompleteResponse_ToolCalls(t *testing.T) {
	resp := &GeminiResponse{
		Candidates: []*Candidate{{
			Index: 0,
			Content: &Content{Role: "model", Parts: []Part{
				{FunctionCall: &FunctionCall{Name: "get_weather", Args: map[string]any{"city": "上海"}}},
			}},
			FinishReason: "STOP",
		}},
	}
	out := OpenAICompleteResponse(resp, "m", "id", 1, 1, 1)
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量不符: %d", len(calls))
	}
	if calls[0].ID == "" || !strings.HasPrefix(calls[0].ID, "call_") {
		t.Errorf("tool_call id 应自动生成且带 call_ 前缀: %q", calls[0].ID)
	}
	if calls[0].Type != "function" || calls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call 内容不符: %+v", calls[0])
	}
	// arguments 必须是 JSON 字符串
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments 应为合法 JSON: %v", err)
	}
	if args["city"] != "上海" {
		t.Errorf("arguments 解析不符: %+v", args)
	}
}

func TestOpenAICompleteResponse_EmptyIsContentFilter(t *testing.T) {
	// 安全拦截场景：无 candidates。必须以 content_filter 收尾而非解析失败。
	out := OpenAICompleteResponse(&GeminiResponse{}, "m", "id", 1, 1, 0)
	if len(out.Choices) != 1 {
		t.Fatalf("应补一个空 choice: %+v", out.Choices)
	}
	if out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != "content_filter" {
		t.Errorf("空响应应标记 content_filter: %v", out.Choices[0].FinishReason)
	}
}

func TestOpenAICompleteResponse_NilFinishDefaultsLength(t *testing.T) {
	// 非流式下 finish_reason 必须有值。空 finishReason（干净 EOF 无终帧，
	// 典型是抗截断 partial）如实映射为 length——伪造 stop 会把截断伪装成正常完成。
	resp := &GeminiResponse{
		Candidates: []*Candidate{{Index: 0, Content: &Content{Role: "model", Parts: []Part{{Text: "x"}}}}},
	}
	out := OpenAICompleteResponse(resp, "m", "id", 1, 1, 1)
	if *out.Choices[0].FinishReason != "length" {
		t.Errorf("非流式空 finishReason 应为 length，实得 %q", *out.Choices[0].FinishReason)
	}
}

func TestOpenAICompleteResponse_UnknownFinishStillStop(t *testing.T) {
	// 非空但未映射的 finishReason 仍是真实终帧（扫描层见到真实终帧才停），
	// 按 stop 兜底而非 length。
	resp := &GeminiResponse{
		Candidates: []*Candidate{{Index: 0, FinishReason: "OTHER",
			Content: &Content{Role: "model", Parts: []Part{{Text: "x"}}}}},
	}
	out := OpenAICompleteResponse(resp, "m", "id", 1, 1, 1)
	if *out.Choices[0].FinishReason != "stop" {
		t.Errorf("未知枚举应按 stop 兜底，实得 %q", *out.Choices[0].FinishReason)
	}
}

func TestOpenAIStreamChunk_FirstCarriesRole(t *testing.T) {
	chunk := &GeminiChunk{
		Candidates: []*Candidate{{Index: 0, Content: &Content{Role: "model", Parts: []Part{{Text: "你"}}}}},
	}
	out := OpenAIStreamChunk(chunk, "m", "id", 1, true, testToolCallResolver)
	if out == nil {
		t.Fatal("首帧不应为 nil")
	}
	if out.Object != OpenAIObjectChunk {
		t.Errorf("object 不符: %q", out.Object)
	}
	if out.Choices[0].Delta.Role != "assistant" {
		t.Errorf("首帧应携带 role: %+v", out.Choices[0].Delta)
	}
	if out.Choices[0].Delta.Content != "你" {
		t.Errorf("content 不符: %q", out.Choices[0].Delta.Content)
	}
	// 中间帧的 finish_reason 必须为 null（nil）
	if out.Choices[0].FinishReason != nil {
		t.Errorf("中间帧 finish_reason 应为 null，实得 %v", *out.Choices[0].FinishReason)
	}
}

func TestOpenAIStreamChunk_SkipsEmptyMetadata(t *testing.T) {
	// 纯 usageMetadata 帧（无 candidates）：不应产出 chunk。
	chunk := &GeminiChunk{UsageMetadata: &UsageMetadata{TotalTokenCount: 10}}
	out := OpenAIStreamChunk(chunk, "m", "id", 1, false, testToolCallResolver)
	if out != nil {
		t.Errorf("无实质内容帧应返回 nil，实得 %+v", out)
	}
}

func TestOpenAIStreamChunk_FinishReasonSet(t *testing.T) {
	chunk := &GeminiChunk{
		Candidates: []*Candidate{{Index: 0, FinishReason: "STOP", Content: &Content{Role: "model", Parts: []Part{{Text: "。"}}}}},
	}
	out := OpenAIStreamChunk(chunk, "m", "id", 1, false, testToolCallResolver)
	if out == nil || out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != "stop" {
		t.Fatalf("终帧应带 stop: %+v", out)
	}
}

func TestOpenAIStreamChunk_ReasoningDelta(t *testing.T) {
	chunk := &GeminiChunk{
		Candidates: []*Candidate{{Index: 0, Content: &Content{Role: "model", Parts: []Part{{Text: "思考中", Thought: true}}}}},
	}
	out := OpenAIStreamChunk(chunk, "m", "id", 1, false, testToolCallResolver)
	if out.Choices[0].Delta.ReasoningContent != "思考中" {
		t.Errorf("reasoning_content 不符: %+v", out.Choices[0].Delta)
	}
	if out.Choices[0].Delta.Content != "" {
		t.Errorf("思考内容不应进 content: %q", out.Choices[0].Delta.Content)
	}
}

func TestOpenAIUsageChunk(t *testing.T) {
	out := OpenAIUsageChunk("m", "id", 1, 7, 3)
	if len(out.Choices) != 0 {
		t.Errorf("usage 帧不应带 choices: %+v", out.Choices)
	}
	if out.Usage == nil || out.Usage.TotalTokens != 10 {
		t.Errorf("usage 不符: %+v", out.Usage)
	}
}

func TestOpenAIErrorOf(t *testing.T) {
	cases := []struct {
		code int
		kind string
		want OpenAIErrorKind
	}{
		{400, "invalid", OpenAIErrKindInvalid},
		{404, "notfound", OpenAIErrKindNotFound},
		{403, "permission", OpenAIErrKindPermission},
		{429, "ratelimit", OpenAIErrKindRateLimit},
		{502, "auth", OpenAIErrKindServer},
		{0, "", OpenAIErrKindServer},
	}
	for _, c := range cases {
		got, msg := OpenAIErrorOf(c.code, c.kind, "raw", "friendly", "")
		if got != c.want {
			t.Errorf("OpenAIErrorOf(%d,%q) = %q, want %q", c.code, c.kind, got, c.want)
		}
		if !strings.Contains(msg, "friendly") || !strings.Contains(msg, "raw") {
			t.Errorf("错误文案应含友好提示与原始信息: %q", msg)
		}
	}
}

// ---- 常量不变式 ----

func TestOpenAIStreamConstants(t *testing.T) {
	// new-api 等网关以 "data: [DONE]" 判定流结束，格式不可变。
	if OpenAIStreamDone != "data: [DONE]\n\n" {
		t.Errorf("[DONE] 帧格式不符: %q", OpenAIStreamDone)
	}
	// 心跳必须是 SSE 注释行，否则会被客户端当成内容解析。
	if !strings.HasPrefix(OpenAIHeartbeat, ":") {
		t.Errorf("心跳必须是注释帧: %q", OpenAIHeartbeat)
	}
}

func TestNewOpenAIRequestID(t *testing.T) {
	a := NewOpenAIRequestID()
	b := NewOpenAIRequestID()
	if !strings.HasPrefix(a, "chatcmpl-") {
		t.Errorf("id 前缀不符: %q", a)
	}
	if a == b {
		t.Errorf("连续两次生成的 id 不应相同: %q", a)
	}
}
