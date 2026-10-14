package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// forcedE2EToolPrefix 与 transform.ForcedToolNamePrefix 一致；此处独立书写以形成交叉校验。
const forcedE2EToolPrefix = "vertex_emit_"

// upstreamCapture 线程安全地留存上游请求体与观察到的工具名。
// mock handler 运行在服务端 goroutine、断言运行在测试 goroutine，必须显式同步。
type upstreamCapture struct {
	mu    sync.Mutex
	body  []byte
	names []string
}

func (c *upstreamCapture) record(body []byte, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = body
	c.names = append(c.names, name)
}

func (c *upstreamCapture) raw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.body)
}

func (c *upstreamCapture) variables(t *testing.T) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(c.raw()), &envelope); err != nil {
		t.Fatalf("解析上游请求体失败: %v (body=%s)", err, c.raw())
	}
	vars, ok := envelope["variables"].(map[string]any)
	if !ok {
		t.Fatalf("上游请求体缺少 variables 段: %s", c.raw())
	}
	return vars
}

func (c *upstreamCapture) observedNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...)
}

// upstreamToolNames 取出上游请求 tools 中的全部函数名。
func upstreamToolNames(t *testing.T, vars map[string]any) []string {
	t.Helper()
	var names []string
	tools, _ := vars["tools"].([]any)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		decls, _ := tool["functionDeclarations"].([]any)
		for _, rawDecl := range decls {
			decl, ok := rawDecl.(map[string]any)
			if !ok {
				continue
			}
			if n, ok := decl["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}

// syntheticToolNameFrom 从上游请求中找出合成工具名——回包必须使用同一个名字，
// 这正是"请求级名字绑定"的端到端验证点。
func syntheticToolNameFrom(t *testing.T, vars map[string]any) string {
	t.Helper()
	for _, n := range upstreamToolNames(t, vars) {
		if strings.HasPrefix(n, forcedE2EToolPrefix) {
			return n
		}
	}
	return ""
}

// geminiNonStreamFrame 构造单帧上游响应（data.ui.streamGenerateContentAnonymous 的值）。
func geminiNonStreamFrame(partsJSON string) string {
	return fmt.Sprintf(`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":`+
		`{"candidates":[{"content":{"parts":[%s],"role":"model"},"finishReason":"STOP","index":0}]}}}}]}]`, partsJSON)
}

// forcedUpstreamMock 以"同名合成工具调用"回包，并记录上游请求。
func forcedUpstreamMock(t *testing.T, cap *upstreamCapture, answer string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取上游请求体失败: %v", err)
			return
		}
		name := syntheticToolNameFrom(t, variablesOf(t, body))
		cap.record(body, name)
		if name == "" {
			t.Errorf("上游请求未包含合成工具声明")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, geminiNonStreamFrame(fmt.Sprintf(
			`{"functionCall":{"name":%q,"args":{"content":%q}}}`, name, answer)))
	}
}

// plainTextUpstreamMock 返回普通文本响应（模型未遵守封装契约）。
func plainTextUpstreamMock(t *testing.T, cap *upstreamCapture, text string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取上游请求体失败: %v", err)
			return
		}
		name := syntheticToolNameFrom(t, variablesOf(t, body))
		cap.record(body, name)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, geminiNonStreamFrame(fmt.Sprintf(`{"text":%q}`, text)))
	}
}

// variablesOf 从原始请求体解析 variables 段（供 mock handler 使用）。
func variablesOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Errorf("解析上游请求体失败: %v", err)
		return nil
	}
	vars, _ := envelope["variables"].(map[string]any)
	return vars
}

// geminiTextResponse 从客户端响应中取出首候选的全部文本，并断言不出现 functionCall。
func geminiTextResponse(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d (body=%s)", resp.StatusCode, string(raw))
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall any    `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, string(raw))
	}
	var sb strings.Builder
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			if p.FunctionCall != nil {
				t.Fatalf("客户端不应看到 functionCall: %s", string(raw))
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func forcedGenerateURL(f *testFixture) string {
	return f.server.URL + "/v1beta/models/gemini-3.7-flash:generateContent"
}

func simpleUserBody() map[string]any {
	return map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
	}
}

// TestForcedToolE2E_Required 强制档端到端：客户端只见纯文本；上游请求含同名合成工具、
// mode=ANY、不含 allowedFunctionNames；用户 prompt 未被包裹。
func TestForcedToolE2E_Required(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, forcedUpstreamMock(t, &cap, "解包后的正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	if got := geminiTextResponse(t, resp); got != "解包后的正文" {
		t.Fatalf("客户端文本不符: %q", got)
	}

	raw := cap.raw()
	vars := cap.variables(t)

	name := syntheticToolNameFrom(t, vars)
	if name == "" {
		t.Fatalf("上游请求未含合成工具: %s", raw)
	}
	// allowedFunctionNames 必须整个字段不存在（不能只判空数组）。
	if strings.Contains(raw, "allowedFunctionNames") {
		t.Fatalf("上游请求不应出现 allowedFunctionNames: %s", raw)
	}

	toolConfig, ok := vars["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("强制档应设置 toolConfig: %s", raw)
	}
	fcc, ok := toolConfig["functionCallingConfig"].(map[string]any)
	if !ok || fcc["mode"] != "ANY" {
		t.Fatalf("toolConfig.functionCallingConfig.mode 应为 ANY: %v", toolConfig)
	}

	// 用户 prompt 未被包裹：contents[0] 仍是原始 user 文本回合。
	contents, _ := vars["contents"].([]any)
	if len(contents) == 0 {
		t.Fatalf("上游请求缺少 contents: %s", raw)
	}
	first, _ := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("contents[0].role 应为 user: %v", first["role"])
	}
	firstParts, _ := first["parts"].([]any)
	if len(firstParts) == 0 {
		t.Fatalf("contents[0] 缺少 parts: %v", first)
	}
	firstPart, _ := firstParts[0].(map[string]any)
	if firstPart["text"] != "hi" {
		t.Fatalf("用户 prompt 被改动（不应包裹）: %v", firstPart)
	}

	// 控制指令已注入并点名合成工具。
	if !strings.Contains(raw, name) {
		t.Fatalf("控制指令未点名合成工具 %q: %s", name, raw)
	}
}

// TestForcedToolE2E_Prefer 优先档：注入合成工具但不强制 mode，且 toolConfig 整个字段不出现。
func TestForcedToolE2E_Prefer(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "模型直接回了普通文本"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModePrefer
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	if got := geminiTextResponse(t, resp); got != "模型直接回了普通文本" {
		t.Fatalf("优先档应原样透传普通文本: %q", got)
	}

	vars := cap.variables(t)
	if syntheticToolNameFrom(t, vars) == "" {
		t.Fatal("优先档仍应注入合成工具")
	}
	if _, exists := vars["toolConfig"]; exists {
		t.Fatalf("优先档不得出现 toolConfig 字段: %v", vars["toolConfig"])
	}
}

// TestForcedToolE2E_CoexistsWithClientTools 客户端真实工具必须被保留而非替换。
func TestForcedToolE2E_CoexistsWithClientTools(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "ok"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	body := simpleUserBody()
	body["tools"] = []any{map[string]any{"functionDeclarations": []any{map[string]any{
		"name":        "client_tool",
		"description": "客户端自己的工具",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
	}}}}
	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", body)
	defer func() { _ = resp.Body.Close() }()
	_ = geminiTextResponse(t, resp)

	vars := cap.variables(t)
	var hasClient, hasSynthetic bool
	for _, n := range upstreamToolNames(t, vars) {
		if n == "client_tool" {
			hasClient = true
		}
		if strings.HasPrefix(n, forcedE2EToolPrefix) {
			hasSynthetic = true
		}
	}
	if !hasClient {
		t.Fatalf("客户端工具被删除: %v", upstreamToolNames(t, vars))
	}
	if !hasSynthetic {
		t.Fatalf("合成工具未与客户端工具共存: %v", upstreamToolNames(t, vars))
	}
	if !strings.Contains(cap.raw(), "客户端自己的工具") {
		t.Fatal("客户端工具描述被改写")
	}
}

// TestForcedToolE2E_OffIsZeroChange 关闭档下上游请求不得出现任何注入痕迹。
func TestForcedToolE2E_OffIsZeroChange(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "原样文本"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	if got := geminiTextResponse(t, resp); got != "原样文本" {
		t.Fatalf("关闭档应原样透传: %q", got)
	}

	vars := cap.variables(t)
	if names := upstreamToolNames(t, vars); len(names) != 0 {
		t.Fatalf("关闭档不得注入工具: %v", names)
	}
	if _, exists := vars["toolConfig"]; exists {
		t.Fatalf("关闭档不得设置 toolConfig: %v", vars["toolConfig"])
	}
	if strings.Contains(cap.raw(), forcedE2EToolPrefix) {
		t.Fatalf("关闭档上游请求不应出现合成工具前缀: %s", cap.raw())
	}
}

// TestForcedToolE2E_Stream 流式端到端：SSE 中只应出现解包后的文本，不得泄漏 functionCall。
func TestForcedToolE2E_Stream(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, forcedUpstreamMock(t, &cap, "流式解包正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:streamGenerateContent", "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取流式响应失败: %v", err)
	}
	body := string(raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d (body=%s)", resp.StatusCode, body)
	}
	if strings.Contains(body, "functionCall") {
		t.Fatalf("流式响应泄漏了 functionCall: %s", body)
	}
	if !strings.Contains(body, "流式解包正文") {
		t.Fatalf("流式响应缺少解包正文: %s", body)
	}
	if !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("流式响应丢失 finishReason: %s", body)
	}
	if !strings.Contains(cap.raw(), forcedE2EToolPrefix) {
		t.Fatal("流式上游请求未注入合成工具")
	}
}

// TestForcedToolE2E_NameBindingStable 端到端锁定请求级名字绑定：
// mock 从请求中读出工具名再以同名回包；若名字在尝试间漂移，解包将失败、客户端拿不到正文。
func TestForcedToolE2E_NameBindingStable(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, forcedUpstreamMock(t, &cap, "绑定正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	if got := geminiTextResponse(t, resp); got != "绑定正文" {
		t.Fatalf("名字绑定失败导致解包未生效: %q", got)
	}
	names := cap.observedNames()
	if len(names) == 0 {
		t.Fatal("mock 未收到任何上游请求")
	}
	for i, n := range names {
		if n == "" {
			t.Fatalf("第 %d 次上游请求未携带合成工具名", i+1)
		}
		if n != names[0] {
			t.Fatalf("同一次客户端请求的上游工具名发生漂移: %v", names)
		}
	}
}

// TestForcedToolE2E_SkipsWhenBuiltinToolPresent 带内置工具的请求应整体跳过（内置工具不产生
// functionCall，与 mode=ANY 语义冲突）。
func TestForcedToolE2E_SkipsWhenBuiltinToolPresent(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "检索后的回答"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	body := simpleUserBody()
	body["tools"] = []any{map[string]any{"googleSearch": map[string]any{}}}
	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", body)
	defer func() { _ = resp.Body.Close() }()
	_ = geminiTextResponse(t, resp)

	if strings.Contains(cap.raw(), forcedE2EToolPrefix) {
		t.Fatalf("带内置工具时不应注入合成工具: %s", cap.raw())
	}
	if _, exists := cap.variables(t)["toolConfig"]; exists {
		t.Fatal("带内置工具时不应设置 toolConfig")
	}
}

// TestForcedToolE2E_SkipsWhenClientModeNone 客户端显式 NONE 时应尊重其意图。
func TestForcedToolE2E_SkipsWhenClientModeNone(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "客户端指定不使用工具"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	body := simpleUserBody()
	body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", body)
	defer func() { _ = resp.Body.Close() }()
	_ = geminiTextResponse(t, resp)

	if strings.Contains(cap.raw(), forcedE2EToolPrefix) {
		t.Fatalf("客户端显式 NONE 时不应注入合成工具: %s", cap.raw())
	}
}

// TestForcedToolE2E_WhitelistMiss 白名单未命中时应整体跳过。
func TestForcedToolE2E_WhitelistMiss(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "未命中白名单"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
		c.ForcedToolCallModels = []string{"some-other-model"}
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	_ = geminiTextResponse(t, resp)

	if strings.Contains(cap.raw(), forcedE2EToolPrefix) {
		t.Fatalf("白名单未命中时不应注入合成工具: %s", cap.raw())
	}
}

// TestForcedToolE2E_PrefixMatchesTransform 交叉校验测试内独立书写的前缀与 transform 包一致。
func TestForcedToolE2E_PrefixMatchesTransform(t *testing.T) {
	if transform.ForcedToolNamePrefix != forcedE2EToolPrefix {
		t.Fatalf("前缀不一致: transform=%q, 测试=%q", transform.ForcedToolNamePrefix, forcedE2EToolPrefix)
	}
}

// TestPrepareForcedTool_FamilyGate 家族闸门：图像/音频家族不得被置入强制工具上下文。
// 否则 executePipelineChat 会因 req.ForcedToolName 非空把非流式入口错误改道到真流式引擎
// （该函数被三家族共用，只能靠 IsAntiTruncate/ForcedToolName 门控）。
func TestPrepareForcedTool_FamilyGate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ForcedToolCallMode = config.ForcedModeRequired
	provider := config.StaticProvider(cfg)
	h := &handler{cfg: provider}

	reqBody := func() *transform.GeminiRequest {
		return &transform.GeminiRequest{
			Contents: []transform.Content{{Role: "user", Parts: []transform.Part{{Text: "hi"}}}},
		}
	}

	t.Run("文本模型应置位", func(t *testing.T) {
		req := reqBody()
		h.prepareForcedTool(transform.ResolveModel("gemini-3.7-flash", provider), req)
		if req.ForcedToolName == "" {
			t.Fatal("文本模型应被置入强制工具上下文")
		}
		if !strings.HasPrefix(req.ForcedToolName, forcedE2EToolPrefix) {
			t.Fatalf("工具名前缀不符: %q", req.ForcedToolName)
		}
		if req.ForcedToolMode != config.ForcedModeRequired {
			t.Fatalf("模式不符: %q", req.ForcedToolMode)
		}
	})

	for _, model := range []string{"gemini-3.1-flash-image", "gemini-3.1-flash-tts-preview"} {
		t.Run("非文本模型应跳过: "+model, func(t *testing.T) {
			req := reqBody()
			h.prepareForcedTool(transform.ResolveModel(model, provider), req)
			if req.ForcedToolName != "" {
				t.Fatalf("非文本模型不得置入强制工具上下文: %q", req.ForcedToolName)
			}
		})
	}
}
