package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// getWithKey 发起带 API Key 的 GET 请求。
func getWithKey(t *testing.T, url, apiKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestAntiTruncateVariant_PayloadUnchanged 锁定"前缀优先、全局兜底"的裸模型侧：
// 未加前缀时，同一请求的 variables 与基线一致——全局档位语义不因本特性漂移。
// 抗截断变体会注入合成工具（这正是它的意义），注入行为由
// TestAntiTruncateVariant_InjectsSyntheticTool 单独锁定。
func TestAntiTruncateVariant_PayloadUnchanged(t *testing.T) {
	collect := func(modelPath string) []byte {
		var cap upstreamCapture
		f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "x"), nil)
		resp := doPost(t, f.server.URL+"/v1beta/models/"+modelPath+":generateContent", "sk-test-key", simpleUserBody())
		defer func() { _ = resp.Body.Close() }()
		_ = geminiTextResponse(t, resp)

		vars := cap.variables(t)
		// 剥掉每次请求随机生成的包壳字段，只比对真正发往上游的 variables。
		out, err := json.Marshal(vars)
		if err != nil {
			t.Fatalf("序列化 variables 失败: %v", err)
		}
		return out
	}

	plain := collect("gemini-3.7-flash")
	variant := collect("假非流-gemini-3.7-flash")
	if string(plain) != string(variant) {
		t.Fatalf("假非流变体改动了上游请求体:\n裸模型: %s\n变体:   %s", plain, variant)
	}
}

// TestAntiTruncateVariant_InjectsSyntheticTool 抗截断变体是单次请求的显式意图：
// 全局档位关闭时也必须注入合成工具（正文经工具参数传输并强制调用）。
func TestAntiTruncateVariant_InjectsSyntheticTool(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "x"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})
	resp := doPost(t, f.server.URL+"/v1beta/models/抗截断-gemini-3.7-flash:generateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	_ = geminiTextResponse(t, resp)

	vars := cap.variables(t)
	name := syntheticToolNameFrom(t, vars)
	if name == "" {
		t.Fatal("抗截断变体应注入合成工具")
	}
	// 强制档语义：toolConfig.mode 应为 ANY（正文必经合成工具）。
	tc, ok := vars["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("toolConfig 应存在: %+v", vars["toolConfig"])
	}
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc == nil || fcc["mode"] != "ANY" {
		t.Fatalf("functionCallingConfig.mode 应为 ANY: %+v", fcc)
	}
}

// TestModelsEndpoint_ListsVariants 模型清单端点必须同时暴露假非流与抗截断两种文本变体，
// 且不为非文本模型暴露任何变体。
func TestModelsEndpoint_ListsVariants(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "x"), nil)

	resp := getWithKey(t, f.server.URL+"/v1beta/models", "sk-test-key")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	var parsed struct {
		Models []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, string(raw))
	}

	// name 带 models/ 前缀，version 是裸模型名（含变体前缀）。
	names := map[string]bool{}
	for _, m := range parsed.Models {
		names[m.Version] = true
	}

	for _, want := range []string{
		"gemini-3.7-flash",
		"假非流-gemini-3.7-flash",
		"抗截断-gemini-3.7-flash",
	} {
		if !names[want] {
			t.Errorf("模型清单缺少 %q", want)
		}
	}
	for _, unwanted := range []string{
		"假非流-gemini-3.1-flash-image",
		"抗截断-gemini-3.1-flash-image",
		"抗截断-gemini-3.1-flash-tts-preview",
	} {
		if names[unwanted] {
			t.Errorf("模型清单不应包含非文本模型的变体 %q", unwanted)
		}
	}
}

// TestFakeNonStreamVariant_StillAggregates 假非流变体的既有行为不得被抗截断变体改动影响：
// 客户端请求流式端点，代理聚合成单个 SSE 帧返回。
func TestFakeNonStreamVariant_StillAggregates(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "聚合后的正文"), nil)

	resp := doPost(t, f.server.URL+"/v1beta/models/假非流-gemini-3.7-flash:streamGenerateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body := string(raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d (body=%s)", resp.StatusCode, body)
	}
	if n := strings.Count(body, "data: "); n != 1 {
		t.Fatalf("假非流应聚合为单个 SSE 帧，实际 %d 帧: %s", n, body)
	}
	if !strings.Contains(body, "聚合后的正文") {
		t.Fatalf("聚合帧缺少正文: %s", body)
	}
}

// TestForcedToolE2E_OffPreservesClientToolsAndToolConfig 关闭档下客户端自带的工具与
// toolConfig 必须原样透传——既有结构的任何改动都属回归。
func TestForcedToolE2E_OffPreservesClientToolsAndToolConfig(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "原样"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	body := simpleUserBody()
	body["tools"] = []any{map[string]any{"functionDeclarations": []any{map[string]any{
		"name":        "client_tool",
		"description": "客户端工具",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}}}}
	body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", body)
	defer func() { _ = resp.Body.Close() }()
	_ = geminiTextResponse(t, resp)

	raw := cap.raw()
	if strings.Contains(raw, forcedE2EToolPrefix) {
		t.Fatalf("关闭档不得注入合成工具: %s", raw)
	}
	if strings.Contains(raw, "allowedFunctionNames") {
		t.Fatalf("关闭档不得写入 allowedFunctionNames: %s", raw)
	}

	vars := cap.variables(t)
	names := upstreamToolNames(t, vars)
	if len(names) != 1 || names[0] != "client_tool" {
		t.Fatalf("关闭档应原样透传客户端工具，实际 %v", names)
	}

	toolConfig, ok := vars["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("关闭档应保留客户端 toolConfig: %s", raw)
	}
	fcc, _ := toolConfig["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "AUTO" {
		t.Fatalf("关闭档不得覆盖客户端 mode，实际 %v", fcc["mode"])
	}

	// 客户端未声明任何控制指令，不得凭空注入。
	if strings.Contains(raw, "请只通过工具") {
		t.Fatalf("关闭档不得注入控制指令: %s", raw)
	}
}
