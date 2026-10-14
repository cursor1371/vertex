package vertex

import (
	"context"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
)

// StreamChatCollect 以真流式竞速引擎执行请求，但把胜出流聚合为单个完整响应返回。
//
// 供抗截断变体的非流式入口（以及强制工具调用）使用：非流式聚合路径的胜出判定要求
// finishReason 恰为 STOP，而 StreamParallel 在首个有效 chunk 上即锁定胜出（不等终帧），
// 对上游半路截断更耐受，也不会因判定不满足而耗尽发射预算。
//
// 契约：
//   - 已收到内容即交付，truncated=true 表示流未正常收尾（此时响应仍返回，由调用方决定交付）；
//   - 绝不伪造 finishReason=STOP——partial 不等价于正常完成（见 ErrStreamIncomplete 的约定）；
//   - 仅当完全无内容时才返回错误。
//
// 注意：StreamParallel 用了 WithPreserveRaceCtxOnWin，胜出后不换点补位、预算已耗尽，
// 因此"交付 partial"不是取舍而是唯一选项。
func (c *VertexAIClient) StreamChatCollect(ctx context.Context, model string, req *transform.GeminiRequest,
	strategy transform.ModelStrategy) (*transform.GeminiResponse, bool, error) {
	var chunks []*transform.GeminiChunk
	var streamErr *VertexError

	c.StreamChat(ctx, model, req, func(ch StreamChunk) bool {
		if ch.Err != nil {
			streamErr = ch.Err
			return false
		}
		if ch.Data != nil {
			chunks = append(chunks, ch.Data)
		}
		return true
	}, strategy)

	resp, err := c.buildCompleteResponseTyped(collectChunksToParseResultTyped(chunks))
	if err != nil {
		// 无内容可交付：优先透传上游真实错误，避免把网络/协议故障伪装成空响应。
		if streamErr != nil {
			return nil, false, streamErr
		}
		return nil, false, err
	}
	// 截断有两种形态：显式错误，以及干净 EOF 但始终未出现真实 finishReason
	// （匿名端点契约要求完整流必以真实终帧收尾）。
	truncated := streamErr != nil || !chunksHaveRealFinish(chunks)
	return resp, truncated, nil
}

// chunksHaveRealFinish 判定收集到的 chunk 中是否出现过真实 finishReason。
func chunksHaveRealFinish(chunks []*transform.GeminiChunk) bool {
	for _, ch := range chunks {
		if fr := chunkFinishReasonTyped(ch); fr != "" && fr != finishReasonUnspecified {
			return true
		}
	}
	return false
}
