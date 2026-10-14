package transport

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

type Header = http.Header
type Response = http.Response

type Session struct {
	client   tls_client.HttpClient
	ProxyURI string
	cleanup  func()
	rec      HTTPLogRecorder
	reqID    string
}

// scopeOf 根据请求目标 URL 判定记录范围标记。
func (s *Session) scopeOf(url string) string {
	if s.rec == nil || !s.rec.Enabled() {
		return ""
	}
	if strings.Contains(url, "recaptcha") {
		return "recaptcha"
	}
	if strings.Contains(url, "batchGraphql") {
		return "batchgraphql"
	}
	return "other"
}

func (s *Session) Do(ctx context.Context, method, url string, header http.Header, body io.Reader) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}
	req = req.WithContext(ctx)
	if header != nil {
		req.Header = header
	}

	// 开关开启时捕获完整请求/响应日志（httptrace + 请求体 tee + 响应体累积）。
	// recaptcha scope 只记元数据：anchor/reload 正文含有效 token（凭据级敏感，
	// 落库即可经 admin 导出），正文一律不捕获。
	rec := s.rec
	if rec != nil && rec.Enabled() {
		scope := s.scopeOf(url)
		capture := &httpCapture{
			rec:      rec,
			ts:       time.Now().Unix(),
			reqID:    s.reqID,
			scope:    scope,
			nodeURI:  s.ProxyURI,
			finalURL: url,
			start:    time.Now(),
			bodyCap:  rec.BodyCapBytes(),
			skipBody: scope == "recaptcha",
		}
		req = capture.wrapRequest(req)
		req = capture.wrapRequestBody(req)
		resp, doErr := s.client.Do(req)
		if doErr != nil {
			// 请求失败（未得到响应）：仍记录请求侧信息与错误原因。
			recErr := capture.makeRecord(req.Header)
			recErr.ElapsedMs = time.Since(capture.start).Milliseconds()
			recErr.Error = doErr.Error()
			if capture.info.hasInfo {
				recErr.PeerIP = capture.info.peerIP
				recErr.TCPReused = capture.info.reused
			}
			rec.Record(recErr)
			return nil, doErr
		}
		lb := capture.attachResponse(resp, req.Header)
		if lb != nil {
			resp.Body = lb
		}
		return resp, nil
	}

	return s.client.Do(req)
}

func (s *Session) DoAndRead(ctx context.Context, method, url string, header http.Header, body io.Reader) (int, []byte, error) {
	resp, err := s.Do(ctx, method, url, header, body)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, fmt.Errorf("error: %w", readErr)

	}
	return resp.StatusCode, data, nil
}

type StreamResponse struct { //nolint:govet
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	closeOnce  sync.Once
}

func (sr *StreamResponse) Close() {
	if sr.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, sr.Body)
	sr.closeOnce.Do(func() {
		_ = sr.Body.Close()
	})
}

// Abort 强制中断流连接并关闭 Body，绝不执行 drain 排空。
// 适用于取消、空闲超时和异常中断路径。
func (sr *StreamResponse) Abort() {
	if sr.Body == nil {
		return
	}
	sr.closeOnce.Do(func() {
		_ = sr.Body.Close()
	})
}

func (s *Session) DoStream(ctx context.Context, method, url string, header http.Header, body io.Reader) (*StreamResponse, error) {
	resp, err := s.Do(ctx, method, url, header, body)
	if err != nil {
		return nil, err
	}
	return &StreamResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

func (s *Session) Close() {
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
	if s.cleanup != nil {
		s.cleanup()
	}
}

type NetworkClient struct {
	dialer ProxyDialer
	rec    HTTPLogRecorder
}

func NewNetworkClient(dialer ProxyDialer) *NetworkClient {
	return &NetworkClient{dialer: dialer, rec: nopHTTPLogRecorder{}}
}

// SetHTTPLogRecorder 设置进程级 HTTP 日志记录器（main 装配时注入一次）。
// nil 或未设置时关闭记录（空实现，零开销）。
func (c *NetworkClient) SetHTTPLogRecorder(rec HTTPLogRecorder) {
	if rec == nil {
		rec = nopHTTPLogRecorder{}
	}
	c.rec = rec
}

func (c *NetworkClient) Dialer() ProxyDialer {
	return c.dialer
}

// legacyBrowserProfiles 是指纹开关关闭时的既有行为：在两个版本间随机，
// 配 Chrome 150 请求头（headers.go 的 Legacy 基线）。
//
//nolint:gochecknoglobals
var legacyBrowserProfiles = []profiles.ClientProfile{
	profiles.Chrome_144, profiles.Chrome_146,
}

// pickProfile 返回本次会话的 TLS ClientHello profile：指纹开关生效时用当前
// 指纹包指定值（TLS 与请求头版本成对切换，见 fingerprint 包）；否则维持既有
// 行为——随机 144/146，零行为变化。
func pickProfile() profiles.ClientProfile {
	if b := fingerprint.Active(); b != nil {
		return b.Pick()
	}
	return legacyBrowserProfiles[rand.Intn(len(legacyBrowserProfiles))]
}

// CreateSession 创建一个新 Session。
//
//   - secondHopURI 为空时：全局前置代理非空则经回环 SOCKS 第一跳，否则直连。
//   - secondHopURI 非空时：创建临时第二跳 sing-box（有前置时 detour 经第一跳）。
//     禁止对第二跳 URI 直接使用 WithProxyUrl 绕过第一跳。
func (c *NetworkClient) CreateSession(timeoutSec int, secondHopURI string, reqID string) (*Session, error) {
	prof := pickProfile()

	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(timeoutSec),
		tls_client.WithClientProfile(prof),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}

	var cleanup func()
	var err error

	if secondHopURI != "" {
		opts, cleanup, err = c.injectSecondHopDialer(opts, secondHopURI, reqID)
	} else {
		opts, err = c.injectEntryProxy(opts)
	}

	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("error: %w", err)

	}
	return &Session{client: client, ProxyURI: secondHopURI, cleanup: cleanup, rec: c.rec, reqID: reqID}, nil
}

// injectEntryProxy 配置 tls-client 经回环 SOCKS 走前置代理池（按请求轮询）。
// 池已 StopAll（优雅停机）时显式拒绝：此时放行会按"池为空"降级直连，让在途请求
// 绕过前置代理出网；停机期应尽快失败，由 ctx 取消链收尾。
func (c *NetworkClient) injectEntryProxy(opts []tls_client.HttpClientOption) ([]tls_client.HttpClientOption, error) {
	if c.dialer == nil {
		return opts, nil
	}
	if c.dialer.Stopped() {
		return nil, fmt.Errorf("entry proxy pool stopped (shutting down)")
	}
	addr := c.dialer.GetNextEntrySocksAddr()
	if addr == "" {
		return opts, nil
	}
	proxyURL := "socks5://" + addr
	return append(opts, tls_client.WithProxyUrl(proxyURL)), nil
}

// injectSecondHopDialer 配置 tls-client 通过临时第二跳 sing-box 拨号。
// 第二跳 box 内部有前置时 detour 经回环 SOCKS。
// 无论 URI 协议类型（VLESS/VMess/SS/HTTP/SOCKS等），统一使用 CreateDialer，
// 禁止对任何第二跳 URI 直接使用 tls_client.WithProxyUrl 绕过第一跳。
func (c *NetworkClient) injectSecondHopDialer(opts []tls_client.HttpClientOption, secondHopURI string, reqID string) ([]tls_client.HttpClientOption, func(), error) {
	dialCtx, cleanup, err := c.dialer.CreateDialer(secondHopURI, reqID)
	if err != nil {
		return nil, nil, fmt.Errorf("第二跳 Dialer 启动失败: %w", err)
	}
	opts = append(opts, tls_client.WithDialContext(dialCtx))
	return opts, cleanup, nil
}
