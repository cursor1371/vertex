package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeDoer 是 catalog.Doer 的测试替身：按 handler 返回响应并记录调用。
type fakeDoer struct {
	handler func(method, url string, body []byte) (int, []byte)
	calls   []string
	bodies  []string
	mu      sync.Mutex
}

func (f *fakeDoer) Do(_ context.Context, method, url string, _ http.Header, body []byte) (int, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+url)
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	status, resp := f.handler(method, url, body)
	return status, resp, nil
}

func (f *fakeDoer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// catalogPageJSON 构造一页 ListPublisherModel 响应。
func catalogPageJSON(ids []string, nextToken string) []byte {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, `{"name":"publishers/google/models/`+id+`"}`)
	}
	return []byte(`[{"results":[{"data":{"ui":{"listPublisherModel":{"data":[` +
		strings.Join(items, ",") + `],"nextPageToken":"` + nextToken + `"}}}}]}]`)
}

func TestFetchPaginationAndContract(t *testing.T) {
	page := 0
	doer := &fakeDoer{handler: func(_ string, _ string, _ []byte) (int, []byte) {
		page++
		if page == 1 {
			return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash", "gemini-3.7-flash"}, "TOKEN2")
		}
		return http.StatusOK, catalogPageJSON([]string{"gemini-3.7-flash", "veo-3.0-generate-preview"}, "")
	}}

	res, err := Fetch(context.Background(), doer, "https://example.invalid/batchGraphql?key=k", "SIG")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Pages != 2 {
		t.Fatalf("Pages=%d, want 2", res.Pages)
	}
	want := []string{"gemini-3.6-flash", "gemini-3.7-flash", "veo-3.0-generate-preview"}
	if len(res.All) != len(want) {
		t.Fatalf("All=%v, want %v（跨页去重）", res.All, want)
	}
	for i := range want {
		if res.All[i] != want[i] {
			t.Fatalf("All=%v, want %v", res.All, want)
		}
	}

	// 请求体契约：operationName/querySignature/variables 与 bat 一致；第二页带 pageToken。
	var b1, b2 map[string]any
	if uerr := json.Unmarshal([]byte(doer.bodies[0]), &b1); uerr != nil {
		t.Fatalf("解析第一页请求体: %v", uerr)
	}
	if uerr := json.Unmarshal([]byte(doer.bodies[1]), &b2); uerr != nil {
		t.Fatalf("解析第二页请求体: %v", uerr)
	}
	if b1["operationName"] != "ListPublisherModel" || b1["querySignature"] != "SIG" {
		t.Fatalf("请求体契约不符: %v", b1)
	}
	v1, _ := b1["variables"].(map[string]any)
	if v1["parent"] != Parent || v1["pageSize"] != float64(PageSize) {
		t.Fatalf("variables 不符: %v", v1)
	}
	if _, ok := v1["pageToken"]; ok {
		t.Fatalf("第一页不应携带 pageToken: %v", v1)
	}
	v2, _ := b2["variables"].(map[string]any)
	if v2["pageToken"] != "TOKEN2" {
		t.Fatalf("第二页应携带 pageToken: %v", v2)
	}
}

func TestFetchUpstreamErrorPropagatesMessage(t *testing.T) {
	doer := &fakeDoer{handler: func(_ string, _ string, _ []byte) (int, []byte) {
		return http.StatusOK, []byte(`[{"results":[{"errors":[{"message":"Query signature is invalid"}]}]}]`)
	}}
	_, err := Fetch(context.Background(), doer, "https://example.invalid/", "SIG")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "Query signature is invalid") {
		t.Fatalf("错误应包含上游 message: %v", err)
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Page != 1 {
		t.Fatalf("应为第 1 页的 UpstreamError: %v", err)
	}
	if !IsSignatureError(err.Error()) {
		t.Fatalf("签名失效判据未命中: %v", err)
	}
}

func TestFetchIncompleteWhenMaxPagesExceeded(t *testing.T) {
	doer := &fakeDoer{handler: func(_ string, _ string, _ []byte) (int, []byte) {
		return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash"}, "ALWAYS-MORE")
	}}
	_, err := Fetch(context.Background(), doer, "https://example.invalid/", "SIG")
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("翻页触顶应报不完整错误: %v", err)
	}
	if n := doer.callCount(); n != MaxPages {
		t.Fatalf("应在 MaxPages 次请求后停止: %d", n)
	}
}

func TestFetchCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := &fakeDoer{handler: func(_ string, _ string, _ []byte) (int, []byte) {
		t.Fatal("取消后不应发起请求")
		return 0, nil
	}}
	if _, err := Fetch(ctx, doer, "https://example.invalid/", "SIG"); err == nil {
		t.Fatal("已取消的 ctx 应报错")
	}
}

func TestFilterCatalog(t *testing.T) {
	all := []string{
		"gemini-3.6-flash",
		"publishers-not-generation",       // 非 gemini/veo：剔除
		"veo-3.0-generate-preview",        // 视频：纳入目录
		"gemini-embedding-001",            // embedding：剔除
		"gemini-2.0-flash-live-001",       // -live：剔除
		"gemini-3.1-flash-tts-preview",    // TTS：保留（audio 家族）
		"gemini-3.6-flash",                // 重复：去重
		"gemini-robotics-something",       // robotics：剔除
		"veo-2.0-generate-001-transcribe", // transcribe：剔除
	}
	kept, skipped := FilterCatalog(all)
	want := []string{"gemini-3.1-flash-tts-preview", "gemini-3.6-flash", "veo-3.0-generate-preview"}
	if len(kept) != len(want) {
		t.Fatalf("kept=%v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("kept=%v, want %v（稳定排序）", kept, want)
		}
	}
	// skipped 只统计"非生成式/被模式剔除"的条目；跨页或页内重复只去重、不计数。
	if skipped != 5 {
		t.Fatalf("skipped=%d, want 5", skipped)
	}
}

func TestComputeDiff(t *testing.T) {
	local := []string{"gemini-3.6-flash", "gemini-gone", "gemini-4-argon"}
	upstream := []string{"gemini-3.6-flash", "gemini-brand-new"}
	d := ComputeDiff(local, upstream)
	if len(d.New) != 1 || d.New[0] != "gemini-brand-new" {
		t.Fatalf("New=%v", d.New)
	}
	if len(d.Existing) != 1 || d.Existing[0] != "gemini-3.6-flash" {
		t.Fatalf("Existing=%v", d.Existing)
	}
	if len(d.Removed) != 2 || d.Removed[0] != "gemini-4-argon" || d.Removed[1] != "gemini-gone" {
		t.Fatalf("Removed=%v（稳定排序）", d.Removed)
	}
}

func TestIsSuspicious(t *testing.T) {
	if IsSuspicious(0, 0) || IsSuspicious(0, 5) {
		t.Fatal("无历史快照不应判可疑")
	}
	if !IsSuspicious(100, 49) {
		t.Fatal("49 < 50% 应判可疑")
	}
	if IsSuspicious(100, 50) {
		t.Fatal("50 == 50% 不应判可疑")
	}
	if IsSuspicious(200, 100) {
		t.Fatal("100 == 50% 不应判可疑")
	}
	if !IsSuspicious(200, 99) {
		t.Fatal("99 < 50% 应判可疑")
	}
}

func TestIsSignatureError(t *testing.T) {
	cases := map[string]bool{
		"Query signature is invalid": true,
		"Operation not found: X":     true,
		"could not be found":         true,
		"HTTP 500 internal error":    false,
		"rate limit exceeded":        false,
	}
	for msg, want := range cases {
		if got := IsSignatureError(msg); got != want {
			t.Errorf("IsSignatureError(%q)=%v, want %v", msg, got, want)
		}
	}
}

func TestFindFreshSignature(t *testing.T) {
	bundleURL := "https://www.gstatic.com/_/mss/boq-cloud-client/_/js/k=boq-cloud-client.AiPlatformMicroUi.zh_CN.someid/am=1/rs=2"
	// 页面里同时放一个其它 bundle 作为诱饵；并用 \u003d 转义形态覆盖反转义路径。
	html := []byte(`<html>` +
		`<script src="https://www.gstatic.com/_/mss/boq-cloud-client/_/js/k=boq-cloud-client.OtherUi.zh_CN.id/am=1/rs=2"></script>` +
		`<script src="` + bundleURL + `"></script>` +
		`<div data-x="a\u003db"></div></html>`)
	js := []byte(`function X(){return request("ListPublisherModelRegional",{},{},null,@Signature(bytes:"DECOY-SIG"));}` +
		`function Y(){return request("ListPublisherModel",{parent:"publishers/google"},{},null,@Signature(bytes:"FRESH-SIG-123"));}`)
	doer := &fakeDoer{handler: func(_ string, url string, _ []byte) (int, []byte) {
		switch {
		case strings.Contains(url, "console.cloud.google.com"):
			return http.StatusOK, html
		case strings.Contains(url, "gstatic.com"):
			return http.StatusOK, js
		default:
			return http.StatusNotFound, nil
		}
	}}
	sig, err := FindFreshSignature(context.Background(), doer, ConsolePageURL)
	if err != nil {
		t.Fatalf("FindFreshSignature: %v", err)
	}
	if sig != "FRESH-SIG-123" {
		t.Fatalf("sig=%q, want FRESH-SIG-123（不得命中 Regional 前缀的诱饵签名）", sig)
	}
}

func TestClassify(t *testing.T) {
	veo := Classify("veo-3.0-generate-preview")
	if veo.Family != "vedio" || veo.Supported || veo.UnavailableReason == "" {
		t.Fatalf("veo 应归 vedio 且不可用并给出原因: %+v", veo)
	}
	text := Classify("gemini-3.6-flash")
	if text.Family != "text" || !text.Supported {
		t.Fatalf("文本模型应可用: %+v", text)
	}
}
