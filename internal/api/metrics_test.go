package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// firstReqKey 是并发闸门测试用的 context 键：staticcheck SA1029 要求
// context key 使用自定义类型而非内建 string，避免跨模块键冲突。
type firstReqKey struct{}

// TestWithMetrics 验证 withMetrics 中间件行为：
// 设 X-Request-Id、注入 context、跳过 /health。
func TestWithMetrics(t *testing.T) {
	mw := &middleware{} //nolint:exhaustruct

	var seenReqID string
	ok := mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenReqID = vertex.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	ok.ServeHTTP(rec, httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-flash:generateContent", nil))
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("应设置 X-Request-Id 响应头")
	}
	if seenReqID == "" || seenReqID != rec.Header().Get("X-Request-Id") {
		t.Fatalf("context 里的 request-id 应与响应头一致，got ctx=%q header=%q", seenReqID, rec.Header().Get("X-Request-Id"))
	}

	// /health 应被跳过，不设 request-id。
	rec2 := httptest.NewRecorder()
	mw.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec2, httptest.NewRequest("GET", "/health", nil))
	if rec2.Header().Get("X-Request-Id") != "" {
		t.Fatal("/health 不应设 X-Request-Id")
	}
}

// TestWithAPIKey_ContextInjection 验证鉴权通过后 KeyName 与 RawKey 注入 context。
func TestWithAPIKey_ContextInjection(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "api_keys.txt")
	if err := os.WriteFile(keysFile, []byte("alice:sk-test-key-123:Test Key\n"), 0o644); err != nil {
		t.Fatalf("write keys: %v", err)
	}
	t.Setenv("VPROXY_API_KEYS", keysFile)

	keys := NewAPIKeyManager()
	keys.LoadKeys()
	mw := &middleware{keys: keys, cfg: config.StaticProvider(config.DefaultConfig())}

	handler := mw.withAPIKey(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := keyNameFromContext(ctx)
		raw := rawKeyFromContext(ctx)
		if name != "alice" {
			t.Errorf("KeyName=%q, want alice", name)
		}
		if raw != "sk-test-key-123" {
			t.Errorf("RawKey=%q, want sk-test-key-123", raw)
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-flash:generateContent", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key-123")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
}

// TestRawKeyFromContext 验证未注入时 rawKeyFromContext 返回空串。
func TestRawKeyFromContext(t *testing.T) {
	if got := rawKeyFromContext(context.Background()); got != "" {
		t.Fatalf("空 context 应返回空串, got %q", got)
	}
}

// TestWithConcurrencyLimit_Gate 验证入站并发闸门：
// max_concurrent>0 时超限请求返回 429+Retry-After；空位释放后恢复放行；<=0 时不限制。
func TestWithConcurrencyLimit_Gate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxConcurrent = 1
	mw := &middleware{cfg: config.StaticProvider(cfg)} //nolint:exhaustruct

	firstEntered := make(chan struct{})
	release := make(chan struct{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Value(firstReqKey{}).(bool); ok {
			close(firstEntered)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := mw.withConcurrencyLimit(inner)

	// 第一个请求占用唯一槽位。
	go func() {
		ctx := context.WithValue(context.Background(), firstReqKey{}, true)
		req := httptest.NewRequest("POST", "/x", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}()

	<-firstEntered

	// 第二个请求应被闸门 429（等待 2s 缓冲后仍无空位）。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("gate status=%d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("gate Retry-After=%q, want 1", rec.Header().Get("Retry-After"))
	}

	// 释放后新请求应被放行。
	close(release)
	time.Sleep(50 * time.Millisecond)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest("POST", "/x", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("after release status=%d, want 200", rec2.Code)
	}

	// max_concurrent<=0 不限制。
	cfg2 := config.DefaultConfig()
	cfg2.MaxConcurrent = 0
	mw2 := &middleware{cfg: config.StaticProvider(cfg2)} //nolint:exhaustruct
	rec3 := httptest.NewRecorder()
	mw2.withConcurrencyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec3, httptest.NewRequest("POST", "/x", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("disabled gate status=%d, want 200", rec3.Code)
	}
}

// TestWriteGeminiError_RetryAfterHeader 验证 429 错误透传 Retry-After 响应头。
func TestWriteGeminiError_RetryAfterHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	e := vertex.NewRateLimitError("too many", 30, nil)
	writeGeminiError(rec, context.Background(), e)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("Retry-After=%q, want 30", rec.Header().Get("Retry-After"))
	}

	// RetryAfter=0 时不应写头。
	rec2 := httptest.NewRecorder()
	writeGeminiError(rec2, context.Background(), vertex.NewRateLimitError("too many", 0, nil))
	if rec2.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After should be empty when RetryAfter=0, got %q", rec2.Header().Get("Retry-After"))
	}
}
