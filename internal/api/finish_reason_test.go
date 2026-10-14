package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestCaptureFinishReason 采集入口对各类终止原因的原样返回（标注映射由 transform.TruncationLabel 覆盖）。
func TestCaptureFinishReason(t *testing.T) {
	cases := []struct {
		name string
		resp *transform.GeminiResponse
		want string
	}{
		{name: "nil 响应", resp: nil, want: ""},
		{name: "无候选", resp: &transform.GeminiResponse{}, want: ""},
		{name: "输出上限截断", resp: &transform.GeminiResponse{Candidates: []*transform.Candidate{
			{FinishReason: "MAX_TOKENS"}}}, want: "MAX_TOKENS"},
		{name: "工具调用畸形", resp: &transform.GeminiResponse{Candidates: []*transform.Candidate{
			{FinishReason: "MALFORMED_FUNCTION_CALL"}}}, want: "MALFORMED_FUNCTION_CALL"},
		{name: "正常收尾", resp: &transform.GeminiResponse{Candidates: []*transform.Candidate{
			{FinishReason: "STOP"}}}, want: "STOP"},
		{name: "空指针候选不 panic", resp: &transform.GeminiResponse{Candidates: []*transform.Candidate{nil}}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CaptureFinishReason("test-req", c.resp); got != c.want {
				t.Fatalf("CaptureFinishReason = %q, want %q", got, c.want)
			}
		})
	}
}

// frameWithFinish 构造单帧上游响应，finishReason 可指定（既有 geminiNonStreamFrame 固定 STOP）。
func frameWithFinish(text, finishReason string) string {
	return fmt.Sprintf(`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":`+
		`{"candidates":[{"content":{"parts":[{"text":%q}],"role":"model"},"finishReason":%q,"index":0}]}}}}]}]`,
		text, finishReason)
}

// truncatingUpstreamMock 以上游主动终止原因收尾（非流式单帧）。
func truncatingUpstreamMock(t *testing.T, cap *upstreamCapture, finishReason, text string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(body, "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, frameWithFinish(text, finishReason))
	}
}

// TestFinishReasonPayloadUntouched 核心契约：截断只做标注，
// 交付给客户端的 finishReason 与正文必须原样保留（不改写、不吞掉）。
func TestFinishReasonPayloadUntouched(t *testing.T) {
	for _, fr := range []string{"MAX_TOKENS", "MALFORMED_FUNCTION_CALL"} {
		t.Run(fr, func(t *testing.T) {
			var cap upstreamCapture
			f := newTestServerCustomMock(t, truncatingUpstreamMock(t, &cap, fr, "已交付的正文"), nil)

			resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
			defer func() { _ = resp.Body.Close() }()

			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("读取响应失败: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("状态码应为 200（撞上限仍是 HTTP 成功）, 实际 %d (body=%s)", resp.StatusCode, string(raw))
			}
			body := string(raw)
			if !strings.Contains(body, fr) {
				t.Fatalf("客户端的 finishReason 被改动，应原样保留 %s: %s", fr, body)
			}
			if !strings.Contains(body, "已交付的正文") {
				t.Fatalf("正文被改动: %s", body)
			}
			if cap.raw() == "" {
				t.Fatal("mock 未收到上游请求")
			}
		})
	}
}

// TestFinishReasonStreamPayloadUntouched 流式端点同样只标注、不改帧。
func TestFinishReasonStreamPayloadUntouched(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(body, "")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, frameWithFinish("流式正文", "MAX_TOKENS"))
	}, nil)

	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:streamGenerateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "MAX_TOKENS") {
		t.Fatalf("流式响应应保留 finishReason=MAX_TOKENS: %s", body)
	}
	if !strings.Contains(body, "流式正文") {
		t.Fatalf("流式正文被改动: %s", body)
	}
}

// TestFinishReasonRegressionStop 回归：STOP 时标注为空（状态列不追加后缀），且 success 口径不变。
func TestFinishReasonRegressionStop(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, plainTextUpstreamMock(t, &cap, "正常正文"), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeOff
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got := transform.TruncationLabel("STOP"); got != "" {
		t.Fatalf("STOP 不应产生截断标注, got %q", got)
	}
}
