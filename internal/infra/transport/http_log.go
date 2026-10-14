package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/httptrace"
)

// HTTPLogRecord 是一次上游 HTTP 请求的完整日志（transport 层视角）。
// 各文本字段完整原样存储；由注入的 recorder 负责持久化。
type HTTPLogRecord struct {
	TS              int64
	RequestID       string
	Scope           string // batchgraphql / recaptcha / admin-test ...
	NodeURI         string
	NodeName        string
	PeerIP          string
	TCPReused       bool
	HTTPVersion     string
	FinalURL        string
	RequestHeaders  string
	RequestBody     string
	ResponseHeaders string
	ResponseBody    string
	StatusCode      int // 传输层 HTTP 状态码
	Success         bool
	Error           string
	// 业务响应状态（区别于传输层 HTTP 状态）：从响应体解析的 error.code / finishReason。
	ResponseCode   int
	ResponseResult string
	// 失败详情：batchGraphql errors[].errorType 与 message。
	ResponseErrorType    string
	ResponseErrorMessage string
	ElapsedMs            int64
}

// HTTPLogRecorder 是 transport 层 HTTP 日志的记录器契约。
// 生产实现由 stats 收集器提供；未注入时为空实现（零开销）。
type HTTPLogRecorder interface {
	// Enabled 返回是否应记录（全局开关关闭时返回 false，transport 层跳过捕获）。
	Enabled() bool
	// BodyCapBytes 返回单条请求/响应体捕获上限（字节）；0 表示不截断。
	// 超过上限的部分被丢弃并在记录中追加截断标记，防止大 body 撑爆内存/磁盘。
	BodyCapBytes() int
	// Record 提交一条完整记录（实现方负责异步持久化）。
	Record(HTTPLogRecord)
}

// nopHTTPLogRecorder 是未注入时的空实现。
type nopHTTPLogRecorder struct{}

func (nopHTTPLogRecorder) Enabled() bool        { return false }
func (nopHTTPLogRecorder) BodyCapBytes() int    { return 0 }
func (nopHTTPLogRecorder) Record(HTTPLogRecord) {}

// recConnInfo 是 httptrace.GotConn 捕获的连接元数据。
type recConnInfo struct {
	peerIP  string
	reused  bool
	hasInfo bool
}

// applyTrace 为请求注入 httptrace（捕获 TCP 复用 + 对端 IP）。
func applyTrace(req *http.Request, info *recConnInfo) *http.Request {
	trace := &httptrace.ClientTrace{
		GotConn: func(gc httptrace.GotConnInfo) {
			info.hasInfo = true
			info.reused = gc.Reused
			if gc.Conn != nil {
				if ra := gc.Conn.RemoteAddr(); ra != nil {
					info.peerIP = ra.String()
				}
			}
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

// marshalHeaders 将 http.Header 序列化为 JSON 文本（失败返回空串）。
func marshalHeaders(h http.Header) string {
	if h == nil {
		return ""
	}
	b, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(b)
}

// loggedBody 累积响应体（受 bodyCap 截断）并在读尽/关闭时提交完整记录。
type loggedBody struct {
	r       io.ReadCloser
	rec     HTTPLogRecorder
	record  HTTPLogRecord
	start   time.Time
	buf     bytes.Buffer
	bodyCap int
	// skip 为 true 时只驱动提交、不累积正文：recaptcha scope 的 anchor/reload
	// 响应含有效 token（recaptcha-token value / rresp），落库即可被 admin 导出，
	// 属敏感凭据——元数据（URL/状态/耗时）照常记录，正文一律不留。
	skip    bool
	once    sync.Once
	gotEOF  bool
	aborted bool
	trunc   bool
}

func (lb *loggedBody) Read(p []byte) (int, error) {
	n, err := lb.r.Read(p)
	if n > 0 && !lb.skip {
		// 仅累积到 bodyCap 上限（bodyCap<=0 表示不截断），超出部分丢弃并标记截断。
		// 两个分支都必须只写 p[:n]：短读时 p 尾部是未有效读取的旧数据（可能含复用缓冲的残留）。
		if lb.bodyCap <= 0 {
			_, _ = lb.buf.Write(p[:n])
		} else if room := lb.bodyCap - lb.buf.Len(); room > 0 {
			if n <= room {
				_, _ = lb.buf.Write(p[:n])
			} else {
				_, _ = lb.buf.Write(p[:room])
				lb.trunc = true
			}
		} else {
			lb.trunc = true
		}
	}
	if err == io.EOF {
		lb.gotEOF = true
		lb.finish()
	}
	return n, err
}

func (lb *loggedBody) Close() error {
	// 未读到 EOF 即被关闭（Abort/排空中断）视为未完整交付：不标记成功。
	if !lb.gotEOF {
		lb.aborted = true
	}
	lb.finish()
	return lb.r.Close()
}

// finish 幂等地将缓冲响应体写入记录并提交。
// 成功判定：仅当响应体被完整读尽（EOF）且状态码为 2xx/3xx 时才算成功，
// 避免空闲超时/断流/未收尾的流被误标为成功。
func (lb *loggedBody) finish() {
	lb.once.Do(func() {
		body := lb.buf.String()
		lb.record.ResponseBody = body
		if lb.trunc {
			lb.record.ResponseBody += truncMarker
		}
		lb.record.ElapsedMs = time.Since(lb.start).Milliseconds()
		lb.record.Success = lb.gotEOF && !lb.aborted &&
			lb.record.StatusCode > 0 && lb.record.StatusCode < 400
		if !lb.record.Success && lb.record.Error == "" {
			if lb.aborted {
				lb.record.Error = "stream aborted (incomplete)"
			} else if lb.record.StatusCode >= 400 {
				lb.record.Error = fmt.Sprintf("http status %d", lb.record.StatusCode)
			}
		}
		// 从响应体提取业务响应状态（区别于传输层 HTTP 状态）。
		lb.record.ResponseCode, lb.record.ResponseResult,
			lb.record.ResponseErrorType, lb.record.ResponseErrorMessage =
			extractResponseMeta(body, lb.record.StatusCode)
		lb.rec.Record(lb.record)
	})
}

// responseMeta 是一次业务响应解析结果（四元组）。
type responseMeta struct {
	code      int
	result    string
	errorType string
	message   string
}

// batchError 是 batchGraphql results[].errors[] 中的一条错误。
type batchError struct {
	Message    string `json:"message"`
	ErrorType  string `json:"errorType"`
	Extensions struct {
		Status struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"status"`
	} `json:"extensions"`
}

// batchResult 是 batchGraphql results[] 中的一项。
type batchResult struct {
	Data   json.RawMessage `json:"data"`
	Errors []batchError    `json:"errors"`
}

// batchFrame 是流式多帧响应中的一帧。
type batchFrame struct {
	Results []batchResult `json:"results"`
}

// extractResponseMeta 从上游响应体提取业务响应状态（四元组：code/result/errorType/message）：
//   - batchGraphql 结构 {"results":[{errors:[...]}]} → code=extensions.status.code，
//     result/errorType=errorType，message=message（缺省取 extensions.status.message）；
//   - 顶层数组（流式多帧）→ 取最后含 errors 的帧；
//   - 既有分支：顶层 error / candidates(finishReason) / data；
//   - 解析失败（流式/截断）→ 按 HTTP 状态回退。
//
// 仅解析前缀 maxResponseMetaBytes 字节，避免大 body 全量解析开销。
func extractResponseMeta(body string, statusCode int) (int, string, string, string) {
	const maxResponseMetaBytes = 64 << 10
	if body == "" {
		return responseCodeFallback(statusCode), "", "", ""
	}
	prefix := body
	if len(prefix) > maxResponseMetaBytes {
		prefix = prefix[:maxResponseMetaBytes]
	}
	raw := []byte(prefix)
	if len(raw) > 0 && raw[0] == '[' {
		// 流式多帧数组：取最后含 errors 的帧。
		var frames []batchFrame
		if json.Unmarshal(raw, &frames) == nil {
			for i := len(frames) - 1; i >= 0; i-- {
				if m, ok := batchFrameMeta(frames[i]); ok {
					return m.code, m.result, m.errorType, m.message
				}
			}
			// 所有帧无 errors：业务成功。
			return 0, "OK", "", ""
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err == nil {
		if resRaw, ok := top["results"]; ok && len(resRaw) > 0 && resRaw[0] == '[' {
			var res []batchResult
			if json.Unmarshal(resRaw, &res) == nil {
				for _, r := range res {
					if m, ok := batchResultMeta(r); ok {
						return m.code, m.result, m.errorType, m.message
					}
				}
				// results 均无 errors：业务成功。
				return 0, "OK", "", ""
			}
		}
		if errRaw, ok := top["error"]; ok && len(errRaw) > 0 && errRaw[0] == '{' {
			var e struct {
				Code    int    `json:"code"`
				Status  string `json:"status"`
				Message string `json:"message"`
			}
			if json.Unmarshal(errRaw, &e) == nil {
				result := e.Status
				if result == "" {
					result = truncateStr(e.Message, 120)
				}
				return e.Code, result, result, truncateStr(e.Message, 120)
			}
		}
		if candRaw, ok := top["candidates"]; ok && len(candRaw) > 0 && candRaw[0] == '[' {
			var cands []struct {
				FinishReason string `json:"finishReason"`
			}
			if json.Unmarshal(candRaw, &cands) == nil {
				for _, c := range cands {
					if c.FinishReason != "" {
						return 0, c.FinishReason, "", ""
					}
				}
			}
			return 0, "OK", "", ""
		}
		if dataRaw, ok := top["data"]; ok && len(dataRaw) > 0 && dataRaw[0] == '{' {
			return 0, "OK", "", ""
		}
	}
	// 非 JSON 或解析失败：按 HTTP 状态推断。
	if statusCode > 0 && statusCode < 400 {
		return 0, "OK", "", ""
	}
	return responseCodeFallback(statusCode), truncateStr(body, 120), "", truncateStr(body, 120)
}

// batchFrameMeta 从一帧中提取 errors 元数据；无 errors 返回 ok=false。
func batchFrameMeta(f batchFrame) (responseMeta, bool) {
	for _, r := range f.Results {
		if m, ok := batchResultMeta(r); ok {
			return m, true
		}
	}
	return responseMeta{}, false
}

// batchResultMeta 从 results[] 项中提取 errors 元数据；无 errors 返回 ok=false。
func batchResultMeta(r batchResult) (responseMeta, bool) {
	if len(r.Errors) == 0 {
		return responseMeta{}, false
	}
	e := r.Errors[0]
	code := batchErrorCode(e)
	msg := e.Message
	if msg == "" {
		msg = e.Extensions.Status.Message
	}
	return responseMeta{
		code:      code,
		result:    e.ErrorType,
		errorType: e.ErrorType,
		message:   truncateStr(msg, 240),
	}, true
}

// batchErrorCode 解析 extensions.status.code（数字或字符串）。
func batchErrorCode(e batchError) int {
	raw := e.Extensions.Status.Code
	if len(raw) == 0 {
		return 0
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return int(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

// responseCodeFallback 在无响应体/解析失败时按 HTTP 状态推断业务响应码。
func responseCodeFallback(statusCode int) int {
	if statusCode > 0 {
		return statusCode
	}
	return 0
}

// truncateStr 截断字符串到 max 长度，用于日志/记录避免巨型文本刷屏。
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// httpCapture 是一次请求捕获过程的运行时状态。
type httpCapture struct {
	rec      HTTPLogRecorder
	ts       int64
	reqID    string
	scope    string
	nodeURI  string
	nodeName string
	finalURL string
	start    time.Time
	info     recConnInfo
	bodyCap  int
	// skipBody 与 loggedBody.skip 同语义：recaptcha scope 只记元数据不记正文。
	skipBody bool
	reqRec   *cappedBuffer
}

// truncMarker 是 body 被截断时追加的标记。
const truncMarker = "\n...[truncated]"

// cappedBuffer 是受上限约束的记录缓冲：写入超过 cap 的部分被丢弃并标记截断。
// cap<=0 表示不截断（记录全量）。
type cappedBuffer struct {
	buf   bytes.Buffer
	cap   int
	trunc bool
}

func (cb *cappedBuffer) Write(p []byte) (int, error) {
	if cb.cap <= 0 {
		_, _ = cb.buf.Write(p)
		return len(p), nil
	}
	room := cb.cap - cb.buf.Len()
	if room > 0 {
		if len(p) <= room {
			_, _ = cb.buf.Write(p)
		} else {
			_, _ = cb.buf.Write(p[:room])
			cb.trunc = true
		}
	} else {
		cb.trunc = true
	}
	return len(p), nil
}

// string 返回记录内容；截断时追加标记。
func (cb *cappedBuffer) string() string {
	if cb == nil {
		return ""
	}
	s := cb.buf.String()
	if cb.trunc {
		s += truncMarker
	}
	return s
}

// teeReadCloser 把读取流经 Write 到记录缓冲，同时原样透传全部字节给上游。
// 上游发送的请求体与"无记录"时逐字节一致（ContentLength 由 http.NewRequest 判定），
// 记录侧由 cappedBuffer 按 cap 截断。
type teeReadCloser struct {
	r io.ReadCloser
	w io.Writer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		_, _ = t.w.Write(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.r.Close() }

// wrapRequest 返回带 httptrace 的请求（开关开启时）。
func (c *httpCapture) wrapRequest(req *http.Request) *http.Request {
	return applyTrace(req, &c.info)
}

// wrapRequestBody 用 tee 包装请求体：全量透传发送 + 按 cap 记录副本。
// skipBody（recaptcha scope）时不包装：请求正文含 anchor 表单与 reload token，
// 只记元数据；reqRec 保持 nil（makeRecord 的 string() 有 nil 防护，记空串）。
func (c *httpCapture) wrapRequestBody(req *http.Request) *http.Request {
	if req.Body == nil || c.skipBody {
		return req
	}
	c.reqRec = &cappedBuffer{cap: c.bodyCap}
	req.Body = &teeReadCloser{r: req.Body, w: c.reqRec}
	return req
}

// makeRecord 组装请求侧记录（响应侧由 loggedBody 补全）。
func (c *httpCapture) makeRecord(reqHeader http.Header) HTTPLogRecord {
	return HTTPLogRecord{
		TS:             c.ts,
		RequestID:      c.reqID,
		Scope:          c.scope,
		NodeURI:        c.nodeURI,
		NodeName:       c.nodeName,
		FinalURL:       c.finalURL,
		RequestBody:    c.reqRec.string(),
		RequestHeaders: marshalHeaders(reqHeader),
	}
}

// attachResponse 填充响应侧信息并包装响应体为累积 reader。
// 返回包装后的 body（开关开启时）；resp 为 nil 时返回 nil。
func (c *httpCapture) attachResponse(resp *http.Response, reqHeader http.Header) io.ReadCloser {
	if resp == nil {
		return nil
	}
	rec := c.makeRecord(reqHeader)
	rec.StatusCode = resp.StatusCode
	rec.HTTPVersion = httpVersionOf(resp)
	rec.ResponseHeaders = marshalHeaders(resp.Header)
	if c.info.hasInfo {
		rec.PeerIP = c.info.peerIP
		rec.TCPReused = c.info.reused
	}
	lb := &loggedBody{r: resp.Body, rec: c.rec, record: rec, start: c.start, bodyCap: c.bodyCap, skip: c.skipBody}
	return lb
}

// httpVersionOf 返回响应使用的 HTTP 版本；resp.Proto 为空时按 ProtoMajor 兜底推导。
func httpVersionOf(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	if resp.Proto != "" {
		return resp.Proto
	}
	switch resp.ProtoMajor {
	case 2:
		return "HTTP/2"
	case 3:
		return "HTTP/3"
	default:
		return "HTTP/1.1"
	}
}
