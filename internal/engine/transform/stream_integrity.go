package transform

import (
	"encoding/json"
)

// 本文件实现流式/聚合生成的响应完整性标注（对齐 vertex-streaming-anti-truncation
// 的 completion-integrity 思路）。
//
// 定位：只做记录与标注，不改写响应内容、不中断交付、不参与 Success 判定——
// 已交付的 partial 永远优先送达客户端（ErrStreamIncomplete 契约），完整性状态
// 供管理面板与运维排查"截断到底发生在哪一层"。
//
// 与 ErrStreamIncomplete 的分工：后者是"流未正常收尾"的传输层信号（已发内容
// 照发），本文件在其上细化根因分类——空回复（如只有思维链却声称完成）、
// 工具参数 JSON 非法、正常完成等。

// StreamIntegrity 是一次生成的完整性状态快照。字段只含固定枚举与布尔，
// 不含任何回复正文，可安全写入日志与统计。
type StreamIntegrity struct {
	// Outcome 是根因分类：complete / length / content_filter / tool_calls /
	// empty / incomplete。空串表示未跟踪（请求未产出任何响应）。
	Outcome string
	// FinishReason 是观察到的上游终止原因原样值（可能为空）。
	FinishReason string
	// StreamDone 表示是否出现过真实终帧（非空且非 UNSPECIFIED 的 finishReason）。
	StreamDone bool
	// HasContent / HasToolCalls 标记是否出现过可见正文 / 真实工具调用
	// （思维链不算正文）。
	HasContent   bool
	HasToolCalls bool
}

// 完整性枚举值（对应 Outcome）。
const (
	IntegrityComplete   = "complete"
	IntegrityLength     = "length"
	IntegrityFilter     = "content_filter"
	IntegrityToolCalls  = "tool_calls"
	IntegrityEmpty      = "empty"
	IntegrityIncomplete = "incomplete"
)

// StreamIntegrityGuard 在 chunk 循环中累积完整性状态。
// 零值即可用；非并发安全——只在请求自身的回调链路上使用。
type StreamIntegrityGuard struct {
	finish      string
	hasFinish   bool
	hasContent  bool
	hasToolCall bool
	badToolArgs bool
}

// Observe 观察一帧（解包后的最终形态，即即将交付客户端的内容）。
func (g *StreamIntegrityGuard) Observe(chunk *GeminiChunk) {
	if chunk == nil {
		return
	}
	for _, cand := range chunk.Candidates {
		if cand == nil {
			continue
		}
		if isTerminalFinish(cand.FinishReason) {
			g.hasFinish = true
			g.finish = cand.FinishReason
		}
		if cand.Content == nil {
			continue
		}
		for _, p := range cand.Content.Parts {
			switch {
			case p.Text != "" && !p.Thought:
				g.hasContent = true
			case p.FunctionCall != nil && p.FunctionCall.Name != "":
				g.hasToolCall = true
				if !toolArgsJSONValid(p.FunctionCall.Args) {
					g.badToolArgs = true
				}
			}
		}
	}
}

// ObserveResponse 观察一份完整响应（非流式 / 聚合路径）。
func (g *StreamIntegrityGuard) ObserveResponse(resp *GeminiResponse) {
	if resp == nil {
		return
	}
	g.Observe(&GeminiChunk{Candidates: resp.Candidates})
}

// MarkSafety 标记一次安全拦截交付。拦截帧由 api 层直接构造（不经解包链路），
// 无法走 Observe，故单独登记；finishReason 须为安全类枚举或 SAFETY。
func (g *StreamIntegrityGuard) MarkSafety(finishReason string) {
	g.hasFinish = true
	g.finish = finishReason
}

// Finalize 产出最终完整性状态。
//
// 判定顺序：按 finishReason 与内容形态分类。始终无终帧（干净 EOF 无
// finishReason）→ incomplete，与 ErrStreamIncomplete 的口径一致。
// 硬错误（未产出任何响应）不入本表——那类请求由 StatusCode / ErrorReason
// 表达，完整性列保持空串（未跟踪）。
func (g *StreamIntegrityGuard) Finalize() StreamIntegrity {
	out := StreamIntegrity{
		FinishReason: g.finish,
		StreamDone:   g.hasFinish,
		HasContent:   g.hasContent,
		HasToolCalls: g.hasToolCall,
	}
	switch {
	case !g.hasFinish:
		out.Outcome = IntegrityIncomplete
	case g.badToolArgs:
		// 工具参数 JSON 非法：客户端无法消费，按未完成归类（不中断交付）。
		out.Outcome = IntegrityIncomplete
	case g.finish == FinishReasonMaxTokens:
		out.Outcome = IntegrityLength
	case isSafetyFinish(g.finish):
		out.Outcome = IntegrityFilter
	case g.finish == "MALFORMED_FUNCTION_CALL" || g.hasToolCall:
		out.Outcome = IntegrityToolCalls
	case !g.hasContent && !g.hasToolCall:
		// 有终帧却无任何可见内容（含"只有思维链却声称完成"）：空回复。
		out.Outcome = IntegrityEmpty
	default:
		out.Outcome = IntegrityComplete
	}
	return out
}

// isSafetyFinish 判定终止原因是否属于安全拦截类（复用 IsSafetyFinishReason 的
// 白名单，保证与 OpenAI finish_reason 映射口径一致）。
func isSafetyFinish(fr string) bool {
	return IsSafetyFinishReason(fr)
}

// toolArgsJSONValid 校验工具调用参数是否为合法 JSON 对象标量形态。
// args 已是 map（上游结构化下发）视为合法；字符串形态必须能独立解析为 JSON。
func toolArgsJSONValid(args any) bool {
	switch a := args.(type) {
	case nil:
		return true
	case map[string]any:
		return true
	case string:
		var v any
		return json.Unmarshal([]byte(a), &v) == nil
	default:
		return true
	}
}
