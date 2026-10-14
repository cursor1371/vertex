package transform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// 本文件实现 OpenAI Chat Completions 请求 → Gemini 原生请求的转换。
//
// 关键约束：
//  1. 产出的 *GeminiRequest 直接交给 executePipelineChat，不得再被并发候选共享写入
//     （竞速下 N 个候选并发跑 BuildVariables，写入安全由该层保证）；
//  2. 转换必须在 prepareForcedTool 之前完成——强制工具调用依赖 req.Tools 的最终形态；
//  3. 不支持的入参显式返回错误（如 n>1），不做静默降级，避免客户端拿到"看似成功"的错误结果。

// ErrUnsupportedN 是 n>1 的拒绝哨兵：本代理 GenerationConfig 无 candidateCount 字段，
// 上游不支持一次返回多条候选。
var ErrUnsupportedN = errors.New("n>1 is not supported (upstream has no candidateCount)")

// ErrEmptyMessages 是空消息列表的拒绝哨兵。
var ErrEmptyMessages = errors.New("messages must not be empty")

// toolIDMap 是 OpenAI tool_call.id → Gemini 函数名的映射。
//
// 必要性：Gemini 的 FunctionCall 上行会被 SignatureResolver 清空 ID 字段
// （signature.go:56），而 FunctionResponse 没有 ID 通道。因此 OpenAI 侧的
// tool_call_id ↔ 函数名对应关系必须由本层自行维护。
type toolIDMap struct {
	idToName map[string]string
	seq      int
}

func newToolIDMap() *toolIDMap {
	return &toolIDMap{idToName: map[string]string{}}
}

// nameOf 反查 id 对应的函数名；未命中返回空串。
func (m *toolIDMap) nameOf(id string) string {
	if m == nil {
		return ""
	}
	return m.idToName[id]
}

// remember 登记 id → 函数名映射。
func (m *toolIDMap) remember(id, name string) {
	if m == nil || id == "" {
		return
	}
	m.idToName[id] = name
}

// generate 生成一个 OpenAI 风格的 tool_call id（call_ 前缀 + 序号），保证同一次请求内唯一。
func (m *toolIDMap) generate() string {
	m.seq++
	return fmt.Sprintf("call_%s", randomHex8(m.seq))
}

// randomHex8 生成短标识：优先用自增序号保持确定性（便于测试），避免引入全局随机源。
func randomHex8(seq int) string {
	return fmt.Sprintf("%08x", seq)
}

// OpenAIChatToGemini 把 OpenAI 请求转换为 Gemini 原生请求。
//
// cfg 为 nil 时跳过思考档位白名单校验（仅在单测中如此调用，生产恒非 nil）。
// 返回的 *GeminiRequest 是新分配的独占对象，调用方可安全交给竞速管线。
func OpenAIChatToGemini(in *OpenAIChatRequest, actualModel string, cfg config.ConfigProvider) (*GeminiRequest, *toolIDMap, error) {
	if in == nil {
		return nil, nil, ErrEmptyMessages
	}
	if len(in.Messages) == 0 {
		return nil, nil, ErrEmptyMessages
	}
	if in.N != nil && *in.N > 1 {
		return nil, nil, ErrUnsupportedN
	}

	ids := newToolIDMap()
	out := &GeminiRequest{}
	var systemParts []Part

	for _, msg := range in.Messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			// OpenAI 允许多条 system 消息；Gemini 只有一个 systemInstruction，故合并。
			if text := messageText(msg.Content); text != "" {
				systemParts = append(systemParts, Part{Text: text})
			}
		case "user":
			parts, err := contentToParts(msg.Content)
			if err != nil {
				return nil, nil, err
			}
			if len(parts) > 0 {
				out.Contents = append(out.Contents, Content{Role: "user", Parts: parts})
			}
		case "assistant":
			parts := assistantToParts(msg, ids)
			if len(parts) > 0 {
				out.Contents = append(out.Contents, Content{Role: "model", Parts: parts})
			}
		case "tool":
			parts := toolResultToParts(msg, ids)
			if len(parts) > 0 {
				// Gemini 约定工具结果由 user 回合承载。
				out.Contents = append(out.Contents, Content{Role: "user", Parts: parts})
			}
		default:
			// 未知角色：按 user 处理，避免静默丢消息（OpenAI 客户端偶发自定义角色）。
			parts, err := contentToParts(msg.Content)
			if err != nil {
				return nil, nil, err
			}
			if len(parts) > 0 {
				out.Contents = append(out.Contents, Content{Role: "user", Parts: parts})
			}
		}
	}

	if len(systemParts) > 0 {
		out.SystemInstruction = &Content{Role: "user", Parts: systemParts}
	}
	if len(out.Contents) == 0 {
		return nil, nil, ErrEmptyMessages
	}

	if err := applyGenerationConfig(out, in, actualModel, cfg); err != nil {
		return nil, nil, err
	}
	if err := applyTools(out, in, ids); err != nil {
		return nil, nil, err
	}
	return out, ids, nil
}

// messageText 抽取消息的纯文本内容（content 为 string 时直取；为数组时拼接 text 段）。
// 用于 system/developer 这类不接受多模态的角色。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	case []OpenAIContentPart:
		var sb strings.Builder
		for _, p := range v {
			if strings.EqualFold(p.Type, "text") {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// contentToParts 把 user 消息的 content 转为 Gemini parts（支持多模态）。
func contentToParts(content any) ([]Part, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []Part{{Text: v}}, nil
	case []any:
		var out []Part
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			p, err := contentPartMapToPart(m)
			if err != nil {
				return nil, err
			}
			if p != nil {
				out = append(out, *p)
			}
		}
		return out, nil
	case []OpenAIContentPart:
		var out []Part
		for i := range v {
			p, err := contentPartTypedToPart(&v[i])
			if err != nil {
				return nil, err
			}
			if p != nil {
				out = append(out, *p)
			}
		}
		return out, nil
	default:
		// 未知形态（数字/布尔等）：转字符串兜底，不静默丢弃。
		return []Part{{Text: fmt.Sprintf("%v", v)}}, nil
	}
}

func contentPartMapToPart(m map[string]any) (*Part, error) {
	typ, _ := m["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "text":
		t, _ := m["text"].(string)
		if t == "" {
			return nil, nil
		}
		return &Part{Text: t}, nil
	case "image_url":
		urlObj, _ := m["image_url"].(map[string]any)
		raw, _ := urlObj["url"].(string)
		if raw == "" {
			return nil, nil
		}
		data, mime, err := parseDataURI(raw)
		if err != nil {
			return nil, err
		}
		if data == "" {
			return nil, nil // http(s) URL：Gemini 侧应走 fileData，本代理暂不支持远端拉取
		}
		return &Part{InlineData: &InlineData{MimeType: mime, Data: data}}, nil
	case "input_audio":
		audioObj, _ := m["input_audio"].(map[string]any)
		dataB64, _ := audioObj["data"].(string)
		if dataB64 == "" {
			return nil, nil
		}
		format, _ := audioObj["format"].(string)
		return &Part{InlineData: &InlineData{MimeType: audioMimeOf(format), Data: dataB64}}, nil
	default:
		return nil, nil
	}
}

func contentPartTypedToPart(p *OpenAIContentPart) (*Part, error) {
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "text":
		if p.Text == "" {
			return nil, nil
		}
		return &Part{Text: p.Text}, nil
	case "image_url":
		if p.ImageURL == nil || p.ImageURL.URL == "" {
			return nil, nil
		}
		data, mime, err := parseDataURI(p.ImageURL.URL)
		if err != nil {
			return nil, err
		}
		if data == "" {
			return nil, nil
		}
		return &Part{InlineData: &InlineData{MimeType: mime, Data: data}}, nil
	case "input_audio":
		if p.InputAudio == nil || p.InputAudio.Data == "" {
			return nil, nil
		}
		return &Part{InlineData: &InlineData{MimeType: audioMimeOf(p.InputAudio.Format), Data: p.InputAudio.Data}}, nil
	default:
		return nil, nil
	}
}

// parseDataURI 解析 data: URI，返回 (base64 载荷, mime, error)。
// 非 data: 前缀（http/https）返回空载荷且不报错——由调用方决定如何处理。
func parseDataURI(raw string) (data, mime string, err error) {
	if !strings.HasPrefix(raw, "data:") {
		return "", "", nil
	}
	rest := raw[len("data:"):]
	idx := strings.Index(rest, ",")
	if idx < 0 {
		return "", "", errors.New("malformed data URI: missing comma")
	}
	meta := rest[:idx]
	payload := rest[idx+1:]
	if strings.HasSuffix(meta, ";base64") {
		mime = strings.TrimSuffix(meta, ";base64")
		// 校验确实是合法 base64，避免把脏数据带给上游。
		if _, decErr := base64.StdEncoding.DecodeString(payload); decErr != nil {
			return "", "", errors.New("malformed data URI: invalid base64 payload")
		}
		return payload, mime, nil
	}
	// 非 base64 的 data URI（如 data:text/plain,hello）：自行编码后透传。
	if mime == "" {
		mime = meta
	}
	return base64.StdEncoding.EncodeToString([]byte(payload)), mime, nil
}

// audioMimeOf 把 OpenAI 的音频格式名映射为 mime。未识别时回退 wav。
func audioMimeOf(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "mp3":
		return "audio/mp3"
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	case "ogg":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	default:
		return "audio/wav"
	}
}

// assistantToParts 把 assistant 消息（含 tool_calls）转为 Gemini parts。
// 每个 tool_call 都会登记 id → 函数名映射，供后续 role=tool 的消息反查。
func assistantToParts(msg OpenAIMessage, ids *toolIDMap) []Part {
	var out []Part
	if text := messageText(msg.Content); text != "" {
		out = append(out, Part{Text: text})
	}
	for i := range msg.ToolCalls {
		tc := &msg.ToolCalls[i]
		name := tc.Function.Name
		if name == "" {
			continue
		}
		id := tc.ID
		if id == "" {
			id = ids.generate()
		}
		ids.remember(id, name)
		out = append(out, Part{FunctionCall: &FunctionCall{
			Name: name,
			Args: parseToolArgs(tc.Function.Arguments),
		}})
	}
	return out
}

// parseToolArgs 解析工具调用参数：合法 JSON 解为对象，非 JSON 或标量统一包成
// {"value": ...}，避免把裸字符串塞进 Args 导致上游 schema 校验失败。
func parseToolArgs(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return map[string]any{"value": raw}
	}
	return v
}

// toolResultToParts 把 role=tool 的结果消息转为 Gemini FunctionResponse。
// 函数名优先用 tool_call_id 反查；未命中时回退消息自带的 name 字段。
func toolResultToParts(msg OpenAIMessage, ids *toolIDMap) []Part {
	name := ids.nameOf(msg.ToolCallID)
	if name == "" {
		name = strings.TrimSpace(msg.Name)
	}
	if name == "" {
		// 既查不到 id 映射也无 name：无法构造合法 FunctionResponse，丢弃该回合
		// （Gemini 上游对无名 FunctionResponse 会返 400）。
		return nil
	}
	var resp any
	switch v := msg.Content.(type) {
	case string:
		if v == "" {
			resp = map[string]any{"result": ""}
		} else {
			// 尝试解析为结构化结果；失败则按纯文本包装，符合 Gemini 常见约定。
			var parsed any
			if err := json.Unmarshal([]byte(v), &parsed); err == nil {
				resp = parsed
			} else {
				resp = map[string]any{"result": v}
			}
		}
	default:
		resp = msg.Content
	}
	return []Part{{FunctionResponse: &FunctionResponse{Name: name, Response: resp}}}
}

// applyGenerationConfig 映射采样参数、停止序列、思考档位与结构化输出。
func applyGenerationConfig(out *GeminiRequest, in *OpenAIChatRequest, actualModel string, cfg config.ConfigProvider) error {
	gc := &GenerationConfig{}
	used := false

	if in.Temperature != nil {
		gc.Temperature = in.Temperature
		used = true
	}
	if in.TopP != nil {
		gc.TopP = in.TopP
		used = true
	}
	if mt := in.MaxCompletionTokens; mt != nil {
		gc.MaxOutputTokens = mt
		used = true
	} else if in.MaxTokens != nil {
		gc.MaxOutputTokens = in.MaxTokens
		used = true
	}

	switch v := in.Stop.(type) {
	case string:
		if v != "" {
			gc.StopSequences = []string{v}
			used = true
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				gc.StopSequences = append(gc.StopSequences, s)
			}
		}
		if len(gc.StopSequences) > 0 {
			used = true
		}
	case []string:
		for _, s := range v {
			if s != "" {
				gc.StopSequences = append(gc.StopSequences, s)
			}
		}
		if len(gc.StopSequences) > 0 {
			used = true
		}
	}

	if rf := in.ResponseFormat; rf != nil {
		switch strings.ToLower(strings.TrimSpace(rf.Type)) {
		case "json_object":
			gc.ResponseMimeType = "application/json"
			used = true
		case "json_schema":
			if rf.JSONSchema != nil && rf.JSONSchema.Schema != nil {
				gc.ResponseMimeType = "application/json"
				// Schema 交给上行清洗层：cleanFunctionParameters 会剥离上游不支持的关键字，
				// 但那是针对 functionDeclarations 的；此处直接透传原生 schema，
				// 由 BuildVariables 决定最终形态。
				gc.ResponseSchema = rf.JSONSchema.Schema
				used = true
			}
		}
	}

	if tc, err := thinkingConfigFor(in.ReasoningEffort, actualModel, cfg); err != nil {
		return err
	} else if tc != nil {
		gc.ThinkingConfig = tc
		used = true
	}

	if used {
		out.GenerationConfig = gc
	}
	return nil
}

// thinkingConfigFor 把 OpenAI 的 reasoning_effort 映射为 Gemini ThinkingConfig。
//
// 硬约束：必须过 TextSpecFor(model).AllowedLevels 白名单，否则下游
// TextStrategy.Validate（strategy_text.go:57）会直接返 400。
// 模型不支持该档位时按"最近可用档位"降级，而非报错——reasoning_effort 是建议性参数。
func thinkingConfigFor(effort, actualModel string, _ config.ConfigProvider) (*ThinkingConfig, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return nil, nil
	}

	// OpenAI 档位 → Gemini level（除 none 外需过白名单）
	if effort == "none" {
		return nil, nil // 不注入：交给策略层按默认档位处理
	}
	want := ""
	switch effort {
	case "minimal":
		want = "MINIMAL"
	case "low":
		want = "LOW"
	case "medium":
		want = "MEDIUM"
	case "high":
		want = "HIGH"
	default:
		return nil, fmt.Errorf("unsupported reasoning_effort %q", effort)
	}

	if FamilyFor(actualModel) != FamilyText {
		// 图像家族的档位白名单在 ImageSpec 里，且生图走另一套平滑降级；
		// 这里只对文本家族做注入，避免与 ImageStrategy 的默认注入打架。
		return nil, nil
	}

	spec := TextSpecFor(actualModel)
	if spec.Mechanism == ThinkingUnsupported {
		return nil, nil // 模型不支持思考：静默忽略而非报错
	}
	if spec.AllowedLevels[want] {
		return &ThinkingConfig{ThinkingLevel: want}, nil
	}
	// 降级顺序：从期望档位向更高档位找最近的可用档（保留"思考强度不低于要求"的语义）。
	for _, cand := range []string{"LOW", "MEDIUM", "HIGH"} {
		if spec.AllowedLevels[cand] {
			return &ThinkingConfig{ThinkingLevel: cand}, nil
		}
	}
	return nil, nil
}

// applyTools 映射工具声明与 tool_choice。
func applyTools(out *GeminiRequest, in *OpenAIChatRequest, _ *toolIDMap) error {
	if len(in.Tools) == 0 {
		return nil
	}
	var decls []FunctionDeclaration
	for i := range in.Tools {
		t := &in.Tools[i]
		if !strings.EqualFold(t.Type, "function") {
			continue
		}
		if t.Function.Name == "" {
			return errors.New("tool name must not be empty")
		}
		decls = append(decls, FunctionDeclaration{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	if len(decls) == 0 {
		return nil
	}
	out.Tools = []Tool{{FunctionDeclarations: decls}}

	// tool_choice：string（none/auto/required）或 object（指定函数）。
	switch v := in.ToolChoice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "none":
			out.ToolConfig = &ToolConfig{FunctionCallingConfig: &FunctionCallingConfig{Mode: "NONE"}}
		case "required":
			out.ToolConfig = &ToolConfig{FunctionCallingConfig: &FunctionCallingConfig{Mode: "ANY"}}
		case "auto", "":
			// AUTO 是上游默认值，不显式设置（避免覆盖家族策略的既有注入）。
		default:
			return fmt.Errorf("unsupported tool_choice %q", v)
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			if name == "" {
				return errors.New("tool_choice.function.name must not be empty")
			}
			out.ToolConfig = &ToolConfig{FunctionCallingConfig: &FunctionCallingConfig{
				Mode:                 "ANY",
				AllowedFunctionNames: []string{name},
			}}
		}
	}
	return nil
}
