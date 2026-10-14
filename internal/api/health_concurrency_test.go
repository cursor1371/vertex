package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

func TestHealthBypassesSaturatedConcurrencyGate(t *testing.T) {
	cfg := config.StaticProvider(config.AppConfig{MaxConcurrent: 1})
	srv := NewServer(ServerDeps{Cfg: cfg, Keys: NewAPIKeyManager()})
	// 模拟唯一业务槽已占用，不依赖网络或 goroutine 的调度时序。
	srv.mw.sem = make(chan struct{}, 1)
	srv.mw.semSize = 1
	srv.mw.sem <- struct{}{}
	h := srv.Handler()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		req := httptest.NewRequest(method, "/health", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		cancel()
		if rec.Code != http.StatusOK {
			t.Fatalf("health status=%d, want 200", rec.Code)
		}
		if len(srv.mw.sem) != 1 || rec.Header().Get("Retry-After") != "" {
			t.Fatal("health must not touch the business semaphore")
		}
	}

	for _, path := range []string{"/health/", "/health-extra", "/v1/models", "/v1/chat/completions"} {
		// 已取消的上下文使满载业务请求立即返回，无需每例等待 2 秒。
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("path=%s must still be limited, got %d", path, rec.Code)
		}
		if path == "/v1/chat/completions" && !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
			t.Fatal("OpenAI rate-limit error shape changed")
		}
	}
}

func TestHealthExceptionDoesNotBypassBusinessAuthentication(t *testing.T) {
	cfg := config.StaticProvider(config.AppConfig{MaxConcurrent: 1})
	h := NewServer(ServerDeps{Cfg: cfg, Keys: NewAPIKeyManager()}).Handler()
	for _, path := range []string{"/v1/models", "/v1beta/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("path=%s status=%d, want 401", path, rec.Code)
		}
	}
}
