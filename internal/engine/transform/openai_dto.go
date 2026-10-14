// Package transform 的 OpenAI 兼容层：定义 OpenAI Chat Completions 规范的请求/响应
// 结构体，供 /v1/chat/completions 端点与 Gemini 原生结构互转使用。
//
// 字段刻意只取 OpenAI 规范中与本代理能力对得上、且上游 Gemini 能承载的子集；
// 不支持的入参（如 n>1）在转换层显式拒绝，而非静默降级。
package transform

// OpenAIChatRequest 是 POST /v1/chat/completions 的请求体。
type OpenAIChatRequest struct {
	Model               string                `json:"model"`
	Messages            []OpenAIMessage       `json:"messages"`
	Temperature         *float64              `json:"temperature,omitempty"`
	TopP                *float64              `json:"top_p,omitempty"`
	MaxTokens           *int                  `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                  `json:"max_completion_tokens,omitempty"`
	Stop                any                   `json:"stop,omitempty"` // string 或 []string
	Stream              bool                  `json:"stream,omitempty"`
	StreamOptions       *OpenAIStreamOptions  `json:"stream_options,omitempty"`
	Tools               []OpenAITool          `json:"tools,omitempty"`
	ToolChoice          any                   `json:"tool_choice,omitempty"` // string 或 object
	User                string                `json:"user,omitempty"`
	ReasoningEffort     string                `json:"reasoning_effort,omitempty"`
	ResponseFormat      *OpenAIResponseFormat `json:"response_format,omitempty"`

	// N 是一次生成的候选条数。本代理 GenerationConfig 无 candidateCount 字段，
	// 仅接受 0/1（缺省视为 1），>1 由转换层显式拒绝。
	N *int `json:"n,omitempty"`
}

// OpenAIStreamOptions 是流式的附加选项。
type OpenAIStreamOptions struct {
	// IncludeUsage 为 true 时在终帧前额外发一帧携带 usage 的 chunk。
	// 本代理 usage 恒为本地估算值，故该开关只影响"是否多带一帧"，不改变数值来源。
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// OpenAIMessage 是单条对话消息。
type OpenAIMessage struct {
	Role       string           `json:"role"`              // system / developer / user / assistant / tool
	Content    any              `json:"content,omitempty"` // string 或 []OpenAIContentPart
	Name       string           `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // role=tool 时必填

	// ReasoningContent 承载思考/推理内容（Gemini 的 Part.Thought）。
	// 非 OpenAI 官方字段，但被 Cherry Studio / new-api 等广泛采用，
	// 与流式 delta 的 reasoning_content 保持同名以便客户端统一处理。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// OpenAIContentPart 是 user 消息的多模态内容块。
type OpenAIContentPart struct {
	Type       string             `json:"type"` // text / image_url / input_audio
	Text       string             `json:"text,omitempty"`
	ImageURL   *OpenAIURLSource   `json:"image_url,omitempty"`
	InputAudio *OpenAIAudioSource `json:"input_audio,omitempty"`
}

// OpenAIURLSource 是图片/音频的 URL 来源（支持 data: URI 与 http(s)）。
type OpenAIURLSource struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// OpenAIAudioSource 是输入音频源。
type OpenAIAudioSource struct {
	Data   string `json:"data"`   // base64
	Format string `json:"format"` // wav / mp3
}

// OpenAITool 是一个工具声明。
type OpenAITool struct {
	Type     string            `json:"type"` // function
	Function OpenAIFunctionDef `json:"function"`
}

// OpenAIFunctionDef 是函数定义。
type OpenAIFunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"` // JSON Schema（保留原生 map）
	Strict      *bool  `json:"strict,omitempty"`
}

// OpenAIToolCall 是模型发起的工具调用（assistant 消息携带，或流式 delta 中累积）。
type OpenAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"` // function
	Function OpenAIFunctionCall `json:"function"`
	// Index 仅流式 delta 使用：同一 index 的多个 delta 需按序拼接 arguments。
	Index *int `json:"index,omitempty"`
}

// OpenAIFunctionCall 是工具调用的函数名与参数。
type OpenAIFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // JSON 字符串（流式下为增量片段）
}

// OpenAIResponseFormat 指定输出格式。
type OpenAIResponseFormat struct {
	Type       string            `json:"type"` // text / json_object / json_schema
	JSONSchema *OpenAIJSONSchema `json:"json_schema,omitempty"`
}

// OpenAIJSONSchema 是结构化输出 schema。
type OpenAIJSONSchema struct {
	Name   string `json:"name"`
	Strict *bool  `json:"strict,omitempty"`
	Schema any    `json:"schema"`
}

// ---- 响应侧 ----

// OpenAIChatResponse 是非流式 chat/completions 响应，也是流式单帧的载体。
type OpenAIChatResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"` // chat.completion / chat.completion.chunk
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []OpenAIChoice `json:"choices"`
	Usage             *OpenAIUsage   `json:"usage,omitempty"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
}

// OpenAIChoice 是单个候选。
type OpenAIChoice struct {
	Index        int            `json:"index"`
	Message      *OpenAIMessage `json:"message,omitempty"`       // 非流式
	Delta        *OpenAIDelta   `json:"delta,omitempty"`         // 流式
	FinishReason *string        `json:"finish_reason,omitempty"` // 流式中间帧为 null
	Logprobs     any            `json:"logprobs,omitempty"`
}

// OpenAIDelta 是流式增量载荷。
type OpenAIDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// OpenAIUsage 是 token 用量。匿名端点不回 usageMetadata，故统一为本地估算值。
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIErrorResponse 是 OpenAI 规范的错误响应。
type OpenAIErrorResponse struct {
	Error OpenAIErrorDetail `json:"error"`
}

// OpenAIErrorDetail 是错误详情。
type OpenAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// OpenAIErrorType 是 OpenAI 的错误类型枚举。
const (
	OpenAIErrTypeInvalidRequest = "invalid_request_error"
	OpenAIErrTypeRateLimit      = "rate_limit_error"
	OpenAIErrTypeServer         = "server_error"
)

// OpenAIModel 是 OpenAI 模型清单的单个条目（/v1/models 混响响应的 data 字段）。
type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // model
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// OpenAIModelList 是模型清单响应（注意：与 Gemini 的 {"models":[...]} 不同，
// OpenAI 规范是 {"object":"list","data":[...]}，两者不可混用）。
type OpenAIModelList struct {
	Object string        `json:"object"` // list
	Data   []OpenAIModel `json:"data"`
}
