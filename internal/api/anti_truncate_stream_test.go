package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestAntiTruncateStreamInjectsSyntheticTool 锁定流式入口的抗截断链路（修复前是死代码）：
// 抗截断- 前缀 + 流式端点，即使全局 forced_tool_call_mode 关闭，
// 上游请求也必须装配合成工具与 ANY 强制调用。
func TestAntiTruncateStreamInjectsSyntheticTool(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	resp := doPost(t, f.server.URL+"/v1beta/models/抗截断-gemini-3.7-flash:streamGenerateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d (body=%s)", resp.StatusCode, string(raw))
	}

	vars := cap.variables(t)
	if syntheticToolNameFrom(t, vars) == "" {
		t.Fatal("流式端点的抗截断前缀应注入合成工具")
	}
	rawReq := cap.raw()
	if !strings.Contains(rawReq, `"ANY"`) {
		t.Fatalf("流式抗截断应以 ANY 模式强制调用: %s", rawReq)
	}
	// 客户端交付侧：合成调用不得泄漏给客户端。
	if strings.Contains(string(raw), forcedE2EToolPrefix) {
		t.Fatalf("合成工具名不得出现在客户端响应中: %s", string(raw))
	}
}

// TestStreamBareModelRespectsGlobalOff 全局关闭档下，未加前缀的流式请求不注入。
func TestStreamBareModelRespectsGlobalOff(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:streamGenerateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if syntheticToolNameFrom(t, cap.variables(t)) != "" {
		t.Fatal("关闭档 + 裸模型的流式请求不应注入合成工具")
	}
}

// TestDroppedParamsHeader 回显将被剥离的不支持参数（x-gemini-dropped-params）。
func TestDroppedParamsHeader(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "x"), nil)

	body := simpleUserBody()
	body["generationConfig"] = map[string]any{
		"temperature":      1.0,
		"topK":             32,
		"frequencyPenalty": 0.5,
	}
	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:generateContent",
		"sk-test-key", body)
	defer func() { _ = resp.Body.Close() }()
	geminiTextResponse(t, resp)

	got := resp.Header.Get("X-Gemini-Dropped-Params")
	if got == "" {
		t.Fatal("应回显 x-gemini-dropped-params 响应头")
	}
	for _, want := range []string{"temperature", "top_k", "frequency_penalty"} {
		if !strings.Contains(got, want) {
			t.Errorf("响应头缺少 %q: %q", want, got)
		}
	}
	// 无可剥离参数时不设头。
	resp2 := doPost(t, f.server.URL+"/v1beta/models/gemini-2.5-flash:generateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp2.Body.Close() }()
	geminiTextResponse(t, resp2)
	if v := resp2.Header.Get("X-Gemini-Dropped-Params"); v != "" {
		t.Errorf("无可剥离参数时不应设头，实得 %q", v)
	}
}
