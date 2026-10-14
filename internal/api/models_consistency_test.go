package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestHiddenVariantStillCallable 契约固化：关闭「非流前缀」只把变体从清单中隐藏（不再对外广告），
// 直接按变体名调用仍按变体语义生效——「隐藏变体不是权限控制」，与 models 清单为暴露面而非权限面一致。
func TestHiddenVariantStillCallable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	var cap upstreamCapture
	fx, _ := catalogFetchFixture(t, plainTextUpstreamMock(t, &cap, "隐藏变体正文"),
		`{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`,
		`{"disabled_models":[],"custom_models":[],"model_options":{"gemini-3.6-flash":{"non_stream_prefix":false}}}`)

	get := func(path string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, fx.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-test-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return string(raw)
	}

	// 清单中不出现该变体
	if listBody := get("/v1beta/models"); strings.Contains(listBody, "假非流-gemini-3.6-flash") {
		t.Fatalf("显式关闭后变体不应出现在清单: %s", listBody)
	}

	// 直接调用仍生效：假非流变体走聚合交付（单帧 SSE）
	resp := doPost(t, fx.server.URL+"/v1beta/models/假非流-gemini-3.6-flash:streamGenerateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("隐藏变体直接调用应成功，状态码 %d (body=%s)", resp.StatusCode, string(raw))
	}
	body := string(raw)
	if !strings.Contains(body, "隐藏变体正文") {
		t.Fatalf("聚合交付应包含正文: %s", body)
	}
	if n := strings.Count(body, "data:"); n != 1 {
		t.Fatalf("假非流应为单帧聚合交付，data 行数=%d (body=%s)", n, body)
	}
}

// /v1/models（OpenAI 混响）、/v1beta/models（Gemini 原生）与单模型详情的白名单
// 必须同时反映「启用集 + 按模型选项展开的变体」，且视频家族整体不暴露。
func TestClientModelListsConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	fx, _, _ := adminModelsFixture(t,
		`{"models":["gemini-3.6-flash","gemini-3.7-flash","gemini-3.1-flash-image",`+
			`"veo-3.0-generate-preview","gemini-4-argon"],"alias_map":{},"family_map":{}}`,
		`{"disabled_models":[],"custom_models":["gemini-4-argon"],`+
			`"model_options":{"gemini-3.7-flash":{"anti_truncate":false}}}`)

	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, fx.server.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer sk-test-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	geminiStatus, geminiBody := get("/v1beta/models")
	if geminiStatus != http.StatusOK {
		t.Fatalf("/v1beta/models 状态码 %d (body=%s)", geminiStatus, geminiBody)
	}
	openaiStatus, openaiBody := get("/v1/models")
	if openaiStatus != http.StatusOK {
		t.Fatalf("/v1/models 状态码 %d (body=%s)", openaiStatus, openaiBody)
	}

	// 期望的暴露集合：文本模型 + 按其显式选项展开的变体 + 图像模型；视频不暴露。
	expectPresent := []string{
		"gemini-3.6-flash", "假非流-gemini-3.6-flash", "抗截断-gemini-3.6-flash",
		"gemini-3.7-flash", "假非流-gemini-3.7-flash", // anti 显式关闭 → 无抗截断变体
		"gemini-3.1-flash-image", // 图像家族：不产生文本变体
		"gemini-4-argon",         // 本地自定义模型照常暴露
	}
	expectAbsent := []string{
		"抗截断-gemini-3.7-flash",
		"假非流-gemini-3.1-flash-image",
		"抗截断-gemini-3.1-flash-image",
		"veo-3.0-generate-preview",
		"抗截断-veo-3.0-generate-preview",
	}
	for _, name := range expectPresent {
		if !strings.Contains(geminiBody, `"`+name+`"`) {
			t.Errorf("/v1beta/models 缺少 %s: %s", name, geminiBody)
		}
		if !strings.Contains(openaiBody, `"`+name+`"`) {
			t.Errorf("/v1/models 缺少 %s: %s", name, openaiBody)
		}
	}
	for _, name := range expectAbsent {
		if strings.Contains(geminiBody, `"`+name+`"`) {
			t.Errorf("/v1beta/models 不应包含 %s", name)
		}
		if strings.Contains(openaiBody, `"`+name+`"`) {
			t.Errorf("/v1/models 不应包含 %s", name)
		}
	}

	// 单模型详情与白名单自洽：暴露名 200，未暴露名 404。
	absentSet := make(map[string]bool, len(expectAbsent))
	for _, name := range expectAbsent {
		absentSet[name] = true
	}
	for _, name := range append(append([]string{}, expectPresent...), expectAbsent...) {
		status, body := get("/v1beta/models/" + name)
		want := http.StatusOK
		if absentSet[name] {
			want = http.StatusNotFound
		}
		if status != want {
			t.Errorf("单模型详情 %s 状态码 %d, want %d (body=%s)", name, status, want, body)
		}
	}
}
