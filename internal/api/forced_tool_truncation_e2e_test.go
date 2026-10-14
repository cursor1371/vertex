package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// forcedArgsFrame 构造一帧携带合成工具调用的上游响应。
// 流式端点的每帧是独立 JSON 对象（data.candidates 扁平形态，与真实上游一致）。
func forcedArgsFrame(t *testing.T, name, argsJSON string, finishReason string) string {
	t.Helper()
	finish := ""
	if finishReason != "" {
		finish = `,"finishReason":"` + finishReason + `"`
	}
	return `{"results":[{"data":{"candidates":[{"index":0` + finish +
		`,"content":{"role":"model","parts":[{"functionCall":{"name":"` + name + `","args":` + argsJSON + `}}]}}]}}]}`
}

// snapshotArgsUpstream 模拟上游对 tool-call args 采用"快照式增量下发"：
// 每帧都是"到目前为止的完整对象"，后者覆盖前者。这正是会触发正文截断的场景。
func snapshotArgsUpstream(t *testing.T, cap *upstreamCapture, snapshots []string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取上游请求体失败: %v", err)
			return
		}
		vars := variablesOf(t, body)
		name := syntheticToolNameFrom(t, vars)
		cap.record(body, name)
		if name == "" {
			t.Errorf("上游请求未包含合成工具声明")
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, snap := range snapshots {
			// 快照内容以 JSON 字符串形式给出，模拟 args.content 的逐步增长。
			_, _ = io.WriteString(w, forcedArgsFrame(t, name, `{"content":`+snap+`}`, ""))
			_, _ = io.WriteString(w, "\n")
		}
		// 终帧：空 parts + STOP。
		_, _ = io.WriteString(w, `{"results":[{"data":{"candidates":[{"index":0,"finishReason":"STOP",`+
			`"content":{"role":"model","parts":[]}}]}}]}`+"\n")
	}
}

// TestForcedToolTruncation_NonStreamFullText 非流式：上游快照式增量下发时，
// 客户端必须拿到最后一份完整快照（而非被截断成第一段）。
func TestForcedToolTruncation_NonStreamFullText(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, snapshotArgsUpstream(t, &cap, []string{
		`"第一段"`, `"第一段，第二段"`, `"第一段，第二段，第三段"`,
	}), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()

	if got := geminiTextResponse(t, resp); got != "第一段，第二段，第三段" {
		t.Fatalf("客户端应收到完整正文（最后一份快照）, 实际: %q", got)
	}
}

// TestForcedToolTruncation_StreamFullText 流式：快照式增量下发时，
// 客户端累积收到的各帧增量应还原出完整正文，不得中途停止。
func TestForcedToolTruncation_StreamFullText(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, snapshotArgsUpstream(t, &cap, []string{
		`"第一段"`, `"第一段，第二段"`, `"第一段，第二段，第三段"`,
	}), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
	})

	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:streamGenerateContent",
		"sk-test-key", simpleUserBody())
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

	// 把各帧的 text 依次拼起来，应等于完整正文。
	var sb strings.Builder
	for _, frame := range strings.Split(body, "data: ") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var parsed struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(frame), &parsed); err != nil {
			continue
		}
		for _, c := range parsed.Candidates {
			for _, p := range c.Content.Parts {
				sb.WriteString(p.Text)
			}
		}
	}
	if got := sb.String(); got != "第一段，第二段，第三段" {
		t.Fatalf("客户端累积文本应还原完整正文, 实际: %q (raw=%s)", got, body)
	}
	if !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("流式响应丢失 finishReason: %s", body)
	}
}

// TestForcedToolTruncation_MidJSONCut 上游在 JSON 中途被切断：
// 必须有明确信号，不得静默交付空内容让客户端一直等。
func TestForcedToolTruncation_MidJSONCut(t *testing.T) {
	cutUpstream := func(cap *upstreamCapture) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			name := syntheticToolNameFrom(t, variablesOf(t, body))
			cap.record(body, name)

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			// 只写半截 JSON（引号/花括号均未闭合）后直接结束。
			_, _ = io.WriteString(w, `{"results":[{"data":{"candidates":[{"index":0,`+
				`"content":{"role":"model","parts":[{"functionCall":{"name":"`+name+`","args":{"content":"被切断的正文`)
		}
	}

	var cap upstreamCapture
	f := newTestServerCustomMock(t, cutUpstream(&cap), func(c *config.AppConfig) {
		c.ForcedToolCallMode = config.ForcedModeRequired
		c.StreamIdleTimeoutSeconds = 1
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp := doPost(t, forcedGenerateURL(f), "sk-test-key", simpleUserBody())
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		// 不得交付"看起来正常"的空响应；必须带错误或截断语义。
		if resp.StatusCode == http.StatusOK && !strings.Contains(string(raw), "error") &&
			!strings.Contains(string(raw), "finishReason") {
			t.Errorf("JSON 中途切断应给出错误信号，实际: %s", string(raw))
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("请求未在预期时间内结束（可能挂死）")
	}
}
