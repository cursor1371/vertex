package api

import (
	"context"
	"log"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// executePipelineChat 执行各家族统一的非流式 CompleteChat Pipeline 生命周期。
func (h *handler) executePipelineChat(ctx context.Context, resolved *transform.ResolvedModel, req *transform.GeminiRequest) (*transform.GeminiResponse, *vertex.VertexError) {
	strategy := resolved.Strategy
	strategy.Enhance(req, h.cfg)
	if err := strategy.Validate(req); err != nil {
		return nil, vertex.NewInvalidArgumentError(err.Error(), nil)
	}
	strategy.Prepare(req)

	cli.UpdateReqModel(vertex.RequestIDFromContext(ctx), resolved.ActualModel)

	// 抗截断变体或强制工具调用：改走真流式竞速引擎再聚合。
	// StreamParallel 在首个有效 chunk 上即锁定胜出，不像非流式聚合路径那样必须等到
	// finishReason==STOP，既更耐受上游半路截断，也避免胜出判定永假而耗尽发射预算。
	// 判定复用 AntiTruncateEnabled，与追踪写入点保持同一处定义。
	var resp *transform.GeminiResponse
	var err error
	if AntiTruncateEnabled(resolved, req) {
		var truncated bool
		resp, truncated, err = h.vc.StreamChatCollect(ctx, resolved.ActualModel, req, strategy)
		if truncated {
			// partial 交付：不补假 STOP，finishReason 留空即客户端可区分的截断信号。
			log.Printf("[Server] 抗截断路径交付部分内容: 模型=%s, 请求ID=%s",
				resolved.ActualModel, vertex.RequestIDFromContext(ctx))
		}
	} else {
		resp, err = h.vc.CompleteChat(ctx, resolved.ActualModel, req, strategy)
	}
	if err != nil {
		return nil, toVertexError(err)
	}
	transform.UnwrapForcedToolCall(resp, req.ForcedToolName)
	transform.CleanFinishReasonUnspecified(resp)
	return resp, nil
}

// ExecuteTextComplete 执行文本/语言模型非流式 Complete 生成。
func (h *handler) ExecuteTextComplete(ctx context.Context, resolved *transform.ResolvedModel, req *transform.GeminiRequest) (*transform.GeminiResponse, *vertex.VertexError) {
	return h.executePipelineChat(ctx, resolved, req)
}

// ExecuteTextStream 执行文本/语言模型原生真流式 Stream 生成。
func (h *handler) ExecuteTextStream(ctx context.Context, resolved *transform.ResolvedModel, req *transform.GeminiRequest, onChunk func(chunk *transform.GeminiChunk, err *vertex.VertexError) bool) {
	strategy := resolved.Strategy
	strategy.Enhance(req, h.cfg)
	if err := strategy.Validate(req); err != nil {
		onChunk(nil, vertex.NewInvalidArgumentError(err.Error(), nil))
		return
	}
	strategy.Prepare(req)

	// 强制档缓冲普通文本（正文必经 tool call，普通文本属契约外输出）；优先档立即透传以保留流式体验。
	unwrapper := transform.NewForcedToolUnwrapper(req.ForcedToolName, req.ForcedToolMode == config.ForcedModeRequired)

	cli.UpdateReqModel(vertex.RequestIDFromContext(ctx), resolved.ActualModel)
	h.vc.StreamChat(ctx, resolved.ActualModel, req, func(ch vertex.StreamChunk) bool {
		if ch.Err != nil {
			return onChunk(nil, ch.Err)
		}
		transform.CleanFinishReasonUnspecified(ch.Data)
		unwrapper.Rewrite(ch.Data)
		return onChunk(ch.Data, nil)
	}, strategy)

	// 流异常结束（始终无真实 finishReason）时兜底补发已缓冲的普通文本，避免内容丢失。
	if extra := unwrapper.Flush(); extra != nil {
		onChunk(extra, nil)
	}
}
