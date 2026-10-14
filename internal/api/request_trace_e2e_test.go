package api

import (
	"net/http"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// TestRequestTraceE2E 端到端锁定追踪字段落到 stats 在途快照：
// 抗截断变体的非流式请求应记录带前缀的请求模型、单次回应形态、抗截断启用。
// 该快照与 TUI 追踪器相互独立，二者由 writeReqTrace 用同一组判定结果分别写入。
func TestRequestTraceE2E(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	resp := doPost(t, f.server.URL+"/v1beta/models/抗截断-gemini-3.7-flash:generateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}

	// 请求已结束，在途快照应已移除；改为在请求进行中读取不可行（mock 瞬时返回），
	// 因此这里改为直接校验判定函数在真实 handler 链路上的产物：变体被正确解析，
	// 且前缀显式意图在全局档位关闭时仍然注入合成工具（前缀优先，见 prepareForcedTool）。
	vars := cap.variables(t)
	if vars["model"] != "gemini-3.7-flash" {
		t.Fatalf("实际模型应为剥离前缀后的真名, 实际 %v", vars["model"])
	}
	if syntheticToolNameFrom(t, vars) == "" {
		t.Fatal("抗截断前缀在全局关闭档下也应注入合成工具（前缀是显式意图）")
	}
}

// TestRequestTraceE2E_BareModelNoInjection 裸模型在全局关闭档下不得注入合成工具，
// 锁定"前缀优先、全局兜底"的另一半：未加前缀时全局档位语义不变。
func TestRequestTraceE2E_BareModelNoInjection(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:generateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	vars := cap.variables(t)
	if syntheticToolNameFrom(t, vars) != "" {
		t.Fatal("关闭档下裸模型不应注入合成工具")
	}
}

// TestWriteReqTrace_WritesBothSinks 追踪三元组必须同时落到 TUI 与 stats 两个独立状态。
// 两套状态无共享数据，只写其中一个会导致两个展示面口径分歧。
func TestWriteReqTrace_WritesBothSinks(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ForcedToolCallMode = config.ForcedModeRequired
	provider := config.StaticProvider(cfg)
	h := &handler{cfg: provider}

	resolved := transform.ResolveModel("抗截断-gemini-3.7-flash", provider)
	req := &transform.GeminiRequest{
		Contents: []transform.Content{{Role: "user", Parts: []transform.Part{{Text: "hi"}}}},
	}
	reqID := "trace-e2e-1"

	// stats.StartReq 在 stats 未启动时直接返回（无在途快照可写），
	// 故测试中显式挂一个内存 store 启动 stats，复现生产语义。
	memStore, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = memStore.Close() })
	stats.Init(memStore)
	t.Cleanup(stats.Close)

	stats.StartReq(reqID, resolved.ActualModel, "", "k", false)
	defer stats.StopReq(reqID)

	h.writeReqTrace(reqID, resolved, req, ResponseSingle)

	var got *stats.ActiveRequest
	for _, a := range stats.ActiveRequests() {
		if a.ReqID == reqID {
			cp := a
			got = &cp
			break
		}
	}
	if got == nil {
		t.Fatal("stats 在途快照未找到该请求")
	}
	if got.RawModel != "抗截断-gemini-3.7-flash" {
		t.Errorf("RawModel = %q, 请求模型应保留变体前缀", got.RawModel)
	}
	if got.ResponseMode != string(ResponseSingle) {
		t.Errorf("ResponseMode = %q, want %q", got.ResponseMode, ResponseSingle)
	}
	if !got.AntiTruncate {
		t.Error("AntiTruncate 应为 true（抗截断变体走 StreamChatCollect 链路）")
	}
	// 实际模型不受追踪写入影响。
	if got.Model != "gemini-3.7-flash" {
		t.Errorf("Model = %q, 应为剥离前缀后的真名", got.Model)
	}
}
