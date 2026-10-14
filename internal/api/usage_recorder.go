package api

import (
	"context"
	"net/http"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// familyName 将 ModelFamily 映射为可读字符串（落库与面板展示口径）。
func familyName(f transform.ModelFamily) string {
	switch f {
	case transform.FamilyImage:
		return "image"
	case transform.FamilyAudio:
		return "audio"
	case transform.FamilyVideo:
		return config.FamilyNameVideo // "vedio"：与 family_map 值域同一内部标识
	default:
		return "text"
	}
}

// recordUsage 汇总一次请求的统计并交给 stats 收集器异步落库。
// 由各 Gemini handler 在请求收尾时调用；node 由 stats.TakeNodeInfo 取走竞速胜出节点名与 URI。
// integrity 是响应完整性根因分类（transform.StreamIntegrityGuard.Finalize 的 Outcome），
// 请求未产出任何响应时传空串（未跟踪）。
func (g *GeminiHandler) recordUsage(ctx context.Context, resolved *transform.ResolvedModel,
	req *transform.GeminiRequest, success bool, statusCode int, errorReason string,
	promptTok, candTok, totalTok int, ttfb, output, total time.Duration,
	reqStreaming bool, mode ResponseMode, finishReason, integrity string) {
	reqID := vertex.RequestIDFromContext(ctx)
	// TUI 追踪器与 stats 收集器相互独立：即使 stats 未启用也要更新追踪器里的用量显示。
	cli.UpdateReqUsage(reqID, promptTok, candTok, totalTok)
	if !stats.Enabled() {
		return
	}
	nodeName, nodeURI := stats.TakeNodeInfo(reqID)
	rec := db.UsageRecord{
		TS:             time.Now().Unix(),
		ReqID:          reqID,
		Model:          resolved.ActualModel,
		RequestedModel: resolved.RawModel,
		Family:         familyName(resolved.Family),
		APIKeyName:     keyNameFromContext(ctx),
		UseID:          resolveUseID(ctx),
		NodeName:       nodeName,
		NodeURI:        nodeURI,
		Streaming:      reqStreaming,
		ResponseMode:   string(mode),
		AntiTruncate:   AntiTruncateEnabled(resolved, req),
		// 合成工具传输只在注入真实发生时标记：AntiTruncate 布尔还包含"仅改道
		// 竞速链路、结构性闸门拦下注入"的形态。不含任何正文。
		AntiTruncateTransport: antiTruncateTransportOf(req),
		Integrity:             integrity,
		// 上游终止原因原样留存（不落库存标注文案）：上游新增枚举时无需再改表，
		// 展示层按需映射。仅用于运维观察，不影响 success 口径。
		FinishReason:     finishReason,
		Success:          success,
		StatusCode:       statusCode,
		ErrorReason:      errorReason,
		PromptTokens:     promptTok,
		CandidatesTokens: candTok,
		TotalTokens:      totalTok,
		TotalMs:          total.Milliseconds(),
		TTFBMs:           ttfb.Milliseconds(),
		// 上游首字：自请求开始至上游返回首字节。上游侧恒为流式，故三个端点都能测出。
		// 与客户端侧 TTFBMs 之差即代理自身处理开销（竞速等待/解包/序列化/写出）。
		UpstreamTTFBMs: upstreamTTFBMs(ctx),
		OutputMs:       output.Milliseconds(),
	}
	stats.Record(rec)
}

// antiTruncateTransportOf 推导合成工具传输的审计形态：注入了合成工具 →
// tool-transport；否则空串（未启用 / 仅改道竞速链路）。
func antiTruncateTransportOf(req *transform.GeminiRequest) string {
	if req == nil || req.ForcedToolName == "" {
		return ""
	}
	return "tool-transport"
}

// pushTTFB 把首字耗时同步给 TUI 追踪器。
// 首字口径统一为"上游首字节"（上游侧恒为流式，三个端点都有效），不再上报客户端
// 首帧耗时——非流式/聚合流下它与总耗时相同，并列展示只会造成冗余。
func pushTTFB(reqID string, ctx context.Context) {
	cli.UpdateReqTTFB(reqID, upstreamTTFBMs(ctx))
}

// pushUpstreamTTFB 把当前已就绪的上游首字同步给在途快照与 TUI 追踪器。
// 供"上游首字节就绪"回调即时调用，使首字在数据传输过程中就可见，而不必等整包
// 收完（非流式/聚合流）才显示。值为 0（尚未就绪，或亚毫秒被 Milliseconds 截断）
// 时直接返回，避免用 0 覆盖已写入的有效值。
func pushUpstreamTTFB(reqID string, ctx context.Context) {
	up := upstreamTTFBMs(ctx)
	if up <= 0 {
		return
	}
	stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
		a.UpstreamTTFBMs = up
	})
	pushTTFB(reqID, ctx)
}

// upstreamTTFBMs 计算自请求开始至上游首字节的毫秒数；未采集到返回 0。
// 时序由 vertex 层在读到上游首个网络字节时写入 context（取胜出候选，非全候选最早）。
func upstreamTTFBMs(ctx context.Context) int64 {
	timing := vertex.TimingFromContext(ctx)
	if timing == nil {
		return 0
	}
	start := reqStartFromContext(ctx)
	if start.IsZero() {
		return 0
	}
	return timing.UpstreamTTFB(start).Milliseconds()
}

// recordUsageSimple 是流式/非流式端点共用的收尾入口，按端点语义推导回应形态。
// 流式端点在聚合模式下恒为聚合，否则为增量；非流式端点恒为单次。
func (g *GeminiHandler) recordUsageSimple(ctx context.Context, resolved *transform.ResolvedModel,
	req *transform.GeminiRequest, success bool, statusCode int, errorReason string,
	promptTok, candTok, totalTok int, ttfb, output, total time.Duration,
	reqStreaming bool, streamMode transform.StreamMode, finishReason, integrity string) {
	g.recordUsage(ctx, resolved, req, success, statusCode, errorReason, promptTok, candTok, totalTok,
		ttfb, output, total, reqStreaming, ResponseModeOf(reqStreaming, streamMode), finishReason, integrity)
}

// withBodyUseID 将请求体提取的用户标识写入 context（仅在非空时写入）。
func withBodyUseID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, reqBodyIDKey{}, uid)
}

// estimateResponseTokens 本地估算整份响应的输出 token。
// 匿名端点不回 usageMetadata，依赖它只会长期为 0，因此统一改为本地折算。
func estimateResponseTokens(resp *transform.GeminiResponse) int {
	if resp == nil {
		return 0
	}
	total := 0
	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		total += vertex.EstimatePartsTokens(cand.Content.Parts)
	}
	return total
}

// estimateChunkTokens 本地估算单个流式 chunk 的增量输出 token。
func estimateChunkTokens(chunk *transform.GeminiChunk) int {
	if chunk == nil {
		return 0
	}
	total := 0
	for _, cand := range chunk.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		total += vertex.EstimatePartsTokens(cand.Content.Parts)
	}
	return total
}

// upstreamTokens 从上游 usageMetadata 提取 (prompt, candidates, total) 精确计数。
// 三者全为 0 时返回 ok=false，表示上游本次未提供有效计数（匿名端点常态），
// 调用方应回退本地估算。真实 API Key 走 Vertex 时上游会回 usageMetadata，
// 此时精确值优先于本地折算。
func upstreamTokens(um *transform.UsageMetadata) (prompt, cand, total int, ok bool) {
	if um == nil {
		return 0, 0, 0, false
	}
	p := um.PromptTokenCount
	c := um.CandidatesTokenCount
	t := um.TotalTokenCount
	if p == 0 && c == 0 && t == 0 {
		return 0, 0, 0, false
	}
	if t == 0 {
		t = p + c
	}
	return p, c, t, true
}

// reqStart 返回请求开始时刻；context 未注入时退回当前时刻。
func reqStart(ctx context.Context) time.Time {
	if s := reqStartFromContext(ctx); !s.IsZero() {
		return s
	}
	return time.Now()
}

// httpStatusFor 返回给定错误响应的 HTTP 状态码；0 视为 500。
func httpStatusFor(code int) int {
	if code == 0 {
		return http.StatusInternalServerError
	}
	return code
}
