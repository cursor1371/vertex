package api

import (
	"log"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// prepareForcedTool 在请求进入 pipeline 前一次性固化强制工具调用上下文（工具名 + 模式）。
//
// 必须前置到 handler，而不是在 BuildVariables 内惰性生成：
//   - 竞速的每个候选并行调用 BuildVariables，惰性生成会写同一个 req 造成数据竞争；
//   - RT 补偿（withRTFirstTryCompensation）在同一次尝试内重发两次，
//     executeStreamingAttempt 因此执行两次，惰性生成会让两次发送携带不同的工具名；
//   - 回程解包器需要一个与请求绑定的确定名字。
//
// 生成失败（随机源异常 / 冲突重试耗尽）时本次不启用，绝不留空名。
func (h *handler) prepareForcedTool(resolved *transform.ResolvedModel, req *transform.GeminiRequest) {
	if resolved == nil || req == nil {
		return
	}
	// 家族闸门：图像家族本就降级为聚合流、音频家族为 unary，且两者的策略会强制丢弃工具，
	// 注入既无意义又会让非流式入口被错误改道到真流式引擎。
	if resolved.Family != transform.FamilyText {
		return
	}
	// 抗截断前缀是单次请求的显式意图：优先于全局档位（即使全局为关闭也按强制档装配），
	// 但结构性闸门（模型不支持工具 / 内置工具冲突 / 客户端 tool_choice=NONE）仍生效。
	// 未加前缀时保持既有语义：完全由全局 forced_tool_call_mode 决定。
	mode := transform.ForcedModeFor(resolved.ActualModel, req, h.cfg)
	if resolved.IsAntiTruncate {
		mode = transform.ForcedModeForAntiTruncate(resolved.ActualModel, req)
	}
	if mode == config.ForcedModeOff {
		return
	}
	name, err := transform.GenerateForcedToolName(transform.CollectToolNames(req.Tools))
	if err != nil {
		log.Printf("[Server] [ForcedTool] 生成合成工具名失败，本次不启用: %v", err)
		return
	}
	req.ForcedToolName = name
	req.ForcedToolMode = mode
}
