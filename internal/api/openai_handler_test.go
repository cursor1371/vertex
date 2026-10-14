package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// 本文件是 /v1/chat/completions 端点的端到端测试，重点守护三条跨网关契约：
//  1. 流式响应必须以 "data: [DONE]" 收尾（new-api 等中继网关据此判定流结束）；
//  2. 心跳受 openai_sse_heartbeat_seconds 控制，关闭时不得出现；
//  3. /v1/models 是双协议混响（models + data 并存），/v1beta/models 保持纯
//     Gemini——协议污染由 gemini_routing_test.go 与本文件共同守护。

// openaiTextStreamingUpstream 返回一个延迟分帧的流式上游 mock，
// 使心跳有机会在帧间触发（用于验证心跳确实写入流中）。
func openaiTextStreamingUpstream(chunks []string, gap time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i, text := range chunks {
			last := i == len(chunks)-1
			fr := ""
			if last {
				fr = "STOP"
			}
			payload := `{"candidates":[{"content":{"parts":[{"text":"` + text + `"}],"role":"model"},"finishReason":"` + fr + `"}]}`
			_, _ = w.Write([]byte(`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` + payload + `}}}]}]` + "\n"))
			if flusher != nil {
				flusher.Flush()
			}
			if !last {
				time.Sleep(gap)
			}
		}
	}
}

func TestOpenAIEndpoint_ChatCompletionsNonStream(t *testing.T) {
	fx := newTestServer(t)
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"你好"}]}`
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out transform.OpenAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != transform.OpenAIObjectComplete {
		t.Errorf("object = %q, want %q", out.Object, transform.OpenAIObjectComplete)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Errorf("id 应为 chatcmpl- 前缀，实得 %q", out.ID)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "Hello! How can I help you today?" {
		t.Errorf("content = %v, want mock 文本", out.Choices[0].Message.Content)
	}
	if out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", out.Choices[0].FinishReason)
	}
	// usage 必须有值（本地估算兜底）：上游 mock 回了 usageMetadata，应取精确值。
	if out.Usage == nil {
		t.Fatal("usage 不应为 nil")
	}
	if out.Usage.PromptTokens != 10 || out.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want prompt=10 completion=20（上游精确值）", out.Usage)
	}
}

func TestOpenAIEndpoint_StreamEndsWithDone(t *testing.T) {
	fx := newTestServerCustomMock(t, openaiTextStreamingUpstream([]string{"你好", "世界"}, 10*time.Millisecond),
		func(c *config.AppConfig) {
			c.MaxRetries = 0
			c.ParallelPoolEnabled = false
			c.AggregateStream = false
		})

	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	raw := readAllString(t, resp)
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Errorf("流式响应必须以 data: [DONE] 收尾，实得结尾: %q", truncateTail(raw, 80))
	}
	// 至少应有一帧 data:
	if !strings.Contains(raw, "data: {") {
		t.Errorf("未收到任何 data 帧: %q", truncateTail(raw, 200))
	}
	// 每帧必须是合法 JSON
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(strings.TrimPrefix(line, "data: ")) == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			t.Errorf("data 帧不是合法 JSON: %q (%v)", line, err)
		}
	}
}

func TestOpenAIEndpoint_StreamFirstChunkHasRoleAndFinish(t *testing.T) {
	fx := newTestServerCustomMock(t, openaiTextStreamingUpstream([]string{"你好"}, 5*time.Millisecond),
		func(c *config.AppConfig) {
			c.MaxRetries = 0
			c.ParallelPoolEnabled = false
			c.AggregateStream = false
		})

	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	frames := parseSSEDataFrames(t, resp)
	if len(frames) == 0 {
		t.Fatal("未收到任何 data 帧")
	}
	// 首帧必须携带 role: assistant
	var first transform.OpenAIChatResponse
	if err := json.Unmarshal([]byte(frames[0]), &first); err != nil {
		t.Fatalf("首帧解析失败: %v (%q)", err, frames[0])
	}
	if len(first.Choices) != 1 || first.Choices[0].Delta == nil || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("首帧 delta 应携带 role=assistant: %+v", first.Choices[0].Delta)
	}
	// 终帧（[DONE] 之前）应带 finish_reason=stop
	last := frames[len(frames)-1]
	var lastObj transform.OpenAIChatResponse
	if err := json.Unmarshal([]byte(last), &lastObj); err != nil {
		t.Fatalf("终帧解析失败: %v (%q)", err, last)
	}
	if len(lastObj.Choices) == 0 || lastObj.Choices[0].FinishReason == nil || *lastObj.Choices[0].FinishReason != "stop" {
		t.Errorf("终帧应带 finish_reason=stop: %+v", lastObj.Choices)
	}
}

func TestOpenAIEndpoint_HeartbeatDisabledByDefault(t *testing.T) {
	fx := newTestServerCustomMock(t, openaiTextStreamingUpstream([]string{"a", "b"}, 60*time.Millisecond),
		func(c *config.AppConfig) {
			c.MaxRetries = 0
			c.ParallelPoolEnabled = false
			c.AggregateStream = false
			c.OpenAISSEHeartbeatSeconds = 0 // 显式关闭（默认即 0）
		})

	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	raw := readAllString(t, resp)
	if strings.Contains(raw, ": ping") {
		t.Errorf("心跳关闭时不应出现 : ping，实得: %q", truncateTail(raw, 200))
	}
}

func TestOpenAIEndpoint_HeartbeatWrittenWhenEnabled(t *testing.T) {
	// 直接验证写出器行为：心跳开启后确实写入注释帧，且 stop 后停表。
	rec := &recordingResponseWriter{header: http.Header{}}
	sw := newOpenAISSEWriter(rec)
	sw.startHeartbeat(20 * time.Millisecond)
	sw.writeRaw("data: {}\n\n")
	time.Sleep(120 * time.Millisecond)
	sw.stopHeartbeat()
	got := rec.String()

	if !strings.Contains(got, ": ping") {
		t.Errorf("心跳开启后应写入 : ping，实得: %q", got)
	}
	if !strings.Contains(got, "data: {}") {
		t.Errorf("正常帧不应被心跳冲掉: %q", got)
	}
}

func TestOpenAIEndpoint_HeartbeatStopsOnClientGone(_ *testing.T) {
	rec := &recordingResponseWriter{header: http.Header{}, fail: true}
	sw := newOpenAISSEWriter(rec)
	sw.startHeartbeat(10 * time.Millisecond)
	// 客户端断开后心跳 goroutine 应自行退出，不无限循环。
	time.Sleep(80 * time.Millisecond)
	sw.stopHeartbeat() // 幂等，重复调用不应 panic
	sw.stopHeartbeat()
}

func TestOpenAIEndpoint_ModelsListIsOpenAIShape(t *testing.T) {
	fx := newTestServer(t)
	req, _ := http.NewRequest("GET", fx.server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list transform.OpenAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) == 0 {
		t.Fatal("data 数组不应为空")
	}
	for _, m := range list.Data {
		if m.Object != "model" {
			t.Errorf("条目 object = %q, want model", m.Object)
		}
		if m.ID == "" {
			t.Errorf("条目缺少 id: %+v", m)
		}
	}
}

// TestOpenAIEndpoint_RoutingInvariant 守护协议边界：
// /v1/models 是双协议混响——models（Gemini）与 data（OpenAI）并存且模型集合一致；
// /v1beta/models 保持纯 Gemini，出现 data 即协议污染。
func TestOpenAIEndpoint_RoutingInvariant(t *testing.T) {
	fx := newTestServer(t)

	// /v1/models：混响，两套字段都齐全且集合一致
	reqV1, _ := http.NewRequest("GET", fx.server.URL+"/v1/models", nil)
	reqV1.Header.Set("Authorization", "Bearer sk-test-key")
	respV1, err := http.DefaultClient.Do(reqV1)
	if err != nil {
		t.Fatalf("v1 models: %v", err)
	}
	defer respV1.Body.Close()
	var v1Raw map[string]any
	if err := json.NewDecoder(respV1.Body).Decode(&v1Raw); err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if v1Raw["object"] != "list" {
		t.Errorf("混响 object = %v, want list", v1Raw["object"])
	}
	modelsArr, ok := v1Raw["models"].([]any)
	if !ok || len(modelsArr) == 0 {
		t.Fatalf("混响响应缺少非空 Gemini models 数组: %v", v1Raw)
	}
	dataArr, ok := v1Raw["data"].([]any)
	if !ok || len(dataArr) == 0 {
		t.Fatalf("混响响应缺少非空 OpenAI data 数组: %v", v1Raw)
	}
	geminiNames := map[string]bool{}
	for _, entry := range modelsArr {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("Gemini 模型条目必须是对象: %v", entry)
		}
		name, _ := obj["name"].(string)
		geminiNames[strings.TrimPrefix(name, "models/")] = true
	}
	for _, entry := range dataArr {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("OpenAI 模型条目必须是对象: %v", entry)
		}
		id, _ := obj["id"].(string)
		if id == "" {
			t.Errorf("OpenAI 模型条目缺少 id: %v", obj)
			continue
		}
		if !geminiNames[id] {
			t.Errorf("data 条目 %q 不在 models 集合中，两套清单漂移", id)
		}
	}

	// /v1beta/models：纯 Gemini，不得出现 data
	reqBeta, _ := http.NewRequest("GET", fx.server.URL+"/v1beta/models?key=sk-test-key", nil)
	respBeta, err := http.DefaultClient.Do(reqBeta)
	if err != nil {
		t.Fatalf("v1beta models: %v", err)
	}
	defer respBeta.Body.Close()
	var betaRaw map[string]any
	if err := json.NewDecoder(respBeta.Body).Decode(&betaRaw); err != nil {
		t.Fatalf("decode v1beta: %v", err)
	}
	if _, ok := betaRaw["models"]; !ok {
		t.Errorf("Gemini 模型清单缺少 models 字段: %v", betaRaw)
	}
	if _, ok := betaRaw["data"]; ok {
		t.Errorf("/v1beta 应保持纯 Gemini，不应出现 OpenAI 的 data 字段: %v", betaRaw)
	}
}

// TestOpenAIEndpoint_ModelInfoHybrid 守护单模型详情的双协议行为：
// /v1/models/{id} 混响（同时含 Gemini 与 OpenAI 字段）；/v1beta/models/{id} 纯 Gemini。
func TestOpenAIEndpoint_ModelInfoHybrid(t *testing.T) {
	fx := newTestServer(t)

	reqV1, _ := http.NewRequest("GET", fx.server.URL+"/v1/models/gemini-2.5-flash", nil)
	reqV1.Header.Set("Authorization", "Bearer sk-test-key")
	respV1, err := http.DefaultClient.Do(reqV1)
	if err != nil {
		t.Fatalf("v1 model info: %v", err)
	}
	defer respV1.Body.Close()
	var v1Raw map[string]any
	if err := json.NewDecoder(respV1.Body).Decode(&v1Raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// OpenAI 字段
	if v1Raw["id"] != "gemini-2.5-flash" || v1Raw["object"] != "model" {
		t.Errorf("混响详情缺少 OpenAI 字段: %v", v1Raw)
	}
	// Gemini 字段
	if v1Raw["name"] != "models/gemini-2.5-flash" {
		t.Errorf("混响详情缺少 Gemini name 字段: %v", v1Raw)
	}
	if _, ok := v1Raw["supportedGenerationMethods"]; !ok {
		t.Errorf("混响详情缺少 supportedGenerationMethods: %v", v1Raw)
	}

	reqBeta, _ := http.NewRequest("GET", fx.server.URL+"/v1beta/models/gemini-2.5-flash?key=sk-test-key", nil)
	respBeta, err := http.DefaultClient.Do(reqBeta)
	if err != nil {
		t.Fatalf("v1beta model info: %v", err)
	}
	defer respBeta.Body.Close()
	var betaRaw map[string]any
	if err := json.NewDecoder(respBeta.Body).Decode(&betaRaw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if betaRaw["name"] != "models/gemini-2.5-flash" {
		t.Errorf("v1beta 详情应有 Gemini name: %v", betaRaw)
	}
	if _, ok := betaRaw["id"]; ok {
		t.Errorf("/v1beta 单模型详情应保持纯 Gemini，不应出现 id 字段: %v", betaRaw)
	}
}

func TestOpenAIEndpoint_RequiresAPIKey(t *testing.T) {
	fx := newTestServer(t)
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无密钥访问应返 401，实得 %d", resp.StatusCode)
	}
	var errResp transform.OpenAIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Message == "" {
		t.Errorf("错误响应缺少 message: %+v", errResp)
	}
}

func TestOpenAIEndpoint_RejectsMissingModel(t *testing.T) {
	fx := newTestServer(t)
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 model 应返 400，实得 %d", resp.StatusCode)
	}
	var errResp transform.OpenAIErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Type != transform.OpenAIErrTypeInvalidRequest {
		t.Errorf("错误 type = %q, want %q", errResp.Error.Type, transform.OpenAIErrTypeInvalidRequest)
	}
	if errResp.Error.Param != "model" {
		t.Errorf("错误 param = %q, want model", errResp.Error.Param)
	}
}

func TestOpenAIEndpoint_RejectsN(t *testing.T) {
	fx := newTestServer(t)
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-2.5-flash","n":3,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("n>1 应返 400，实得 %d", resp.StatusCode)
	}
}

func TestOpenAIEndpoint_RejectsNonTextModel(t *testing.T) {
	fx := newTestServer(t)
	req, _ := http.NewRequest("POST", fx.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-3.1-flash-image","messages":[{"role":"user","content":"画只猫"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("生图模型走 chat/completions 应返 400，实得 %d", resp.StatusCode)
	}
}

func TestOpenAIEndpoint_MethodNotAllowed(t *testing.T) {
	fx := newTestServer(t)
	req, _ := http.NewRequest("GET", fx.server.URL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返 405，实得 %d", resp.StatusCode)
	}
}

// ---- 辅助 ----

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		sb.WriteString(sc.Text())
		sb.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return sb.String()
}

// parseSSEDataFrames 抽取所有 "data: " 帧的载荷（剔除 [DONE]）。
func parseSSEDataFrames(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		out = append(out, payload)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return out
}

func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// recordingResponseWriter 是最小 http.ResponseWriter 桩：记录写出的字节，
// fail=true 时模拟客户端断开（Write 返回错误）。
type recordingResponseWriter struct {
	header http.Header
	body   strings.Builder
	mu     sync.Mutex
	fail   bool
	status int
}

func (r *recordingResponseWriter) Header() http.Header { return r.header }

func (r *recordingResponseWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return 0, errors.New("client gone")
	}
	r.body.Write(p)
	return len(p), nil
}

func (r *recordingResponseWriter) WriteHeader(status int) { r.status = status }

func (r *recordingResponseWriter) Flush() {}

func (r *recordingResponseWriter) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}
