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

// antiTruncateUpstreamMock 以指定 finishReason 回包单帧文本响应。
// finishReason 传空串即模拟"干净 EOF 但无终帧"的截断形态。
func antiTruncateUpstreamMock(finishReason string, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		finishField := ""
		if finishReason != "" {
			finishField = fmt.Sprintf(`,"finishReason":%q`, finishReason)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":`+
			`{"candidates":[{"content":{"parts":[{"text":%q}],"role":"model"},"index":0%s}]}}}}]}]`, text, finishField)
	}
}

// antiTruncateResponse 解析客户端非流式响应，返回文本与是否存在 finishReason。
func antiTruncateResponse(t *testing.T, resp *http.Response) (string, string) {
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
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, string(raw))
	}
	var sb strings.Builder
	finish := ""
	for _, c := range parsed.Candidates {
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		for _, p := range c.Content.Parts {
			sb.WriteString(p.Text)
		}
	}
	return sb.String(), finish
}

// TestAntiTruncateVariant_CompleteResponse 抗截断变体的正常路径：
// 变体名可解析、请求成功、内容与 finishReason 均正常透出。
func TestAntiTruncateVariant_CompleteResponse(t *testing.T) {
	f := newTestServerCustomMock(t, antiTruncateUpstreamMock("STOP", "抗截断正常正文"), nil)

	resp := doPost(t, f.server.URL+"/v1beta/models/抗截断-gemini-3.7-flash:generateContent", "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	text, finish := antiTruncateResponse(t, resp)
	if text != "抗截断正常正文" {
		t.Fatalf("正文不符: %q", text)
	}
	if finish != "STOP" {
		t.Fatalf("完整流应保留 finishReason=STOP: %q", finish)
	}
}

// TestAntiTruncateVariant_DeliversPartialWithoutFakeStop 抗截断变体的核心契约：
// 上游干净 EOF 但无终帧时仍交付已收到的内容，且绝不伪造 finishReason=STOP。
func TestAntiTruncateVariant_DeliversPartialWithoutFakeStop(t *testing.T) {
	f := newTestServerCustomMock(t, antiTruncateUpstreamMock("", "被截断前的内容"), nil)

	resp := doPost(t, f.server.URL+"/v1beta/models/抗截断-gemini-3.7-flash:generateContent", "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	text, finish := antiTruncateResponse(t, resp)
	if text != "被截断前的内容" {
		t.Fatalf("截断时应交付已收到的内容: %q", text)
	}
	if finish != "" {
		t.Fatalf("不得伪造 finishReason（必须留空以让客户端区分截断）: %q", finish)
	}
}

// TestAntiTruncateVariant_ResolveModelFlags 变体解析：置位、组合形态、非文本模型不置位。
func TestAntiTruncateVariant_ResolveModelFlags(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())

	cases := []struct {
		rawModel  string
		wantModel string
		wantFake  bool
		wantAnti  bool
	}{
		{rawModel: "抗截断-gemini-3.7-flash", wantModel: "gemini-3.7-flash", wantAnti: true},
		{rawModel: "假非流-gemini-3.7-flash", wantModel: "gemini-3.7-flash", wantFake: true},
		{rawModel: "假非流-抗截断-gemini-3.7-flash", wantModel: "gemini-3.7-flash", wantFake: true, wantAnti: true},
		{rawModel: "抗截断-假非流-gemini-3.7-flash", wantModel: "gemini-3.7-flash", wantFake: true, wantAnti: true},
		// 重复前缀每种至多剥离一次，剩余前缀成为模型名的一部分（不误吞）。
		{rawModel: "抗截断-抗截断-gemini-3.7-flash", wantModel: "抗截断-gemini-3.7-flash", wantAnti: true},
		// 非文本模型不置位任何变体修饰。
		{rawModel: "抗截断-gemini-3.1-flash-image", wantModel: "gemini-3.1-flash-image"},
		{rawModel: "假非流-gemini-3.1-flash-tts-preview", wantModel: "gemini-3.1-flash-tts-preview"},
	}
	for _, c := range cases {
		t.Run(c.rawModel, func(t *testing.T) {
			got := transform.ResolveModel(c.rawModel, cfg)
			if got.ActualModel != c.wantModel {
				t.Errorf("ActualModel=%q, want %q", got.ActualModel, c.wantModel)
			}
			if got.IsFake != c.wantFake {
				t.Errorf("IsFake=%v, want %v", got.IsFake, c.wantFake)
			}
			if got.IsAntiTruncate != c.wantAnti {
				t.Errorf("IsAntiTruncate=%v, want %v", got.IsAntiTruncate, c.wantAnti)
			}
		})
	}
}
