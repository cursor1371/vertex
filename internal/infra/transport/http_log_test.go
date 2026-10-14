package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRecorder 是测试用 HTTPLogRecorder：收集 Enabled 与所有 Record。
type fakeRecorder struct {
	mu      sync.Mutex
	enabled bool
	bodyCap int
	recs    []HTTPLogRecord
}

func (f *fakeRecorder) Enabled() bool { return f.enabled }
func (f *fakeRecorder) BodyCapBytes() int {
	if f.bodyCap <= 0 {
		return 0
	}
	return f.bodyCap
}
func (f *fakeRecorder) Record(r HTTPLogRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
}
func (f *fakeRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}
func (f *fakeRecorder) first() HTTPLogRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recs) == 0 {
		return HTTPLogRecord{}
	}
	return f.recs[0]
}

// TestHTTPLogCaptureDisabled 验证开关关闭时零捕获（零开销路径）。
func TestHTTPLogCaptureDisabled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: false}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "test-disabled")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, _, err = sess.DoAndRead(context.Background(), "POST", ts.URL, XHRHeaders("application/json", "*/*", "", "", "cross-site"), strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("DoAndRead: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("开关关闭时不应有记录, got %d", rec.count())
	}
}

// TestHTTPLogCaptureEnabled 验证开关开启时捕获完整记录：URL/请求体/响应体/状态/协议/网络元数据。
func TestHTTPLogCaptureEnabled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("key") == "" {
			w.WriteHeader(400)
		} else {
			w.Header().Set("X-Custom", "hdr")
			_, _ = w.Write([]byte("hello:" + string(body)))
		}
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: true}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "req-capture-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	url := ts.URL + "/v3/batchGraphql?key=SECRET&prettyPrint=false"
	status, respBody, err := sess.DoAndRead(context.Background(), "POST", url, XHRHeaders("application/json", "*/*", "", "", "cross-site"), strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("DoAndRead: %v", err)
	}
	if status != 200 {
		t.Fatalf("status=%d, want 200", status)
	}
	if !strings.Contains(string(respBody), "hello:") {
		t.Fatalf("unexpected body: %s", respBody)
	}

	// DoAndRead 已读完 body → loggedBody.finish() 已提交记录。
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("expected 1 record, got %d", rec.count())
	}
	recN := rec.first()
	if recN.RequestID != "req-capture-1" {
		t.Fatalf("RequestID=%q, want req-capture-1", recN.RequestID)
	}
	if !strings.Contains(recN.FinalURL, "key=SECRET") {
		t.Fatalf("FinalURL 未保留 key: %s", recN.FinalURL)
	}
	if !strings.Contains(recN.RequestBody, "hi") {
		t.Fatalf("RequestBody 未捕获: %s", recN.RequestBody)
	}
	if !strings.Contains(recN.ResponseBody, "hello:") {
		t.Fatalf("ResponseBody 未捕获: %s", recN.ResponseBody)
	}
	if recN.StatusCode != 200 || !recN.Success {
		t.Fatalf("status/success 不符: %+v", recN)
	}
	if recN.Scope != "batchgraphql" {
		t.Fatalf("Scope=%q, want batchgraphql", recN.Scope)
	}
	if recN.HTTPVersion == "" {
		t.Fatal("HTTPVersion 未捕获")
	}
	// 本地 httptest 走 HTTP/1.1
	if recN.HTTPVersion != "HTTP/1.1" {
		t.Fatalf("HTTPVersion=%q, want HTTP/1.1", recN.HTTPVersion)
	}
	if recN.PeerIP == "" {
		t.Fatal("PeerIP 未捕获（httptrace）")
	}
}

// TestHTTPLogStreamBodyCapture 验证流式响应体经 tee 累积并在读完时提交。
func TestHTTPLogStreamBodyCapture(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("part1;part2;part3"))
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: true}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "req-stream")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	sr, err := sess.DoStream(context.Background(), "POST", ts.URL, XHRHeaders("application/json", "*/*", "", "", "cross-site"), strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	all, _ := io.ReadAll(sr.Body)
	sr.Close()
	if string(all) != "part1;part2;part3" {
		t.Fatalf("stream body=%q, want part1;part2;part3", string(all))
	}

	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("expected 1 record, got %d", rec.count())
	}
	recN := rec.first()
	if !strings.Contains(recN.ResponseBody, "part3") {
		t.Fatalf("stream 响应体未完整捕获: %s", recN.ResponseBody)
	}
	if !recN.Success {
		t.Fatalf("完整读完的流应标记成功, got %+v", recN)
	}
}

// TestHTTPLogStreamAbortedNotSuccess 验证中途 Abort（未读完 EOF）的流不被误标为成功。
func TestHTTPLogStreamAbortedNotSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for i := 0; i < 100; i++ {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
		}
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: true}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "req-abort")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	sr, err := sess.DoStream(context.Background(), "POST", ts.URL, XHRHeaders("application/json", "*/*", "", "", "cross-site"), strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	// 只读一小部分后直接 Abort（模拟空闲超时/断流路径）。
	buf := make([]byte, 4)
	_, _ = sr.Body.Read(buf)
	sr.Abort()

	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("expected 1 record, got %d", rec.count())
	}
	recN := rec.first()
	if recN.Success {
		t.Fatalf("Abort 的流不应标记成功: %+v", recN)
	}
	if recN.StatusCode != 200 {
		t.Fatalf("abort 流 status=%d, want 200", recN.StatusCode)
	}
}

// TestHTTPLogBodyCap 验证 body 捕获被截断到上限并追加截断标记。
func TestHTTPLogBodyCap(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789ABCDEF"))
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: true, bodyCap: 8}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "req-cap")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	_, _, err = sess.DoAndRead(context.Background(), "POST", ts.URL, XHRHeaders("application/json", "*/*", "", "", "cross-site"), strings.NewReader("abcdefghijklmnop"))
	if err != nil {
		t.Fatalf("DoAndRead: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("expected 1 record, got %d", rec.count())
	}
	recN := rec.first()
	if len(recN.ResponseBody) > 8+len(truncMarker) {
		t.Fatalf("响应体未被截断: len=%d body=%q", len(recN.ResponseBody), recN.ResponseBody)
	}
	if !strings.Contains(recN.ResponseBody, "[truncated]") {
		t.Fatalf("截断标记缺失: %q", recN.ResponseBody)
	}
	if len(recN.RequestBody) > 8+len(truncMarker) {
		t.Fatalf("请求体未被截断: %q", recN.RequestBody)
	}
}

func TestExtractResponseMeta(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		statusCode  int
		wantCode    int
		wantResult  string
		wantErrType string
		wantMsg     string
	}{
		{
			name:        "gemini error object",
			body:        `{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`,
			statusCode:  429,
			wantCode:    429,
			wantResult:  "RESOURCE_EXHAUSTED",
			wantErrType: "RESOURCE_EXHAUSTED",
			wantMsg:     "quota exceeded",
		},
		{
			name:        "gemini error no status uses message",
			body:        `{"error":{"code":400,"message":"invalid argument"}}`,
			statusCode:  400,
			wantCode:    400,
			wantResult:  "invalid argument",
			wantErrType: "invalid argument",
			wantMsg:     "invalid argument",
		},
		{
			name:       "candidates finishReason",
			body:       `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":5}}`,
			statusCode: 200,
			wantCode:   0,
			wantResult: "STOP",
		},
		{
			name:       "candidates no finishReason",
			body:       `{"candidates":[{"content":{"parts":[]}}]}`,
			statusCode: 200,
			wantCode:   0,
			wantResult: "OK",
		},
		{
			name:       "data object success",
			body:       `{"data":{"key":"value"}}`,
			statusCode: 200,
			wantCode:   0,
			wantResult: "OK",
		},
		{
			name:       "non-json 2xx",
			body:       `plain text ok`,
			statusCode: 200,
			wantCode:   0,
			wantResult: "OK",
		},
		{
			name:       "non-json 5xx",
			body:       `gateway timeout`,
			statusCode: 502,
			wantCode:   502,
			wantResult: "gateway timeout",
			wantMsg:    "gateway timeout",
		},
		{
			name:       "empty body 500",
			body:       ``,
			statusCode: 500,
			wantCode:   500,
			wantResult: "",
		},
		{
			name:        "batchgraphql errors",
			body:        `{"results":[{"data":null,"errors":[{"message":"Failed to verify action","errorType":"DATA_FETCHING_EXCEPTION","path":["ui","streamGenerateContentAnonymous"],"extensions":{"status":{"code":3,"message":"Failed to verify action"}}}],"path":["ui","streamGenerateContentAnonymous"]}]}`,
			statusCode:  200,
			wantCode:    3,
			wantResult:  "DATA_FETCHING_EXCEPTION",
			wantErrType: "DATA_FETCHING_EXCEPTION",
			wantMsg:     "Failed to verify action",
		},
		{
			name:        "batchgraphql quota exhausted",
			body:        `{"results":[{"data":null,"errors":[{"message":"Resource has been exhausted (e.g. check quota).","errorType":"DATA_FETCHING_EXCEPTION","path":["ui","streamGenerateContentAnonymous"],"extensions":{"status":{"code":8,"message":"Resource has been exhausted (e.g. check quota)."}}}],"path":["ui","streamGenerateContentAnonymous"]}]}`,
			statusCode:  200,
			wantCode:    8,
			wantResult:  "DATA_FETCHING_EXCEPTION",
			wantErrType: "DATA_FETCHING_EXCEPTION",
			wantMsg:     "Resource has been exhausted (e.g. check quota).",
		},
		{
			name:        "batchgraphql code as string",
			body:        `{"results":[{"data":null,"errors":[{"message":"bad","errorType":"TYPE","extensions":{"status":{"code":"5","message":"bad"}}}]}]}`,
			statusCode:  200,
			wantCode:    5,
			wantResult:  "TYPE",
			wantErrType: "TYPE",
			wantMsg:     "bad",
		},
		{
			name:        "streaming multi-frame last error",
			body:        `[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":[]}},"path":[]}],"responseContext":{"eti":"abc"}},{"results":[{"data":null,"errors":[{"message":"Resource has been exhausted (e.g. check quota).","errorType":"DATA_FETCHING_EXCEPTION","path":["ui","streamGenerateContentAnonymous"],"extensions":{"status":{"code":8,"message":"Resource has been exhausted (e.g. check quota)."}}}],"path":["ui","streamGenerateContentAnonymous"]}]}]`,
			statusCode:  200,
			wantCode:    8,
			wantResult:  "DATA_FETCHING_EXCEPTION",
			wantErrType: "DATA_FETCHING_EXCEPTION",
			wantMsg:     "Resource has been exhausted (e.g. check quota).",
		},
		{
			name:       "streaming multi-frame all success",
			body:       `[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":[]}},"path":[]}],"responseContext":{"eti":"abc"}}]`,
			statusCode: 200,
			wantCode:   0,
			wantResult: "OK",
		},
		{
			name:       "batchgraphql data success",
			body:       `{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":[]}},"path":[]}],"responseContext":{"eti":"x"}}`,
			statusCode: 200,
			wantCode:   0,
			wantResult: "OK",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, result, errType, msg := extractResponseMeta(tc.body, tc.statusCode)
			if code != tc.wantCode || result != tc.wantResult || errType != tc.wantErrType || msg != tc.wantMsg {
				t.Fatalf("got (%d, %q, %q, %q), want (%d, %q, %q, %q)",
					code, result, errType, msg, tc.wantCode, tc.wantResult, tc.wantErrType, tc.wantMsg)
			}
		})
	}
}

// TestRecaptchaScopeSkipsBodyCapture 守卫：recaptcha scope 只记元数据不记正文。
// anchor HTML 含 id="recaptcha-token" value="…"、reload 响应含 rresp token——
// 开启 http_log_enabled 时若照常捕获，有效凭据会落库并可经 admin CSV 导出。
func TestRecaptchaScopeSkipsBodyCapture(t *testing.T) {
	const fakeToken = "recaptcha-token-value-SECRET"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("token=" + fakeToken + " echo=" + string(body)))
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: true, bodyCap: 64}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "req-recaptcha-skip")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	// URL 含 recaptcha → scope=recaptcha → skipBody
	url := ts.URL + "/recaptcha/enterprise/reload"
	_, _, err = sess.DoAndRead(context.Background(), "POST", url,
		XHRHeaders("application/x-www-form-urlencoded", "*/*", "", "", "same-origin"),
		strings.NewReader("v=x&rresp="+fakeToken))
	if err != nil {
		t.Fatalf("DoAndRead: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("expected 1 record, got %d", rec.count())
	}
	r := rec.first()
	// 元数据照常：scope/状态/成功判定都在，正文为空。
	if r.Scope != "recaptcha" || r.StatusCode != 200 || !r.Success {
		t.Fatalf("元数据应照常记录: scope=%q status=%d success=%v", r.Scope, r.StatusCode, r.Success)
	}
	if strings.Contains(r.ResponseBody, fakeToken) || r.ResponseBody != "" {
		t.Fatalf("响应体不得捕获（含有效 token）, got %q", r.ResponseBody)
	}
	if strings.Contains(r.RequestBody, fakeToken) || r.RequestBody != "" {
		t.Fatalf("请求体不得捕获, got %q", r.RequestBody)
	}
}

// TestBatchGraphqlScopeStillCapturesBody 守卫：body 跳过仅限 recaptcha scope，
// batchgraphql 等业务 scope 的正文捕获行为不变。
func TestBatchGraphqlScopeStillCapturesBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("resp:" + string(body)))
	}))
	defer ts.Close()

	rec := &fakeRecorder{enabled: true, bodyCap: 64}
	nc := NewNetworkClient(nil)
	nc.SetHTTPLogRecorder(rec)
	sess, err := nc.CreateSession(5, "", "req-batch-capture")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	url := ts.URL + "/v3/batchGraphql?key=SECRET"
	if _, _, err := sess.DoAndRead(context.Background(), "POST", url,
		XHRHeaders("application/json", "*/*", "", "", "same-origin"),
		strings.NewReader(`{"prompt":"hi"}`)); err != nil {
		t.Fatalf("DoAndRead: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("expected 1 record, got %d", rec.count())
	}
	r := rec.first()
	if r.RequestBody != `{"prompt":"hi"}` || !strings.Contains(r.ResponseBody, "resp:") {
		t.Fatalf("batchgraphql scope 正文捕获行为不得改变: req=%q resp=%q", r.RequestBody, r.ResponseBody)
	}
}
