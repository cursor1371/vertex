package api

import (
	"context"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/cli"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

type middleware struct {
	cfg  config.ConfigProvider
	keys *APIKeyManager

	// sem 是全局并发闸门信号量；cfg.MaxConcurrent()<=0 时保持 nil（不限制）。
	// 懒初始化并在配置热重载时重建（仅当并发上限变化）。
	mu      sync.Mutex
	sem     chan struct{}
	semSize int
}

// withConcurrencyLimit 是入站全局并发闸门：超过 max_concurrent 的并发请求
// 等待短缓冲（2s）后仍无空位则返回 429+Retry-After，压制 N×parallel_pool_size 放大。
// 获取到的信号量引用随 defer 持有，配置热重载重建 sem 时旧持有者仍从旧通道释放。
func (m *middleware) withConcurrencyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 平台存活探针不占业务并发额度，避免满载时返回 429 被误判为故障。
		// 必须精确匹配，不豁免 /health/ 或其他业务路径。
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		limit := m.cfg.MaxConcurrent()
		if limit <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		m.mu.Lock()
		if m.sem == nil || m.semSize != limit {
			m.sem = make(chan struct{}, limit)
			m.semSize = limit
		}
		sem := m.sem
		m.mu.Unlock()

		acquired := false
		t := time.NewTimer(2 * time.Second)
		select {
		case sem <- struct{}{}:
			acquired = true
		case <-r.Context().Done():
		case <-t.C:
		}
		t.Stop()
		if !acquired {
			w.Header().Set("Retry-After", "1")
			// 与 writeAuthError 同理：并发闸门的 429 也需按端点协议分流。
			if isOpenAIPath(r.URL.Path) {
				writeJSON(w, http.StatusTooManyRequests, transform.OpenAIErrorResponse{
					Error: transform.OpenAIErrorDetail{
						Message: "并发请求过多，请稍后重试 (too many concurrent requests)",
						Type:    transform.OpenAIErrTypeRateLimit,
						Code:    "rate_limit_exceeded",
					},
				})
				return
			}
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
				"code": 429, "message": "并发请求过多，请稍后重试 (too many concurrent requests)",
				"status": "RESOURCE_EXHAUSTED",
			}})
			return
		}
		defer func() { <-sem }()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *middleware) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Server] panic recovered: %v\n%s", rec, debug.Stack())
				writeGeminiError(w, r.Context(), vertex.NewInternalError("服务内部错误，请联系开发者 (internal error)", nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(m.cfg.MaxRequestMB()) << 20
		if limit > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func (m *middleware) withMetrics(next http.Handler) http.Handler {
	skip := map[string]bool{"/": true, "/health": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		reqID := reqID24()
		w.Header().Set("X-Request-Id", reqID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), vertex.RequestIDKey{}, reqID)
		ctx = context.WithValue(ctx, reqStartKey{}, time.Now())
		ctx = context.WithValue(ctx, reqKeyNameKey{}, m.keyNameOf(r))
		ctx = context.WithValue(ctx, reqRawKeyKey{}, extractAPIKey(r))
		ctx = context.WithValue(ctx, reqHeaderIDKey{}, strings.TrimSpace(r.Header.Get("X-User-Id")))
		// 流式标记按端点判定（此处尚未解析 body）：与 handler 内 stats 的口径一致。
		// /v1/chat/completions 是否流式由 body 的 stream 字段决定，中间件层无法得知；
		// OpenAI 客户端以流式为主，先按流式登记，handler 在解析 body 后立即经
		// cli.UpdateReqStreaming 纠正——与 Gemini 端点"中间件猜测 + handler 纠正"同模式。
		isStream := strings.Contains(r.URL.Path, ":streamGenerateContent") ||
			strings.HasSuffix(r.URL.Path, "/chat/completions")
		cli.StartReq(reqID, isStream)
		defer func() {
			// panic 展开时 defer 先于 withRecover 的 recover 执行，保证 TUI 表项必清理。
			cli.FinishReq(reqID, sw.status < 400)
		}()
		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(ctx))
		elapsed := time.Since(start)
		log.Printf("[Server] %s %s - %d (%.3fs) 请求ID=%s", r.Method, r.URL.Path, sw.status, elapsed.Seconds(), reqID)
	})
}

// keyNameOf 解析请求携带的 API key 并返回其名称（未知/缺省返回空串）。
func (m *middleware) keyNameOf(r *http.Request) string {
	key := extractAPIKey(r)
	if key == "" {
		return ""
	}
	if m.keys == nil {
		return ""
	}
	if name, ok := m.keys.NameOf(key); ok {
		return name
	}
	return ""
}

func (m *middleware) withAPIKey(next http.Handler) http.Handler {
	excluded := map[string]bool{"/": true, "/health": true, "/favicon.ico": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if excluded[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := extractAPIKey(r)
		if key == "" {
			writeAuthError(w, r, "缺少 API 密钥 (missing API key)")
			return
		}
		if key == "sk-your-key-here" {
			writeAuthError(w, r, "示例密钥禁止调用，请新建密钥。")
			return
		}
		if !m.keys.ValidateKey(key) {
			writeAuthError(w, r, "API 密钥无效 (invalid API key)")
			return
		}
		// 鉴权通过后注入 KeyName 与 RawKey 到请求 context（内层覆盖 withMetrics 的兜底值）。
		ctx := r.Context()
		if name, ok := m.keys.NameOf(key); ok {
			ctx = context.WithValue(ctx, reqKeyNameKey{}, name)
		}
		ctx = context.WithValue(ctx, reqRawKeyKey{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeAuthError 按端点协议写出鉴权失败响应。
//
// 鉴权中间件位于路由之前，却同时服务于 Gemini 与 OpenAI 两套协议，而两者的错误
// 规范不兼容（Gemini 是 {"error":{"code":401,"status":"UNAUTHENTICATED"}}，
// OpenAI 是 {"error":{"message":..,"type":..}}，且 code 为字符串）。
// 若统一按 Gemini 格式返回，OpenAI 客户端（含 new-api）解析 401 时会在
// code 字段上类型不匹配。故按路径前缀分流，保持各协议自身的错误形状。
func writeAuthError(w http.ResponseWriter, r *http.Request, msg string) {
	if isOpenAIPath(r.URL.Path) {
		writeJSON(w, http.StatusUnauthorized, transform.OpenAIErrorResponse{
			Error: transform.OpenAIErrorDetail{
				Message: msg,
				Type:    transform.OpenAIErrTypeInvalidRequest,
				Code:    "invalid_api_key",
			},
		})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
		"code": 401, "message": msg, "status": "UNAUTHENTICATED",
	}})
}

// isOpenAIPath 判定请求路径是否属于 OpenAI 协议专属路径。
//
// OpenAI 端点并入 /v1 后，路径前缀不再能区分协议：/v1/models 是双协议混响、
// /v1/models/{m}:generateContent 是 Gemini 专属，唯有 /v1/chat/completions 是
// OpenAI 专属。因此必须用精确匹配——若放宽为 HasPrefix(path, "/v1")，
// /v1/models 的鉴权失败会向 Gemini 客户端返回 OpenAI 格式错误（code 为字符串
// "invalid_api_key" 而非数字 401），破坏 new-api 等网关的类型预期。
func isOpenAIPath(path string) bool {
	return path == "/v1/chat/completions"
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/api/admin/") || strings.HasPrefix(path, "/assets/")
}
