package api

import (
	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// ResponseMode 是客户端实际接收响应的形态。
//
// 注意区分"上游侧流式"与"客户端侧流式"：内部与 Vertex 上游的连接恒为流式
// （executeStreamingAttempt 是唯一通路，非流式的 CompleteChat 内部同样流式收包后再聚合，
// 见 complete.go:44-80），因此上游侧不存在"是否流式"这一维度，只有客户端侧才有。
type ResponseMode string

const (
	// ResponseIncrement 流式端点增量交付：客户端逐帧收到 SSE。
	ResponseIncrement ResponseMode = "增量"
	// ResponseAggregate 流式端点聚合交付：客户端收到单个 SSE 帧（假非流变体 /
	// 全局聚合开关 / 图像家族降级）。
	ResponseAggregate ResponseMode = "聚合"
	// ResponseSingle 非流式端点：客户端收到单个完整 JSON。
	ResponseSingle ResponseMode = "单次"
)

// ResolveStreamMode 复刻流式端点的流模式判定，供 handler 与写入点共用，避免两处漂移。
// aggregate 为全局"聚合流式"开关；resolved 为 nil 或 Strategy 为 nil 时保守按聚合处理。
func ResolveStreamMode(resolved *transform.ResolvedModel, aggregate bool) transform.StreamMode {
	if resolved == nil || resolved.Strategy == nil {
		return transform.StreamModeAggregate
	}
	if resolved.IsFake || aggregate {
		return transform.StreamModeAggregate
	}
	return resolved.Strategy.FamilyStreamMode()
}

// ResponseModeOf 由端点流式标记与流模式推导客户端实际接收形态。
// 非流式端点恒为单次；流式端点按 StreamMode 区分增量与聚合。
// StreamModeForbidden（音频）在流式端点无对应分支、落入真流式路径，按增量计（既有行为，非本次引入）。
func ResponseModeOf(reqStreaming bool, mode transform.StreamMode) ResponseMode {
	if !reqStreaming {
		return ResponseSingle
	}
	if mode == transform.StreamModeAggregate {
		return ResponseAggregate
	}
	return ResponseIncrement
}

// writeReqTrace 把追踪三元组同时推给 TUI 追踪器与 stats 在途请求快照。
//
// 两套状态相互独立（详见 usage_recorder.go 注释），必须各写一次；
// 共用同一组判定结果，避免两个展示面出现口径分歧。
// 端点流式标记由 stats.StartReq / cli.UpdateReqStreaming 各自记录，此处不重复写入。
func (h *handler) writeReqTrace(reqID string, resolved *transform.ResolvedModel,
	req *transform.GeminiRequest, mode ResponseMode) {
	rawModel := ""
	if resolved != nil {
		rawModel = resolved.RawModel
	}
	anti := AntiTruncateEnabled(resolved, req)
	cli.UpdateReqTrace(reqID, rawModel, string(mode), anti)
	stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
		a.RawModel = rawModel
		a.ResponseMode = string(mode)
		a.AntiTruncate = anti
	})
}

// CaptureFinishReason 从完整响应中提取上游终止原因，并把展示标注推给 TUI 追踪器，
// 返回原始 finishReason（供收尾写入统计）。
//
// 只做记录与标注：客户端本就能从 finishReason 看到同样信息，此函数不改动交付内容，
// 也不影响 success 判定（撞输出上限仍是 HTTP 成功）。
// 非流式与聚合流共用本入口；流式逐帧场景在 chunk 循环内直接取帧上的 finishReason。
func CaptureFinishReason(reqID string, resp *transform.GeminiResponse) string {
	fr := firstCandidateFinishReason(resp)
	cli.UpdateReqTruncation(reqID, transform.TruncationLabel(fr))
	return fr
}

// AntiTruncateEnabled 判定本次请求是否走 StreamChatCollect 竞速链路
// （首个有效 chunk 即锁定胜出，而非等到 finishReason==STOP 的终帧）。
//
// core_pipeline_text.go 的分支条件必须调用本函数，避免判定逻辑两处漂移。
func AntiTruncateEnabled(resolved *transform.ResolvedModel, req *transform.GeminiRequest) bool {
	if resolved == nil {
		return false
	}
	return resolved.IsAntiTruncate || (req != nil && req.ForcedToolName != "")
}
