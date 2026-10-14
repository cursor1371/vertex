package transform

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// 本文件实现 Gemini 原生响应 → OpenAI Chat Completions 响应/流帧的转换。
//
// 两条关键语义差异需在此弥合：
//  1. OpenAI 流以 "data: [DONE]" 终帧结束，Gemini 流没有终帧标记——由
//     OpenAIStreamDone 常量提供，handler 在流尾写出；
//  2. OpenAI 区分 content 与 reasoning_content，Gemini 用 Part.Thought 标记思考内容。

// OpenAIStreamDone 是 OpenAI SSE 流的终帧标记。new-api 等中继网关以此判定流结束，
// 缺失会挂等到超时——Gemini 侧无此概念，故由本层补齐。
const OpenAIStreamDone = "data: [DONE]\n\n"

// OpenAIHeartbeat 是 SSE 心跳注释帧。OpenAI 客户端忽略以 ":" 开头的行，
// 故不会被误解析为内容；其作用是穿透 Nginx/new-api 等下游的 proxy_read_timeout。
const OpenAIHeartbeat = ": ping\n\n"

// OpenAIObjectComplete / OpenAIObjectChunk 是响应 object 字段取值。
const (
	OpenAIObjectComplete = "chat.completion"
	OpenAIObjectChunk    = "chat.completion.chunk"
)

// OpenAIFinishReason 把 Gemini finishReason 映射为 OpenAI finish_reason。
//
// 未映射的值（含空串、UNSPECIFIED）返回 ""，由调用方按场景决定是 null（流式中间帧）
// 还是 stop（非流式兜底）。
func OpenAIFinishReason(fr string) string {
	switch strings.ToUpper(strings.TrimSpace(fr)) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "PROHIBITED_CONTENT", "RECITATION", "SPII", "BLOCKLIST", "IMAGE_SAFETY":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL":
		return "tool_calls"
	default:
		return ""
	}
}

// geminiTextAndThought 从候选内容中拆分正文与思考文本。
// 返回 (content, reasoning)；两者可能同时非空。
func geminiTextAndThought(c *Candidate) (content, reasoning string) {
	if c == nil || c.Content == nil {
		return "", ""
	}
	var cSB, rSB strings.Builder
	for _, p := range c.Content.Parts {
		if p.Text == "" {
			continue
		}
		if p.Thought {
			rSB.WriteString(p.Text)
		} else {
			cSB.WriteString(p.Text)
		}
	}
	return cSB.String(), rSB.String()
}

// OpenAIToolCallRegistry 维护一次请求内"函数名 → 稳定 ID / index"的映射。
type OpenAIToolCallRegistry struct {
	ids   map[string]string
	index map[string]int
	seq   int
}

// NewOpenAIToolCallRegistry 构造空的映射表，供单次请求持有、跨帧复用。
func NewOpenAIToolCallRegistry() *OpenAIToolCallRegistry {
	return &OpenAIToolCallRegistry{ids: map[string]string{}, index: map[string]int{}}
}

// Resolve 返回该函数名的稳定 ID 与 tool_calls 下标：首次出现时分配，此后复用。
//
// OpenAI 流式客户端按 index 聚合各 delta 的 arguments、按 ID 识别同一调用；
// 若每帧发新 ID + 全量 arguments，严格按规范拼接的客户端会把它解析成 N 个
// 独立调用。Gemini 侧无并行同名调用语义，按函数名稳定即可。
// 合成工具不经过此路径——它在解包层已被改写为纯文本。
func (r *OpenAIToolCallRegistry) Resolve(name string) (id string, index int) {
	if id, ok := r.ids[name]; ok {
		return id, r.index[name]
	}
	r.seq++
	id = fmt.Sprintf("call_%08x", r.seq)
	r.ids[name] = id
	r.index[name] = len(r.index)
	return id, r.index[name]
}

// geminiToolCalls 把候选中的 FunctionCall parts 转为 OpenAI tool_calls。
// resolve 提供同一逻辑调用跨帧稳定的 ID 与 index（见 OpenAIToolCallRegistry）。
func geminiToolCalls(c *Candidate, resolve func(name string) (string, int)) []OpenAIToolCall {
	if c == nil || c.Content == nil {
		return nil
	}
	var out []OpenAIToolCall
	for _, p := range c.Content.Parts {
		if p.FunctionCall == nil || p.FunctionCall.Name == "" {
			continue
		}
		args := "{}"
		if p.FunctionCall.Args != nil {
			if b, err := json.Marshal(p.FunctionCall.Args); err == nil {
				args = string(b)
			}
		}
		id, index := resolve(p.FunctionCall.Name)
		idx := index
		out = append(out, OpenAIToolCall{
			ID:    id,
			Index: &idx,
			Type:  "function",
			Function: OpenAIFunctionCall{
				Name:      p.FunctionCall.Name,
				Arguments: args,
			},
		})
	}
	return out
}

// OpenAICompleteResponse 把完整 Gemini 响应转为 OpenAI 非流式响应。
//
// usage 由调用方传入（prompt/completion 均为本地估算值或上游精确值），
// 因为 token 计数在 api 层统一折算，转换层不重复计算。
func OpenAICompleteResponse(resp *GeminiResponse, model, id string, created int64,
	promptTok, completionTok int) *OpenAIChatResponse {

	out := &OpenAIChatResponse{
		ID:      id,
		Object:  OpenAIObjectComplete,
		Created: created,
		Model:   model,
		Usage:   &OpenAIUsage{PromptTokens: promptTok, CompletionTokens: completionTok, TotalTokens: promptTok + completionTok},
	}
	if resp == nil || len(resp.Candidates) == 0 {
		// 空响应（典型是安全拦截）：补一个空 choice，让客户端能读到 finish_reason
		// 而不是解析失败。finish_reason 取 content_filter 以表达"被拦"。
		fr := "content_filter"
		out.Choices = []OpenAIChoice{{Index: 0, Message: &OpenAIMessage{Role: "assistant", Content: ""}, FinishReason: &fr}}
		return out
	}

	registry := NewOpenAIToolCallRegistry()

	for i, c := range resp.Candidates {
		if c == nil {
			continue
		}
		content, reasoning := geminiTextAndThought(c)
		msg := &OpenAIMessage{Role: "assistant"}
		if reasoning != "" {
			msg.ReasoningContent = reasoning
		}
		msg.Content = content
		if calls := geminiToolCalls(c, registry.Resolve); len(calls) > 0 {
			msg.ToolCalls = calls
			// 有工具调用时 OpenAI 规范 content 可为 null；此处保留空串更兼容各类客户端。
			if content == "" && reasoning == "" {
				msg.Content = ""
			}
		}
		// 非流式下 finish_reason 必须有值。空 finishReason（干净 EOF 却无终帧，
		// 典型是抗截断路径交付的 partial）如实映射为 length——伪造 stop 会把
		// 截断伪装成正常完成，客户端据此无法重试。非空但未映射的枚举仍是真实
		// 终帧（扫描层见到真实终帧才停），按 stop 兜底。
		fr := OpenAIFinishReason(c.FinishReason)
		if fr == "" {
			if strings.TrimSpace(c.FinishReason) == "" {
				fr = "length"
			} else {
				fr = "stop"
			}
		}
		out.Choices = append(out.Choices, OpenAIChoice{
			Index:        i,
			Message:      msg,
			FinishReason: &fr,
		})
	}
	if len(out.Choices) == 0 {
		fr := "stop"
		out.Choices = []OpenAIChoice{{Index: 0, Message: &OpenAIMessage{Role: "assistant", Content: ""}, FinishReason: &fr}}
	}
	return out
}

// OpenAIStreamChunk 把 Gemini 流帧转为 OpenAI 流式 chunk。
//
// first 标记是否为流的第一帧（首帧需携带 role: assistant，OpenAI 客户端依赖此字段
// 建立消息归属）。finish 为 nil 表示流未结束（中间帧的 finish_reason 为 null）。
// resolve 提供同一逻辑工具调用跨帧稳定的 ID 与 index（见 OpenAIToolCallRegistry）。
//
// 返回 nil 表示本帧无实质内容可交付（如纯元数据帧），调用方应跳过。
func OpenAIStreamChunk(chunk *GeminiChunk, model, id string, created int64,
	first bool, resolve func(name string) (string, int)) *OpenAIChatResponse {

	if chunk == nil {
		return nil
	}

	delta := &OpenAIDelta{}
	if first {
		delta.Role = "assistant"
	}

	var finish *string
	hasContent := false

	if len(chunk.Candidates) > 0 {
		c := chunk.Candidates[0]
		if c != nil {
			content, reasoning := geminiTextAndThought(c)
			if reasoning != "" {
				delta.ReasoningContent = reasoning
				hasContent = true
			}
			if content != "" {
				delta.Content = content
				hasContent = true
			}
			if calls := geminiToolCalls(c, resolve); len(calls) > 0 {
				delta.ToolCalls = calls
				hasContent = true
			}
			if fr := OpenAIFinishReason(c.FinishReason); fr != "" {
				finish = &fr
				hasContent = true
			}
		}
	}

	if !hasContent && !first {
		return nil
	}

	return &OpenAIChatResponse{
		ID:      id,
		Object:  OpenAIObjectChunk,
		Created: created,
		Model:   model,
		Choices: []OpenAIChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finish, // 中间帧为 nil → JSON 序列化为 null
		}},
	}
}

// OpenAIUsageChunk 构造携带 usage 的收尾帧（stream_options.include_usage 场景）。
// 该帧不带 choices 内容，仅有 usage。
func OpenAIUsageChunk(model, id string, created int64, promptTok, completionTok int) *OpenAIChatResponse {
	return &OpenAIChatResponse{
		ID:      id,
		Object:  OpenAIObjectChunk,
		Created: created,
		Model:   model,
		Choices: []OpenAIChoice{},
		Usage: &OpenAIUsage{
			PromptTokens:     promptTok,
			CompletionTokens: completionTok,
			TotalTokens:      promptTok + completionTok,
		},
	}
}

// OpenAIErrorKind 是错误语义分类，供 api 层决定 HTTP 状态码与 OpenAI type。
//
// 拆分原因：错误码映射需要同时依赖 transform 与 vertex 两个包，而 vertex 已依赖
// transform（client.go），放在 transform 会成环，故在此只产出分类，由 api 层落地。
type OpenAIErrorKind string

const (
	OpenAIErrKindInvalid    OpenAIErrorKind = "invalid"
	OpenAIErrKindNotFound   OpenAIErrorKind = "notfound"
	OpenAIErrKindPermission OpenAIErrorKind = "permission"
	OpenAIErrKindRateLimit  OpenAIErrorKind = "ratelimit"
	OpenAIErrKindServer     OpenAIErrorKind = "server"
)

// OpenAIErrorOf 按错误语义返回 (分类, 展示文案)。
// 判定顺序与 vertex.FriendlyErrorMessage 一致，保证同一错误在两种协议下口径相同。
func OpenAIErrorOf(code int, kind, message, friendly, _ string) (OpenAIErrorKind, string) {
	msg := friendly
	if message != "" {
		msg += " | Raw: " + message
	}
	switch {
	case kind == "invalid" || code == 400:
		return OpenAIErrKindInvalid, msg
	case kind == "notfound" || code == 404:
		return OpenAIErrKindNotFound, msg
	case kind == "permission" || code == 403:
		return OpenAIErrKindPermission, msg
	case kind == "ratelimit" || code == 429:
		return OpenAIErrKindRateLimit, msg
	default:
		return OpenAIErrKindServer, msg
	}
}

// openaiIDSeq 是响应 id 的原子序号后缀。
//
// 不能只用 UnixNano 做唯一源：同一纳秒内并发/连续两次生成会撞号，
// 而 OpenAI 客户端（与部分中继网关）以 id 区分请求，重复会串号。
var openaiIDSeq atomic.Uint64 //nolint:gochecknoglobals

// NewOpenAIRequestID 生成 OpenAI 风格的响应 id（chatcmpl- 前缀）。
// 复用 api 层的 reqID24 会导致 import 环（api 依赖 transform），故在此独立生成。
func NewOpenAIRequestID() string {
	return fmt.Sprintf("chatcmpl-%016x%04x", uint64(time.Now().UnixNano()), openaiIDSeq.Add(1))
}
