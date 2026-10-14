package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// videoRejectFixture 建立含视频模型的清单（含别名与 family_map 覆盖）与
// 记录调用次数的 mock 上游，用于断言"视频请求零上游触达"。
func videoRejectFixture(t *testing.T) (*testFixture, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	models := `{
  "models": ["gemini-3.6-flash", "veo-3.0-generate-preview", "my-clip-model"],
  "alias_map": {"my-video-alias": "veo-3.0-generate-preview"},
  "family_map": {"my-clip-model": "vedio"}
}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatalf("写入 models.json 失败: %v", err)
	}
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	t.Cleanup(config.InvalidateModelsCache)

	var calls atomic.Int32
	f := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
			geminiNonStreamingResponse() + `}}}]}]`))
	}, nil)
	return f, &calls
}

// TestVideoModelRejectedWithoutUpstream 视频家族（vedio）在全部生成入口被分发层拒绝：
// 返回明确 4xx、错误信息说明"尚未实现"，且**零上游请求**（绝不误入文本 pipeline）。
func TestVideoModelRejectedWithoutUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cases := []struct { //nolint:govet // 测试表：字段顺序按可读性优先
		name string
		path string
		body any
	}{
		{
			name: "原生非流式",
			path: "/v1beta/models/veo-3.0-generate-preview:generateContent",
			body: simpleUserBody(),
		},
		{
			name: "原生流式",
			path: "/v1beta/models/veo-3.0-generate-preview:streamGenerateContent",
			body: simpleUserBody(),
		},
		{
			name: "抗截断变体前缀",
			path: "/v1beta/models/抗截断-veo-3.0-generate-preview:generateContent",
			body: simpleUserBody(),
		},
		{
			name: "别名指向视频模型",
			path: "/v1beta/models/my-video-alias:generateContent",
			body: simpleUserBody(),
		},
		{
			name: "family_map 覆盖为 vedio",
			path: "/v1beta/models/my-clip-model:generateContent",
			body: simpleUserBody(),
		},
		{
			name: "OpenAI 兼容层",
			path: "/v1/chat/completions",
			body: map[string]any{
				"model":    "veo-3.0-generate-preview",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, calls := videoRejectFixture(t)
			resp := doPost(t, f.server.URL+tc.path, "sk-test-key", tc.body)
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("读取响应失败: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码应为 400，实际 %d (body=%s)", resp.StatusCode, string(raw))
			}
			if !strings.Contains(string(raw), "视频") || !strings.Contains(string(raw), "尚未实现") {
				t.Fatalf("错误信息应明确说明视频能力未实现，got %s", string(raw))
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("视频请求不得触达上游，实际发出 %d 次上游请求", n)
			}
		})
	}
}

// TestTextModelStillServedAlongsideVideoModels 回归锚点：清单里同时存在视频模型时，
// 文本模型的服务不受影响（硬闸不得过度拦截）。
func TestTextModelStillServedAlongsideVideoModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	f, calls := videoRejectFixture(t)
	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.6-flash:generateContent", "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("文本模型应正常服务，实际 %d (body=%s)", resp.StatusCode, string(raw))
	}
	if n := calls.Load(); n == 0 {
		t.Fatal("文本模型请求应触达上游")
	}
}

// TestVideoModelHiddenFromClientLists 客户端模型清单不暴露视频模型（vedio 过滤），
// 且单模型详情查询返回 404（与清单自洽）。
func TestVideoModelHiddenFromClientLists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	f, _ := videoRejectFixture(t)

	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
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

	for _, path := range []string{"/v1beta/models", "/v1/models"} {
		status, body := get(path)
		if status != http.StatusOK {
			t.Fatalf("%s 状态码 %d (body=%s)", path, status, body)
		}
		if strings.Contains(body, "veo-3.0-generate-preview") || strings.Contains(body, "my-clip-model") {
			t.Fatalf("%s 不应暴露视频家族模型: %s", path, body)
		}
		if !strings.Contains(body, "gemini-3.6-flash") {
			t.Fatalf("%s 应包含文本模型: %s", path, body)
		}
	}

	// 单模型详情：不在清单白名单 → 404
	status, _ := get("/v1beta/models/veo-3.0-generate-preview")
	if status != http.StatusNotFound {
		t.Fatalf("视频模型详情应为 404，实际 %d", status)
	}
}
