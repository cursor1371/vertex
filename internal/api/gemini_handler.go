package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// usagePushInterval 是流式输出 token 推送给 TUI 的最小间隔：每个 chunk 都推会让
// 流式循环频繁等待 TUI 事件循环（Program.Send 对无缓冲通道阻塞）。
const usagePushInterval = 250 * time.Millisecond

// GeminiHandler 提供 Gemini 原生 REST 入口。
// 入站 Gemini JSON 解包为强类型 *transform.GeminiRequest，经 ModelResolver 解析后按家族路由至物理隔离的 Pipeline。
type GeminiHandler struct {
	handler
}

func (g *GeminiHandler) handleModelsSubtree(w http.ResponseWriter, r *http.Request) {
	var rest string
	// isV1 标记 /v1 前缀：该前缀是文档承诺的 OpenAI base URL，单模型详情走混响；
	// 其余前缀（/v1beta 等）保持纯 Gemini 形状。
	isV1 := false
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1beta/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	case strings.HasPrefix(r.URL.Path, "/v1beta1/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1beta1/models/")
	case strings.HasPrefix(r.URL.Path, "/v1alpha/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1alpha/models/")
	case strings.HasPrefix(r.URL.Path, "/v1/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1/models/")
		isV1 = true
	default:
		g.geminiError(w, http.StatusNotFound, "not found", "NOT_FOUND")
		return
	}
	if rest == "" {
		g.geminiError(w, http.StatusNotFound, "not found", "NOT_FOUND")
		return
	}

	model := rest
	method := ""
	if idx := strings.LastIndex(rest, ":"); idx != -1 {
		model = rest[:idx]
		method = rest[idx+1:]
	}

	switch method {
	case "":
		if r.Method != http.MethodGet {
			g.geminiError(w, http.StatusMethodNotAllowed, "method not allowed", "INVALID_ARGUMENT")
			return
		}
		g.handleModelInfo(w, model, isV1)
	case "generateContent":
		g.requirePost(w, r, func() { g.handleGeminiGenerate(w, r, model) })
	case "streamGenerateContent":
		g.requirePost(w, r, func() { g.handleGeminiStreamGenerate(w, r, model) })
	case "countTokens":
		g.requirePost(w, r, func() { g.handleCountTokens(w, r, model) })
	default:
		g.geminiError(w, http.StatusNotFound, "未知方法 "+method+" (unknown method)", "NOT_FOUND")
	}
}

func (g *GeminiHandler) requirePost(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodPost {
		g.geminiError(w, http.StatusMethodNotAllowed, "method not allowed", "INVALID_ARGUMENT")
		return
	}
	fn()
}

func (g *GeminiHandler) readGeminiBody(w http.ResponseWriter, r *http.Request) (*transform.GeminiRequest, string, bool) {
	// 单趟解码为 json.RawMessage 顶层值（保留原始字节，零 map[string]any 树构建）。
	// 复用 decoder 双条件分流：SyntaxError 含 "invalid UTF-8" → 编码错误；其余 → 非法 JSON。
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		if _, ok := err.(*json.SyntaxError); ok && strings.Contains(err.Error(), "invalid UTF-8") {
			writeGeminiError(w, r.Context(), vertex.NewInvalidParamError("请求体编码错误，需为 UTF-8 (request body must be UTF-8 encoded)", "", nil))
			return nil, "", false
		}
		writeGeminiError(w, r.Context(), vertex.NewInvalidParamError("请求格式错误，JSON 解析失败 (invalid JSON)", "", nil))
		return nil, "", false
	}
	// 顶层必须是对象（等价旧 decode 到 map 的 UnmarshalTypeError 分支）。
	var indexed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &indexed); err != nil {
		writeGeminiError(w, r.Context(), vertex.NewInvalidParamError("请求格式错误，JSON 解析失败 (invalid JSON)", "", nil))
		return nil, "", false
	}
	// generateContentRequest 解包（仅当值为对象，等价旧 map 类型断言，null/标量不 unwrap）。
	payload := raw
	if reqObj, ok := indexed["generateContentRequest"]; ok && len(reqObj) > 0 && reqObj[0] == '{' {
		payload = reqObj
	}
	// 提取请求体中的用户标识（优先级链的第二级：header > body > api_key_name）。
	uid := bodyUserID(indexed)
	var req transform.GeminiRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		writeGeminiError(w, r.Context(), vertex.NewInvalidParamError("请求参数有误: "+err.Error()+" (invalid argument)", "", nil))
		return nil, "", false
	}
	return &req, uid, true
}

// bodyUserID 从请求体顶层提取自定义 userId 字段（兼容 userId / clientInfo.userId 两种形式）。
func bodyUserID(indexed map[string]json.RawMessage) string {
	if raw, ok := indexed["userId"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if raw, ok := indexed["clientInfo"]; ok && len(raw) > 0 && raw[0] == '{' {
		var ci struct {
			UserID string `json:"userId"`
		}
		if err := json.Unmarshal(raw, &ci); err == nil && strings.TrimSpace(ci.UserID) != "" {
			return strings.TrimSpace(ci.UserID)
		}
	}
	return ""
}

func (g *GeminiHandler) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, rawModel string) {
	start := reqStart(r.Context())
	resolved := transform.ResolveModel(rawModel, g.cfg)
	success := false
	statusCode := http.StatusInternalServerError
	errorReason := ""
	var promptTok, candTok, totalTok int
	// 追踪收尾需要请求体（判定强制工具调用链路）与回应形态，二者在请求体中段才可知；
	// 用外层变量承接，使顶部 defer 在执行时（早已赋值）能读到正确值。
	var req *transform.GeminiRequest
	var streamMode transform.StreamMode = transform.StreamModeAggregate
	var bodyUID string
	var ok bool
	// 上游终止原因（MAX_TOKENS / MALFORMED_FUNCTION_CALL 等），仅用于统计与展示标注，
	// 不改动交付给客户端的内容。countTokens 不涉及生成，保持空串。
	var finishReason string
	// 响应完整性根因分类（错误路径保持空串=未跟踪）。
	integrityOutcome := ""
	// 上游首字节采集器：上游侧恒为流式（executeStreamingAttempt 是唯一通路），
	// 故非流式端点同样能测出"上游首字"，与客户端侧首字（写出首个 SSE 帧）区分开。
	upstreamTiming := vertex.NewUpstreamTiming()

	// 可取消的请求上下文：管理页"强制停止"据此中断本次请求（含上游流）。
	// 取消的是派生 ctx 而非 r.Context()，所以被停止后仍能用 r.Context() 写回响应。
	requestCtx, cancel := withTotalTimeout(vertex.WithUpstreamTiming(r.Context(), upstreamTiming), g.cfg)
	defer cancel()
	reqID := vertex.RequestIDFromContext(requestCtx)
	stats.StartReq(reqID, resolved.ActualModel, "", keyNameFromContext(requestCtx), false)
	stats.SetReqCancel(reqID, cancel)
	cli.UpdateReqStreaming(reqID, false)
	// 上游首字节一到就推首字。非流式要等整包收完才写响应，若只在收尾推送，
	// 首字会一直显示 "-" 直到数据传输结束——这正是用户观察到的现象。
	upstreamTiming.SetOnWinner(func() { pushUpstreamTTFB(reqID, requestCtx) })
	defer func() {
		total := time.Since(start)
		g.recordUsageSimple(requestCtx, resolved, req, success, statusCode, errorReason,
			promptTok, candTok, totalTok, total, 0, total, false, streamMode, finishReason, integrityOutcome)
		stats.StopReq(reqID)
	}()

	// 视频家族硬闸：在读取请求体与任何上游尝试之前拒绝（见 videoUnsupportedError）。
	if ve := videoUnsupportedError(resolved); ve != nil {
		writeGeminiError(w, r.Context(), ve)
		statusCode = httpStatusFor(ve.Code)
		errorReason = ve.Message
		return
	}

	req, bodyUID, ok = g.readGeminiBody(w, r)
	if !ok {
		statusCode = http.StatusBadRequest
		errorReason = "invalid request body"
		return
	}
	requestCtx = withBodyUseID(requestCtx, bodyUID)
	// 强制工具调用上下文必须在任何上游尝试之前固化（竞速并发 + RT 补偿双发均依赖名字稳定）。
	g.prepareForcedTool(resolved, req)
	// 回显将被剥离的不支持参数（文本家族）。
	applyDroppedParamsHeader(w, resolved, req)
	// 追踪三元组在此刻才全部可知：强制工具调用上下文刚固化（决定抗截断链路）。
	// 非流式端点的回应形态恒为单次。
	g.writeReqTrace(reqID, resolved, req, ResponseSingle)

	// 输入 token 本地估算：请求体到手即可算，不必等上游（匿名端点不回 usageMetadata）。
	// 覆盖 contents + systemInstruction + tools，避免只算 contents 造成的系统性低估。
	promptTok = vertex.EstimateRequestTokens(req)
	totalTok = promptTok
	cli.UpdateReqUsage(reqID, promptTok, 0, totalTok)

	stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
		a.UseID = resolveUseID(requestCtx)
	})

	log.Printf("[Server] [GeminiGenerate] 收到请求: 模型=%s, 真模型=%s, 家族=%v", rawModel, resolved.ActualModel, resolved.Family)

	var resp *transform.GeminiResponse
	var ve *vertex.VertexError

	switch resolved.Family {
	case transform.FamilyImage:
		// 模态由 ImageStrategy 按后台配置统一注入（非流式与流式端点行为一致）
		resp, ve = g.ExecuteImageGenerate(requestCtx, resolved, req)
	case transform.FamilyAudio:
		resp, ve = g.ExecuteAudioSpeech(requestCtx, resolved, req)
	case transform.FamilyVideo:
		// 第二道闸：前置 videoUnsupportedError 已拦截，此处防御未来重构把前置检查挪走。
		ve = videoUnsupportedError(resolved)
	default:
		resp, ve = g.ExecuteTextComplete(requestCtx, resolved, req)
	}

	// 非流式端点没有"逐帧交付"的过程，其首字即上游首字节耗时；
	// 在收齐响应后立即上报给 TUI 追踪器（否则 Time 列首字恒为 "-"）。
	pushTTFB(reqID, requestCtx)

	if ve != nil {
		if isSafetyBlock(ve) {
			writeJSON(w, http.StatusOK, geminiSafetyResponse(ve))
			statusCode = http.StatusOK
			success = true
			errorReason = "safety block: " + ve.Message
			return
		}
		// 管理员强制停止优先判定：替换为可区分的明确错误（503），
		// 使状态码、错误文案与统计口径三者一致。
		ve = substituteAdminStop(reqID, ve)
		writeGeminiError(w, r.Context(), ve)
		statusCode = httpStatusFor(ve.Code)
		errorReason = ve.Message
		return
	}
	writeJSON(w, http.StatusOK, resp)
	// 上游终止原因：MAX_TOKENS（撞输出上限）/ MALFORMED_FUNCTION_CALL（工具调用畸形）
	// 等由上游明确给出。此处只做记录与标注，不动已写出的响应体。
	finishReason = CaptureFinishReason(reqID, resp)
	// 响应完整性标注：只记录不改写，供面板排查截断根因（空回复 / 工具参数非法等）。
	var integrityGuard transform.StreamIntegrityGuard
	integrityGuard.ObserveResponse(resp)
	integrityOutcome = integrityGuard.Finalize().Outcome
	// 上游若回传 usageMetadata（真实 Vertex API Key 场景），精确计数优先于本地折算；
	// 匿名端点无该字段，回退本地估算。
	if p, c, t, ok := upstreamTokens(resp.UsageMetadata); ok {
		if p > 0 {
			promptTok = p
		}
		candTok = c
		totalTok = t
	} else {
		candTok = estimateResponseTokens(resp)
		totalTok = promptTok + candTok
	}
	cli.UpdateReqUsage(reqID, promptTok, candTok, totalTok)
	stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
		a.PromptTok = promptTok
		a.CandTok = candTok
		a.TotalTok = totalTok
	})
	statusCode = http.StatusOK
	success = true
}

func (g *GeminiHandler) handleGeminiStreamGenerate(w http.ResponseWriter, r *http.Request, rawModel string) {
	start := reqStart(r.Context())
	resolved := transform.ResolveModel(rawModel, g.cfg)
	success := false
	statusCode := http.StatusInternalServerError
	errorReason := ""
	var promptTok, candTok, totalTok int
	var firstWrite time.Time
	var lastUsagePush time.Time
	// 同 handleGeminiGenerate：请求体与流模式在中段才可知，用外层变量承接供顶部 defer 使用。
	var req *transform.GeminiRequest
	var streamMode transform.StreamMode = transform.StreamModeTrue
	var bodyUID string
	// 上游终止原因（MAX_TOKENS / MALFORMED_FUNCTION_CALL 等），仅用于统计与展示标注。
	var finishReason string
	// 响应完整性状态：chunk 循环内累积，流结束后产出根因分类（错误路径保持空串）。
	var integrityGuard transform.StreamIntegrityGuard
	integrityOutcome := ""
	// 同非流式：上游侧恒为流式，故此处同样采集上游首字节。
	upstreamTiming := vertex.NewUpstreamTiming()

	// 可取消的请求上下文：管理页"强制停止"据此中断本次请求（含上游流）。
	requestCtx, cancel := withTotalTimeout(vertex.WithUpstreamTiming(r.Context(), upstreamTiming), g.cfg)
	defer cancel()
	reqID := vertex.RequestIDFromContext(requestCtx)
	stats.StartReq(reqID, resolved.ActualModel, "", keyNameFromContext(requestCtx), true)
	stats.SetReqCancel(reqID, cancel)
	cli.UpdateReqStreaming(reqID, true)
	// 上游首字节一到就推首字（聚合模式下同样受益，不必等到整包收完）。
	upstreamTiming.SetOnWinner(func() { pushUpstreamTTFB(reqID, requestCtx) })
	defer func() {
		total := time.Since(start)
		// 客户端侧首字：写出首个 SSE 帧的时刻（含竞速等待、解包、序列化等代理开销）。
		ttfb := total
		output := time.Duration(0)
		if !firstWrite.IsZero() {
			ttfb = firstWrite.Sub(start)
			if ttfb < 0 {
				ttfb = 0
			}
			output = total - ttfb
			if output < 0 {
				output = 0
			}
		}
		g.recordUsageSimple(requestCtx, resolved, req, success, statusCode, errorReason,
			promptTok, candTok, totalTok, ttfb, output, total, true, streamMode, finishReason, integrityOutcome)
		stats.StopReq(reqID)
	}()

	var ok bool
	// 视频家族硬闸：与 handleGeminiGenerate 同一条判定（见 videoUnsupportedError），
	// 在任何流模式分支与上游尝试之前拒绝，覆盖真流式与聚合流式两条路径。
	if ve := videoUnsupportedError(resolved); ve != nil {
		writeGeminiError(w, r.Context(), ve)
		statusCode = httpStatusFor(ve.Code)
		errorReason = ve.Message
		return
	}
	req, bodyUID, ok = g.readGeminiBody(w, r)
	if !ok {
		statusCode = http.StatusBadRequest
		errorReason = "invalid request body"
		return
	}
	requestCtx = withBodyUseID(requestCtx, bodyUID)
	// 强制工具调用上下文必须在任何上游尝试之前固化（竞速并发 + RT 补偿双发均依赖名字稳定）。
	g.prepareForcedTool(resolved, req)
	// 回显将被剥离的不支持参数（文本家族）。
	applyDroppedParamsHeader(w, resolved, req)

	// 输入 token 本地估算：请求体到手即可算，不必等上游。
	// 覆盖 contents + systemInstruction + tools，避免只算 contents 造成的系统性低估。
	promptTok = vertex.EstimateRequestTokens(req)
	totalTok = promptTok
	cli.UpdateReqUsage(reqID, promptTok, 0, totalTok)

	stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
		a.UseID = resolveUseID(requestCtx)
	})

	streamMode = ResolveStreamMode(resolved, g.cfg.AggregateStream())
	// 追踪三元组在流模式算出后才全部可知（回应形态取决于 streamMode）。
	g.writeReqTrace(reqID, resolved, req, ResponseModeOf(true, streamMode))

	log.Printf("[Server] [GeminiStreamGenerate] 收到请求: 模型=%s, 真模型=%s, 家族=%v, 流模式=%s", rawModel, resolved.ActualModel, resolved.Family, streamMode)

	sw := newSSEWriter(w, "text/event-stream")

	// markFirst 记录首个字节写出时刻（客户端侧 TTFB 锚点），并同步到在途请求快照。
	markFirst := func() {
		if firstWrite.IsZero() {
			firstWrite = time.Now()
			stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
				a.TTFBMs = int64(time.Since(start).Milliseconds())
			})
			// 同步给 TUI 追踪器，使其在 Time 列显示"首字/总耗时"。
			pushTTFB(reqID, requestCtx)
		}
	}

	// markUpstreamFirst 把上游首字节耗时写入在途快照与 TUI 追踪器。
	// 由 vertex 层在读到上游首个网络字节时采集（取胜出候选）；首个 chunk 到达时该值已就绪。
	//
	// 关键：只在取到非零值时才置位 upstreamMarked。若像早先那样在取值前锁存，
	// 每个 chunk 写完后调用一次即可自愈（未就绪时不锁存，等下一个 chunk）。
	// 注：聚合/非流式路径通常更早由 SetOnWinner 回调推送，此处是兜底。
	upstreamMarked := false
	markUpstreamFirst := func() {
		if upstreamMarked || upstreamTTFBMs(requestCtx) <= 0 {
			return
		}
		upstreamMarked = true
		pushUpstreamTTFB(reqID, requestCtx)
	}

	if streamMode == transform.StreamModeAggregate {
		var resp *transform.GeminiResponse
		var ve *vertex.VertexError

		switch resolved.Family {
		case transform.FamilyImage:
			resp, ve = g.ExecuteImageGenerate(requestCtx, resolved, req)
		case transform.FamilyAudio:
			resp, ve = g.ExecuteAudioSpeech(requestCtx, resolved, req)
		case transform.FamilyVideo:
			// 第二道闸：同非流式，前置 videoUnsupportedError 已拦截，此处为防御。
			ve = videoUnsupportedError(resolved)
		default:
			resp, ve = g.ExecuteTextComplete(requestCtx, resolved, req)
		}

		if ve != nil {
			if isSafetyBlock(ve) {
				_ = sw.write(geminiSSE(geminiSafetyChunk(ve)))
				statusCode = http.StatusOK
				success = true
				errorReason = "safety block: " + ve.Message
				markFirst()
				return
			}
			if !sw.hasWritten() {
				writeGeminiError(w, r.Context(), ve)
				statusCode = httpStatusFor(ve.Code)
				errorReason = ve.Message
				return
			}
			_ = sw.write(geminiSSE(vertexErrorToGemini(ve)))
			statusCode = httpStatusFor(ve.Code)
			errorReason = ve.Message
			markFirst()
			return
		}
		_ = sw.write(geminiSSE(resp))
		// 聚合模式不进 chunk 循环，故在此单独记录上游首字、终止原因与完整性。
		markUpstreamFirst()
		finishReason = CaptureFinishReason(reqID, resp)
		if resolved.Family == transform.FamilyText {
			integrityGuard.ObserveResponse(resp)
			integrityOutcome = integrityGuard.Finalize().Outcome
		}
		if p, c, t, ok := upstreamTokens(resp.UsageMetadata); ok {
			if p > 0 {
				promptTok = p
			}
			candTok = c
			totalTok = t
		} else {
			candTok = estimateResponseTokens(resp)
			totalTok = promptTok + candTok
		}
		cli.UpdateReqUsage(reqID, promptTok, candTok, totalTok)
		stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
			a.PromptTok = promptTok
			a.CandTok = candTok
			a.TotalTok = totalTok
		})
		statusCode = http.StatusOK
		success = true
		markFirst()
		return
	}

	hasFinish := false
	streamErrWritten := false
	var lastUsage *transform.UsageMetadata

	g.ExecuteTextStream(requestCtx, resolved, req, func(chunk *transform.GeminiChunk, err *vertex.VertexError) bool {
		if err != nil {
			// 管理员强制停止优先判定：给出可区分的明确错误（未输出过内容 → 503 状态码；
			// 已输出过内容 → 错误帧），避免与真实上游故障混为一谈。
			err = substituteAdminStop(reqID, err)
			if isSafetyBlock(err) {
				_ = sw.write(geminiSSE(geminiSafetyChunk(err)))
				// 安全拦截帧不经解包链路，直接登记完整性（→ content_filter）。
				safetyFR := err.Status
				if !transform.IsSafetyFinishReason(safetyFR) {
					safetyFR = "SAFETY"
				}
				integrityGuard.MarkSafety(safetyFR)
				statusCode = http.StatusOK
				success = true
				errorReason = "safety block: " + err.Message
				markFirst()
			} else if !sw.hasWritten() {
				writeGeminiError(w, r.Context(), err)
				statusCode = httpStatusFor(err.Code)
				errorReason = err.Message
			} else {
				// 已向客户端输出内容后断流：透传真实错误（含 Truncated 标记的真实原因），
				// 而非模糊 finishReason:OTHER；客户端可区分"网络中断"与"安全拦截"。
				_ = sw.write(geminiSSE(vertexErrorToGemini(err)))
				statusCode = httpStatusFor(err.Code)
				errorReason = err.Message
				markFirst()
			}
			streamErrWritten = true
			return false
		}

		if chunk == nil {
			return true
		}
		if chunk.UsageMetadata != nil {
			lastUsage = chunk.UsageMetadata
		}

		if len(chunk.Candidates) == 0 {
			// 如果没有 candidates，但包含元数据（例如 usageMetadata），补齐 Gemini 官方 SDK 规范的 candidates 骨架包
			if chunk.UsageMetadata != nil || chunk.PromptFeedback != nil || chunk.ModelVersion != "" {
				emptyCand := &transform.Candidate{
					Index: 0,
					Content: &transform.Content{
						Role:  "model",
						Parts: []transform.Part{},
					},
				}
				if hasFinish {
					emptyCand.FinishReason = "STOP"
				}
				chunk.Candidates = []*transform.Candidate{emptyCand}
			} else {
				return true
			}
		}

		for _, cand := range chunk.Candidates {
			if cand == nil {
				continue
			}
			if cand.Content == nil {
				cand.Content = &transform.Content{Role: "model", Parts: []transform.Part{}}
			} else if cand.Content.Role == "" {
				cand.Content.Role = "model"
			}
			if cand.Content.Parts == nil {
				cand.Content.Parts = []transform.Part{}
			}
		}

		if !hasFinish {
			if fr := firstCandidateFinishReason(chunk); fr != "" && fr != transform.FinishReasonUnspecified {
				hasFinish = true
				// 留存真实终止原因（MAX_TOKENS / MALFORMED_FUNCTION_CALL 等），
				// 供收尾时写入统计并给 TUI 打标注；不改动本帧已写出的内容。
				finishReason = fr
				cli.UpdateReqTruncation(reqID, transform.TruncationLabel(fr))
			}
		}
		okWrite := sw.write(geminiSSE(chunk))
		markFirst()
		// 首个 chunk 交付客户端时上游首字节早已到达，此刻记录即为最终值。
		// 幂等：重复写入同值无害，且不引入额外状态。
		markUpstreamFirst()
		// 输出 token 随 chunk 本地累加（不再依赖上游 usageMetadata）。
		// 按时间节流推送：每个 chunk 都推会让流式循环频繁等待 TUI 事件循环。
		if n := estimateChunkTokens(chunk); n > 0 {
			candTok += n
			totalTok = promptTok + candTok
			if time.Since(lastUsagePush) >= usagePushInterval {
				lastUsagePush = time.Now()
				cli.UpdateReqUsage(reqID, promptTok, candTok, totalTok)
				stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
					a.PromptTok = promptTok
					a.CandTok = candTok
					a.TotalTok = totalTok
				})
			}
		}
		// 完整性观察在写出之前：看到的即本次交付的内容形态（解包后的最终帧）。
		integrityGuard.Observe(chunk)
		return okWrite
	})

	// 流已收尾：产出完整性根因分类。异常收尾（无真实终帧）→ incomplete，
	// 与 ErrStreamIncomplete 口径一致；中途硬错误同样按已交付形态归类
	// （partial/空），错误语境由 StatusCode / ErrorReason 表达。
	integrityOutcome = integrityGuard.Finalize().Outcome

	if streamErrWritten {
		return
	}
	if !hasFinish {
		// 流结束但从未出现真实 finishReason：按匿名端点契约（每帧 FINISH_REASON_UNSPECIFIED
		// 无意义，完整流必以真实 finishReason 终帧收尾）此为异常收尾，绝非完整生成。
		// 已交付过任何帧则标记 Truncated（Committed 语义：绝不重试），否则按普通网络错误帧透传；
		// 严禁补发 FinishReason=STOP 假结束帧。
		e := vertex.NewNetworkError(vertex.ErrStreamIncomplete)
		if sw.hasWritten() {
			e = e.WithTruncated()
		}
		_ = sw.write(geminiSSE(vertexErrorToGemini(e)))
		statusCode = httpStatusFor(e.Code)
		errorReason = e.Message
		markFirst()
		return
	}
	// 上游 usageMetadata（若回传）覆盖本地估算，作为最终权威计数。
	if p, c, t, ok := upstreamTokens(lastUsage); ok {
		if p > 0 {
			promptTok = p
		}
		candTok = c
		totalTok = t
		cli.UpdateReqUsage(reqID, promptTok, candTok, totalTok)
	}
	statusCode = http.StatusOK
	success = true
}

func (g *GeminiHandler) handleCountTokens(w http.ResponseWriter, r *http.Request, rawModel string) {
	start := reqStart(r.Context())
	resolved := transform.ResolveModel(rawModel, g.cfg)
	statusCode := http.StatusInternalServerError
	errorReason := ""
	var countTok int

	// countTokens 是纯本地估算（不涉及上游与竞速），仍登记取消函数以保持"在途请求
	// 都能被管理页停止"的口径一致。
	requestCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reqID := vertex.RequestIDFromContext(requestCtx)
	stats.StartReq(reqID, resolved.ActualModel, "", keyNameFromContext(requestCtx), false)
	stats.SetReqCancel(reqID, cancel)
	cli.UpdateReqStreaming(reqID, false)
	defer func() {
		total := time.Since(start)
		// countTokens 只有 prompt 计数，没有生成 token；把估算结果记为 prompt/total token。
		// countTokens 不涉及生成与竞速链路：请求体为 nil，抗截断按未启用记录，亦无终止原因与完整性。
		g.recordUsage(requestCtx, resolved, nil, statusCode < 400, statusCode, errorReason,
			countTok, 0, countTok, total, 0, total, false, ResponseSingle, "", "")
		stats.StopReq(reqID)
	}()

	body, bodyUID, ok := g.readGeminiBody(w, r)
	if !ok {
		statusCode = http.StatusBadRequest
		errorReason = "invalid request body"
		return
	}
	requestCtx = withBodyUseID(requestCtx, bodyUID)
	stats.UpdateReq(reqID, func(a *stats.ActiveRequest) {
		a.UseID = resolveUseID(requestCtx)
	})
	cli.UpdateReqModel(vertex.RequestIDFromContext(requestCtx), resolved.ActualModel)
	log.Printf("[Server] [CountTokens] 收到请求: 模型=%s, 真模型=%s", rawModel, resolved.ActualModel)

	contents := body.Contents
	countTok = g.vc.CountTokens(requestCtx, resolved.ActualModel, contents)
	writeJSON(w, http.StatusOK, map[string]any{"totalTokens": countTok})
	statusCode = http.StatusOK
}

// handleModelInfo 服务 GET /{prefix}/models/{id} 的单模型详情。
// hybrid 仅在 /v1 前缀下为真：响应在 Gemini 形状上附加 OpenAI 规范的
// id/object/created/owned_by 字段（与 /v1/models 混响清单同构），
// 使 OpenAI 客户端直接请求单模型详情也能解析。其余前缀保持纯 Gemini。
func (g *GeminiHandler) handleModelInfo(w http.ResponseWriter, modelName string, hybrid bool) {
	name := strings.TrimPrefix(modelName, "models/")
	known := false
	for _, m := range g.cfg.ModelsWithVariants() {
		if m == name {
			known = true
			break
		}
	}
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "message": "Model '" + modelName + "' not found.", "status": "NOT_FOUND",
		}})
		return
	}
	info := geminiModelInfo(name)
	if hybrid {
		info["id"] = name
		info["object"] = "model"
		info["created"] = modelsListCreated
		info["owned_by"] = "vertex-ai-proxy"
	}
	writeJSON(w, http.StatusOK, info)
}

// geminiSSE 把任意 Gemini 帧（map 或 typed 结构）序列化为单条 SSE data 帧。
// 原先 map/typed 两个方法逐字重复，仅参数类型不同——jsonx.Marshal 本就接受任意
// 类型，统一为一个函数即可。
func geminiSSE(obj any) string {
	data, err := jsonx.Marshal(obj)
	if err != nil {
		return "data: {}\n\n"
	}
	return "data: " + string(data) + "\n\n"
}

func (g *GeminiHandler) geminiError(w http.ResponseWriter, status int, msg, geminiStatus string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": msg, "status": geminiStatus,
	}})
}

func vertexErrorToGemini(e *vertex.VertexError) map[string]any {
	msg := vertex.FriendlyErrorMessage(e)
	if e.Message != "" {
		msg += " | Raw: " + e.Message
	}
	if e.UpstreamResponse != "" {
		msg += " | Upstream: " + e.UpstreamResponse
	}
	errObj := map[string]any{
		"code": e.Code, "message": msg, "status": geminiStatusOf(e),
	}
	// 透传上游结构化 details（Gemini/google.rpc.Status 规范：details 为数组，单元素为原始对象）。
	if len(e.Details) > 0 {
		errObj["details"] = []any{e.Details}
	}
	return map[string]any{"error": errObj}
}

func geminiStatusOf(e *vertex.VertexError) string {
	if e != nil && e.Status != "" {
		return e.Status
	}
	return "INTERNAL"
}

func geminiSafetyResponse(e *vertex.VertexError) map[string]any {
	blockReason := e.Status
	if !transform.IsBlockReason(blockReason) {
		blockReason = "SAFETY"
	}
	return map[string]any{
		"candidates": []any{},
		"promptFeedback": map[string]any{
			"blockReason":        blockReason,
			"safetyRatings":      []any{},
			"blockReasonMessage": e.Message,
		},
	}
}

func geminiSafetyChunk(e *vertex.VertexError) map[string]any {
	blockReason := e.Status
	if !transform.IsBlockReason(blockReason) {
		blockReason = "SAFETY"
	}
	finishReason := e.Status
	if !transform.IsSafetyFinishReason(finishReason) {
		finishReason = "SAFETY"
	}
	return map[string]any{
		"candidates": []any{map[string]any{
			"content":       map[string]any{"parts": []any{}, "role": "model"},
			"finishReason":  finishReason,
			"safetyRatings": []any{},
			"index":         0,
		}},
		"promptFeedback": map[string]any{
			"blockReason":        blockReason,
			"safetyRatings":      []any{},
			"blockReasonMessage": e.Message,
		},
	}
}

// firstCandidateFinishReason 读取首候选 finishReason（原始未清洗）。
func firstCandidateFinishReason(chunk *transform.GeminiResponse) string {
	if chunk == nil {
		return ""
	}
	for _, c := range chunk.Candidates {
		if c != nil {
			return c.FinishReason
		}
	}
	return ""
}
