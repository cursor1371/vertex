package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// OpenAIHandler 提供 OpenAI Chat Completions 兼容入口（/v1/chat/completions）。
//
// 只做协议转换 + 复用既有 Pipeline，不改变上游链路、竞速语义与统计口径。
// 模型清单不在此实现：/v1/models 由 server.go 的混响 handler 同时服务两种协议。
//
// 流式的两个关键补齐（Gemini 侧没有的概念）：
//  1. 流尾写出 "data: [DONE]"（transform.OpenAIStreamDone）——new-api 等中继网关
//     依赖此标记判定流结束，缺失会挂等到超时；
//  2. 可选心跳帧（transform.OpenAIHeartbeat，受 openai_sse_heartbeat_seconds 控制），
//     防止长思考期间下游网关先按 proxy_read_timeout 断流。
type OpenAIHandler struct {
	handler
}

// handleChatCompletions 是 /v1/chat/completions 的唯一入口：
// 解析 OpenAI 请求 → 转 Gemini → 走既有 Pipeline → 转回 OpenAI 交付。
func (o *OpenAIHandler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		o.openaiError(w, http.StatusMethodNotAllowed, "method not allowed", transform.OpenAIErrTypeInvalidRequest, "")
		return
	}

	start := reqStart(r.Context())
	reqID := vertex.RequestIDFromContext(r.Context())
	created := start.Unix()
	if created == 0 {
		created = time.Now().Unix()
	}
	respID := transform.NewOpenAIRequestID()

	var in transform.OpenAIChatRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		o.openaiError(w, http.StatusBadRequest, "请求格式错误，JSON 解析失败 (invalid JSON)", transform.OpenAIErrTypeInvalidRequest, "")
		o.recordSimple(r.Context(), nil, nil, false, http.StatusBadRequest, "invalid JSON", 0, 0, 0, time.Since(start), false, transform.StreamModeAggregate, "")
		return
	}
	if strings.TrimSpace(in.Model) == "" {
		o.openaiError(w, http.StatusBadRequest, "缺少 model 字段 (missing model)", transform.OpenAIErrTypeInvalidRequest, "model")
		return
	}

	resolved := transform.ResolveModel(in.Model, o.cfg)
	// 视频家族（vedio）先行拒绝：上游是 long-running 协议、后端能力未实现，
	// 给出与"非文本模型"区分开的明确原因（见 videoUnsupportedError）。
	if resolved.Family == transform.FamilyVideo {
		o.openaiError(w, http.StatusBadRequest,
			"模型 "+resolved.ActualModel+" 属视频家族(vedio)，后端视频生成能力尚未实现 (video generation is not implemented)",
			transform.OpenAIErrTypeInvalidRequest, "model")
		return
	}
	// OpenAI 兼容层只覆盖文本家族：生图/TTS 走各自的 OpenAI 端点才有意义，
	// 用 chat/completions 承载多模态产物会破坏 OpenAI 客户端的解析预期。
	if resolved.Family != transform.FamilyText {
		o.openaiError(w, http.StatusBadRequest,
			"模型 "+resolved.ActualModel+" 为非文本模型，请使用对应的 OpenAI 专用端点 (non-text model)",
			transform.OpenAIErrTypeInvalidRequest, "model")
		return
	}

	gemReq, _, err := transform.OpenAIChatToGemini(&in, resolved.ActualModel, o.cfg)
	if err != nil {
		status := http.StatusBadRequest
		o.openaiError(w, status, err.Error(), transform.OpenAIErrTypeInvalidRequest, "")
		o.recordSimple(r.Context(), resolved, nil, false, status, err.Error(), 0, 0, 0, time.Since(start), false, transform.StreamModeAggregate, "")
		return
	}

	promptTok := vertex.EstimateRequestTokens(gemReq)
	o.logOpenAIRequest("ChatCompletions", in.Model, resolved.ActualModel)
	if in.Stream {
		o.streamGenerate(w, r, resolved, gemReq, &in, reqID, respID, created, start, promptTok)
		return
	}
	o.completeGenerate(w, r, resolved, gemReq, in.Model, reqID, respID, created, start, promptTok)
}

// completeGenerate 走非流式链路：上游收完再聚合，最后一次性写出 OpenAI 响应。
func (o *OpenAIHandler) completeGenerate(w http.ResponseWriter, r *http.Request,
	resolved *transform.ResolvedModel, gemReq *transform.GeminiRequest,
	modelName, reqID, respID string, created int64, start time.Time, promptTok int) {

	statusCode := http.StatusInternalServerError
	errorReason := ""
	success := false
	finishReason := ""
	candTok := 0
	totalTok := promptTok
	streamMode := transform.StreamModeAggregate
	// 响应完整性根因分类（错误路径保持空串=未跟踪）。
	integrityOutcome := ""

	requestCtx, cancel := withTotalTimeout(vertex.WithUpstreamTiming(r.Context(), vertex.NewUpstreamTiming()), o.cfg)
	defer cancel()
	stats.StartReq(reqID, resolved.ActualModel, "", keyNameFromContext(requestCtx), false)
	stats.SetReqCancel(reqID, cancel)
	cli.UpdateReqStreaming(reqID, false)
	defer func() {
		total := time.Since(start)
		g := &GeminiHandler{handler: o.handler}
		g.recordUsageSimple(requestCtx, resolved, gemReq, success, statusCode, errorReason,
			promptTok, candTok, totalTok, total, 0, total, false, streamMode, finishReason, integrityOutcome)
		stats.StopReq(reqID)
	}()

	// 强制工具调用上下文必须在任何上游尝试之前固化（与 Gemini 端点同一约束）。
	o.prepareForcedTool(resolved, gemReq)
	// 回显将被剥离的不支持参数（OpenAI 端点恒为文本家族）。
	applyDroppedParamsHeader(w, resolved, gemReq)
	// 追踪三元组（含抗截断标记）在此刻才全部可知；此前 OpenAI 端点漏写，
	// 面板/TUI 的 AntiTruncate 列对 OpenAI 请求恒为否。
	o.writeReqTrace(reqID, resolved, gemReq, ResponseSingle)

	resp, ve := o.ExecuteTextComplete(requestCtx, resolved, gemReq)
	if ve != nil {
		ve = substituteAdminStop(reqID, ve)
		statusCode, _ = o.writeVertexError(w, r.Context(), ve)
		errorReason = ve.Message
		return
	}

	finishReason = CaptureFinishReason(reqID, resp)
	// 响应完整性标注：只记录不改写。
	var integrityGuard transform.StreamIntegrityGuard
	integrityGuard.ObserveResponse(resp)
	integrityOutcome = integrityGuard.Finalize().Outcome
	// 上游若回传 usageMetadata 则优先精确值，否则回落本地估算（与 Gemini 端点同一口径）。
	if p, c, _, ok := upstreamTokens(resp.UsageMetadata); ok {
		if p > 0 {
			promptTok = p
		}
		candTok = c
	} else {
		candTok = estimateResponseTokens(resp)
	}
	totalTok = promptTok + candTok

	out := transform.OpenAICompleteResponse(resp, modelName, respID, created, promptTok, candTok)
	writeJSON(w, http.StatusOK, out)
	statusCode = http.StatusOK
	success = true
}

// streamGenerate 走真流式链路：逐帧转 OpenAI chunk，流尾补 [DONE]，可选心跳。
func (o *OpenAIHandler) streamGenerate(w http.ResponseWriter, r *http.Request,
	resolved *transform.ResolvedModel, gemReq *transform.GeminiRequest,
	in *transform.OpenAIChatRequest, reqID, respID string, created int64,
	start time.Time, promptTok int) {

	statusCode := http.StatusInternalServerError
	errorReason := ""
	success := false
	finishReason := ""
	candTok := 0
	totalTok := promptTok
	streamMode := transform.StreamModeTrue
	var firstWrite time.Time
	// 响应完整性状态：chunk 循环内累积，流结束后产出根因分类。
	var integrityGuard transform.StreamIntegrityGuard
	integrityOutcome := ""

	requestCtx, cancel := withTotalTimeout(vertex.WithUpstreamTiming(r.Context(), vertex.NewUpstreamTiming()), o.cfg)
	defer cancel()
	stats.StartReq(reqID, resolved.ActualModel, "", keyNameFromContext(requestCtx), true)
	stats.SetReqCancel(reqID, cancel)
	cli.UpdateReqStreaming(reqID, true)
	defer func() {
		total := time.Since(start)
		ttfb := total
		if !firstWrite.IsZero() {
			ttfb = firstWrite.Sub(start)
			if ttfb < 0 {
				ttfb = 0
			}
		}
		output := total - ttfb
		if output < 0 {
			output = 0
		}
		g := &GeminiHandler{handler: o.handler}
		g.recordUsageSimple(requestCtx, resolved, gemReq, success, statusCode, errorReason,
			promptTok, candTok, totalTok, ttfb, output, total, true, streamMode, finishReason, integrityOutcome)
		stats.StopReq(reqID)
	}()

	o.prepareForcedTool(resolved, gemReq)
	// 回显将被剥离的不支持参数（OpenAI 端点恒为文本家族）。
	applyDroppedParamsHeader(w, resolved, gemReq)

	// 聚合模式（假非流 / 全局聚合开关）：上游收完再单帧交付，仍需补 [DONE]。
	streamMode = ResolveStreamMode(resolved, o.cfg.AggregateStream())
	// 追踪三元组在流模式算出后才全部可知（回应形态取决于 streamMode）。
	// 此前 OpenAI 端点漏写，面板/TUI 的 AntiTruncate 列对 OpenAI 请求恒为否。
	o.writeReqTrace(reqID, resolved, gemReq, ResponseModeOf(true, streamMode))

	// 带心跳的 SSE 写出器：心跳与 chunk 并发写，内部加锁避免帧撕裂。
	sw := newOpenAISSEWriter(w)
	hbSeconds := o.cfg.OpenAISSEHeartbeatSeconds()
	if hbSeconds > 0 {
		sw.startHeartbeat(time.Duration(hbSeconds) * time.Second)
	}
	defer sw.stopHeartbeat()

	modelName := in.Model
	// 同一逻辑工具调用跨帧稳定 ID/index：OpenAI 客户端按 index 聚合 arguments、
	// 按 ID 识别同一调用，每帧新 ID 会被解析成 N 个独立调用。
	toolCallIDs := transform.NewOpenAIToolCallRegistry()

	if streamMode == transform.StreamModeAggregate {
		resp, ve := o.ExecuteTextComplete(requestCtx, resolved, gemReq)
		if ve != nil {
			ve = substituteAdminStop(reqID, ve)
			// 安全拦截：按 OpenAI 语义以 200 + content_filter 交付，而非报错。
			if isSafetyBlock(ve) {
				out := transform.OpenAICompleteResponse(nil, modelName, respID, created, promptTok, 0)
				if !sw.writeJSON(out) {
					return
				}
				sw.writeDone()
				statusCode, success, errorReason = http.StatusOK, true, "safety block: "+ve.Message
				return
			}
			statusCode, _ = o.writeVertexError(w, r.Context(), ve)
			errorReason = ve.Message
			return
		}
		finishReason = CaptureFinishReason(reqID, resp)
		integrityGuard.ObserveResponse(resp)
		integrityOutcome = integrityGuard.Finalize().Outcome
		if p, c, _, ok := upstreamTokens(resp.UsageMetadata); ok {
			if p > 0 {
				promptTok = p
			}
			candTok = c
		} else {
			candTok = estimateResponseTokens(resp)
		}
		totalTok = promptTok + candTok
		out := transform.OpenAICompleteResponse(resp, modelName, respID, created, promptTok, candTok)
		if !sw.writeJSON(out) {
			return
		}
		sw.writeDone()
		if firstWrite.IsZero() {
			firstWrite = time.Now()
		}
		statusCode, success = http.StatusOK, true
		return
	}

	hasFinish := false
	streamErrWritten := false
	var lastUsage *transform.UsageMetadata
	first := true

	o.ExecuteTextStream(requestCtx, resolved, gemReq, func(chunk *transform.GeminiChunk, err *vertex.VertexError) bool {
		if err != nil {
			err = substituteAdminStop(reqID, err)
			if isSafetyBlock(err) {
				// 已向客户端输出内容：以 content_filter 收尾，保持 200 语义。
				out := transform.OpenAIStreamChunk(&transform.GeminiChunk{
					Candidates: []*transform.Candidate{{Index: 0, FinishReason: "SAFETY"}},
				}, modelName, respID, created, first, toolCallIDs.Resolve)
				_ = sw.writeJSON(out)
				sw.writeDone()
				statusCode, success = http.StatusOK, true
				errorReason = "safety block: " + err.Message
				if firstWrite.IsZero() {
					firstWrite = time.Now()
				}
				return false
			}
			if !sw.hasWritten() {
				// 首帧前出错：仍可返回标准 JSON 错误（HTTP 状态尚未提交）。
				statusCode, _ = o.writeVertexError(w, r.Context(), err)
				errorReason = err.Message
				streamErrWritten = true
				return false
			}
			// 已输出内容后断流：HTTP 200 已提交，只能把错误塞进流末尾再收尾。
			statusCode = httpStatusFor(err.Code)
			errorReason = err.Message
			sw.writeDone()
			streamErrWritten = true
			return false
		}
		if chunk == nil {
			return true
		}
		if chunk.UsageMetadata != nil {
			lastUsage = chunk.UsageMetadata
		}
		if !hasFinish {
			if fr := firstCandidateFinishReason(chunk); fr != "" && fr != transform.FinishReasonUnspecified {
				hasFinish = true
				finishReason = fr
				cli.UpdateReqTruncation(reqID, transform.TruncationLabel(fr))
			}
		}

		out := transform.OpenAIStreamChunk(chunk, modelName, respID, created, first, toolCallIDs.Resolve)
		if out == nil {
			return true // 无实质内容帧（纯元数据），跳过
		}
		if !sw.writeJSON(out) {
			return false
		}
		first = false
		if firstWrite.IsZero() {
			firstWrite = time.Now()
		}
		if n := estimateChunkTokens(chunk); n > 0 {
			candTok += n
			totalTok = promptTok + candTok
		}
		// 完整性观察在写出之后状态等价：chunk 已是解包后的最终形态。
		integrityGuard.Observe(chunk)
		return true
	})

	// 流已收尾：产出完整性根因分类（与 Gemini 端点同口径）。
	integrityOutcome = integrityGuard.Finalize().Outcome

	if streamErrWritten {
		return
	}
	if !hasFinish {
		// 流未以真实 finishReason 收尾（上游截断/异常 EOF）：如实以 length 收尾，
		// 严禁伪造 stop——客户端据此可区分"被截断"与"正常结束"。
		out := transform.OpenAIStreamChunk(&transform.GeminiChunk{
			Candidates: []*transform.Candidate{{Index: 0, FinishReason: transform.FinishReasonMaxTokens}},
		}, modelName, respID, created, first, toolCallIDs.Resolve)
		_ = sw.writeJSON(out)
		statusCode = http.StatusOK
		errorReason = vertex.ErrStreamIncomplete.Error()
		if firstWrite.IsZero() {
			firstWrite = time.Now()
		}
		// include_usage 在异常收尾同样补 usage 帧：截断场景恰恰最需要 token 计数
		// （计费对账 / 客户端估算上下文余量），缺失会让客户端只能按 0 处理。
		if in.StreamOptions != nil && in.StreamOptions.IncludeUsage {
			_ = sw.writeJSON(transform.OpenAIUsageChunk(modelName, respID, created, promptTok, candTok))
		}
		sw.writeDone()
		return
	}

	if p, c, t, ok := upstreamTokens(lastUsage); ok {
		if p > 0 {
			promptTok = p
		}
		candTok = c
		totalTok = t
		if totalTok == 0 {
			totalTok = promptTok + candTok
		}
	}
	// stream_options.include_usage：终帧前补一帧纯 usage（OpenAI 规范）。
	if in.StreamOptions != nil && in.StreamOptions.IncludeUsage {
		_ = sw.writeJSON(transform.OpenAIUsageChunk(modelName, respID, created, promptTok, candTok))
	}
	sw.writeDone()
	statusCode, success = http.StatusOK, true
}

// writeVertexError 把 VertexError 渲染为 OpenAI 规范错误响应，返回所用 HTTP 状态码。
// 客户端已断开（ctx 取消）时不写入，返回预期状态码供统计使用。
func (o *OpenAIHandler) writeVertexError(w http.ResponseWriter, clientCtx context.Context, ve *vertex.VertexError) (int, bool) {
	if ve == nil {
		ve = vertex.NewInternalError("internal server error", nil)
	}
	status := httpStatusFor(ve.Code)
	kind, msg := transform.OpenAIErrorOf(ve.Code, ve.Kind, ve.Message, vertex.FriendlyErrorMessage(ve), ve.Param)

	// 429 透传 Retry-After（与 writeGeminiError 同口径）。
	if ve.Code == http.StatusTooManyRequests && ve.RetryAfter > 0 {
		w.Header().Set("Retry-After", itoaSeq(ve.RetryAfter))
	}
	if clientCtx != nil && clientCtx.Err() != nil {
		return status, false
	}

	var errType, code string
	var errStatus int
	switch kind {
	case transform.OpenAIErrKindInvalid:
		errType, code, errStatus = transform.OpenAIErrTypeInvalidRequest, "invalid_request_error", http.StatusBadRequest
	case transform.OpenAIErrKindNotFound:
		errType, code, errStatus = transform.OpenAIErrTypeInvalidRequest, "model_not_found", http.StatusNotFound
	case transform.OpenAIErrKindPermission:
		errType, code, errStatus = transform.OpenAIErrTypeInvalidRequest, "permission_denied", http.StatusForbidden
	case transform.OpenAIErrKindRateLimit:
		errType, code, errStatus = transform.OpenAIErrTypeRateLimit, "rate_limit_exceeded", http.StatusTooManyRequests
	default:
		errType, code, errStatus = transform.OpenAIErrTypeServer, "server_error", status
		if errStatus == 0 || errStatus < 400 {
			errStatus = http.StatusInternalServerError
		}
	}
	writeJSON(w, errStatus, transform.OpenAIErrorResponse{Error: transform.OpenAIErrorDetail{
		Message: msg,
		Type:    errType,
		Code:    code,
		Param:   ve.Param,
	}})
	return errStatus, true
}

// openaiError 直接写出 OpenAI 规范错误（用于入参校验等不涉及上游的场景）。
func (o *OpenAIHandler) openaiError(w http.ResponseWriter, status int, msg, errType, param string) {
	if errType == "" {
		errType = transform.OpenAIErrTypeInvalidRequest
	}
	writeJSON(w, status, transform.OpenAIErrorResponse{Error: transform.OpenAIErrorDetail{
		Message: msg,
		Type:    errType,
		Param:   param,
	}})
}

// ---- SSE 写出器（含心跳）----

// openaiSSEWriter 是 /openai/v1 专用的 SSE 写出器。
//
// 与 Gemini 端点共用了底层 http.ResponseWriter，但独立实现写出逻辑：
// ① 帧结尾统一补 [DONE]；② 支持心跳 goroutine 并发写入（加锁防撕裂）。
// 刻意不复用 sseWriter：后者无锁，且其 hasWritten/flush 语义与心跳需求不匹配。
type openaiSSEWriter struct {
	w           http.ResponseWriter
	flush       func()
	mu          sync.Mutex // 保护 write：心跳 goroutine 与 chunk 循环并发调用
	wroteHeader bool
	clientGone  bool

	hbStop chan struct{}
	hbOnce sync.Once
}

func newOpenAISSEWriter(w http.ResponseWriter) *openaiSSEWriter {
	flusher, _ := w.(http.Flusher)
	sw := &openaiSSEWriter{w: w}
	if flusher != nil {
		sw.flush = flusher.Flush
	}
	return sw
}

func (sw *openaiSSEWriter) ensureHeader() {
	if sw.wroteHeader {
		return
	}
	sw.w.Header().Set("Content-Type", "text/event-stream")
	sw.w.Header().Set("Cache-Control", "no-cache")
	sw.w.Header().Set("Connection", "keep-alive")
	sw.w.Header().Set("Transfer-Encoding", "chunked")
	sw.w.Header().Set("X-Accel-Buffering", "no")
	sw.w.WriteHeader(http.StatusOK)
	sw.wroteHeader = true
}

func (sw *openaiSSEWriter) hasWritten() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.wroteHeader
}

// writeRaw 写一条原始字符串并 flush。返回 false 表示客户端断开。
func (sw *openaiSSEWriter) writeRaw(line string) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.clientGone {
		return false
	}
	sw.ensureHeader()
	if _, err := sw.w.Write([]byte(line)); err != nil {
		sw.clientGone = true
		return false
	}
	if sw.flush != nil {
		sw.flush()
	}
	return true
}

// writeJSON 把对象序列化为单条 SSE data 帧写出。
func (sw *openaiSSEWriter) writeJSON(obj any) bool {
	data, err := jsonx.Marshal(obj)
	if err != nil {
		return sw.writeRaw("data: {}\n\n")
	}
	return sw.writeRaw("data: " + string(data) + "\n\n")
}

// writeDone 写出 OpenAI 流终帧。幂等：重复调用只会多发一帧 [DONE]，
// 各调用点已通过 streamErrWritten / return 保证至多一次。
func (sw *openaiSSEWriter) writeDone() {
	sw.writeRaw(transform.OpenAIStreamDone)
}

// startHeartbeat 启动心跳 goroutine，每 interval 写一帧 SSE 注释。
// 仅在 openai_sse_heartbeat_seconds > 0 时调用。
func (sw *openaiSSEWriter) startHeartbeat(interval time.Duration) {
	if interval <= 0 {
		return
	}
	sw.hbStop = make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// 客户端断开时 writeRaw 返回 false，立即停表不空转。
				if !sw.writeRaw(transform.OpenAIHeartbeat) {
					return
				}
			case <-sw.hbStop:
				return
			}
		}
	}()
}

// stopHeartbeat 停止心跳 goroutine（幂等，供 defer 调用）。
func (sw *openaiSSEWriter) stopHeartbeat() {
	if sw.hbStop == nil {
		return
	}
	sw.hbOnce.Do(func() { close(sw.hbStop) })
}

// itoaSeq 是轻量整数转字符串（避免为序号生成引入 strconv 依赖到热路径）。
func itoaSeq(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// recordSimple 是入参校验失败等"未进入 Pipeline"场景的统计兜底。
// 复用 GeminiHandler 的统计入口，保持两张面板口径一致。
// 未产出响应，integrity 恒为空串（未跟踪）。
func (o *OpenAIHandler) recordSimple(ctx context.Context, resolved *transform.ResolvedModel,
	req *transform.GeminiRequest, success bool, statusCode int, errorReason string,
	promptTok, candTok, totalTok int, total time.Duration, reqStreaming bool,
	mode transform.StreamMode, finishReason string) {
	if resolved == nil {
		return
	}
	g := &GeminiHandler{handler: o.handler}
	g.recordUsageSimple(ctx, resolved, req, success, statusCode, errorReason,
		promptTok, candTok, totalTok, total, 0, total, reqStreaming, mode, finishReason, "")
}

// logOpenAIRequest 记录 OpenAI 端点入站请求（与 Gemini 端点的日志口径对齐）。
func (o *OpenAIHandler) logOpenAIRequest(action, model, actual string) {
	log.Printf("[Server] [OpenAI] %s: 模型=%s, 真模型=%s (OpenAI)", action, model, actual)
}
