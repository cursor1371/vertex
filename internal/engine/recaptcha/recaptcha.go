// Package recaptcha 实现 Google reCAPTCHA Enterprise 匿名 token 的现抓现用。
//
// 流程：anchor iframe GET 抠出 base token，再 reload POST
// 拿到最终 token（rresp）。token 用于 batchGraphql 的 recaptchaToken 字段。
package recaptcha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
)

// recaptcha 相关硬编码常量（逐字节保持既定常量）。
// hl 语言指纹已收敛到 fingerprint 包的地区预设（经 transport.FingerprintHL 读取）。
const (
	recaptchaBase      = "https://www.google.com"
	siteKey            = "6LdCjtspAAAAAMcV4TGdWLJqRTEk1TfpdLqEnKdj"
	recaptchaCo        = "aHR0cHM6Ly9jb25zb2xlLmNsb3VkLmdvb2dsZS5jb206NDQz"
	recaptchaVFallback = "jdMmXeCQEkPbnFDy9T04NbgJ"
	recaptchaVh        = "6581054572"
	randomCharset      = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// versionNegativeCacheTTL 是一次版本拉取/疑似过期后，到再次请求 enterprise.js 的
// 最短间隔。此前 invalidateVersion 直接清空缓存：每次 token 失败的下一个 attempt
// 都要多打一次 enterprise.js（15 候选 × 3 重试最坏 135 次请求，+50% HTTP 放大），
// 且缓存为空的瞬间预取池 + 竞速候选全量并发穿透。负缓存窗口内继续用最后已知版本。
var versionNegativeCacheTTL = 30 * time.Second // 测试可缩短

// fetchVersionFn 是版本拉取的实现入口，测试注入用；生产恒为 fetchVersionFromSession。
var fetchVersionFn = fetchVersionFromSession

var (
	versionMu sync.Mutex
	cachedVer string
	// cachedVerSuspect 标记"版本可能已过期"：token 失败后置位，负缓存窗口到期后
	// 由下一次 currentVersion 触发重取（成功即清除）。不清空 cachedVer——最后已知
	// 版本大概率仍有效（失败也可能只是网络抖动），继续用它的代价为零。
	cachedVerSuspect   bool
	cachedVerInvalidAt time.Time // 最近一次失败/置位时刻，负缓存窗口的锚点
	// verGroup 折叠并发版本拉取：缓存为空/疑似过期的瞬间，预取池 + 竞速候选的
	// 全量 goroutine 共享同一次 enterprise.js 请求。
	verGroup singleflight.Group

	// 从 enterprise.js 提取 reCAPTCHA release 版本号（Google 定期滚动，不能硬编码）。
	versionRe = regexp.MustCompile(`releases/([A-Za-z0-9_-]{20,})`)

	// 从 anchor HTML 抠 base token。用正则而非 HTML 解析器（已实测可行、无需额外依赖）。
	tokenRe = regexp.MustCompile(`id="recaptcha-token"[^>]*value="([^"]+)"`)
	// 从 reload 响应抠最终 token。
	rrespRe = regexp.MustCompile(`rresp","(.*?)"`)
)

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = randomCharset[rand.Intn(len(randomCharset))]
	}
	return string(b)
}

func fetchVersionFromSession(ctx context.Context, sess *transport.Session) (string, bool) {
	jsURL := recaptchaBase + "/recaptcha/enterprise.js"
	header := transport.XHRHeaders("", "*/*", recaptchaBase, recaptchaBase, "cross-site")
	status, body, err := sess.DoAndRead(ctx, "GET", jsURL, header, nil)
	if err != nil || status != 200 {
		return "", false
	}
	m := versionRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// currentVersion 返回 reCAPTCHA release 版本：优先用缓存；被标记疑似过期且负缓存
// 窗口已过时同步重取一次（并发经 singleflight 折叠）；空缓存且处于负缓存窗口时
// 直接回落硬编码版本，不穿透 enterprise.js。
//
// 此前实现是"取锁看缓存→解锁→联网"：缓存为空的瞬间（启动首抓、或失败清缓存后），
// 预取池 + 竞速候选的全量 goroutine 各自穿透一次 enterprise.js。
func currentVersion(ctx context.Context, sess *transport.Session) (string, bool) {
	now := time.Now()
	versionMu.Lock()
	ver := cachedVer
	// suspect 且负缓存窗口已过：同步重取一次换新版本。窗口内继续用最后已知版本，
	// 避免 15 候选 × 3 重试的失败风暴把 enterprise.js 打成每 attempt 一次。
	suspectRefresh := cachedVerSuspect && now.Sub(cachedVerInvalidAt) >= versionNegativeCacheTTL
	// 空缓存时的负缓存窗口：刚失败过（含版本拉取失败）就先回落 recaptchaVFallback。
	emptyCooldown := cachedVer == "" && now.Sub(cachedVerInvalidAt) < versionNegativeCacheTTL
	versionMu.Unlock()

	if ver != "" {
		if suspectRefresh {
			// 重取成功才换新；失败保持旧值（多半仍有效，且负缓存会再拦 30s）。
			if fresh, ok := fetchAndCacheVersion(ctx, sess); ok {
				return fresh, true
			}
		}
		return ver, true
	}
	if emptyCooldown {
		return "", false
	}
	return fetchAndCacheVersion(ctx, sess)
}

// fetchAndCacheVersion 拉取并缓存版本号。并发调用经 singleflight 折叠为一次
// enterprise.js 请求；共享调用用脱离请求生命周期的 ctx——版本是进程级数据，
// 任何一个发起者的取消都不应拖垮同批等待者。
func fetchAndCacheVersion(ctx context.Context, sess *transport.Session) (string, bool) {
	v, err, _ := verGroup.Do("ver", func() (any, error) {
		ver, ok := fetchVersionFn(context.WithoutCancel(ctx), sess)
		if !ok {
			return nil, errors.New("fetch recaptcha version failed")
		}
		return ver, nil
	})
	if err != nil {
		versionMu.Lock()
		cachedVerInvalidAt = time.Now() // 负缓存：30s 内不再穿透
		versionMu.Unlock()
		return "", false
	}
	ver := v.(string)
	versionMu.Lock()
	cachedVer = ver
	cachedVerSuspect = false
	versionMu.Unlock()
	return ver, true
}

// invalidateVersion 在 token 抓取失败后把缓存版本标记为"疑似过期"。
//
// 此前直接清空 cachedVer：每次失败的下一个 attempt 都要多打一次 enterprise.js
// （+50% HTTP 放大）。现在不清空——最后已知版本大概率仍有效（失败也可能只是网络
// 抖动，回退硬编码版本反而更旧），只置 suspect，由负缓存窗口到期后的下一次
// currentVersion 触发折叠重取；真因版本滚动导致的失败会在重取后恢复。
func invalidateVersion() {
	versionMu.Lock()
	defer versionMu.Unlock()
	cachedVerSuspect = true
	cachedVerInvalidAt = time.Now()
}

// fetchToken 获取 Google reCAPTCHA token（隔离特征）。
//
// 最多 3 次重试，每次新建一个 short Timeout Session
// （即用即毁，FRESH_CONNECT 语义）。全部失败返回 ("", nil) —— 返回空值表示失败，
// 调用方按"空则换新/重试"处理。返回非空字符串即成功。
func (p *TokenPool) fetchToken(ctx context.Context, proxyURI string, debugMode bool) (string, error) {
	return p.fetchTokenWith(ctx, proxyURI, debugMode, true)
}

// fetchTokenQuiet 与 fetchToken 相同，但失败时不失效全局版本缓存（预取池专用）。
func (p *TokenPool) fetchTokenQuiet(ctx context.Context, proxyURI string, debugMode bool) (string, error) {
	return p.fetchTokenWith(ctx, proxyURI, debugMode, false)
}

// fetchTokenWith 是抓取单节点 token 的重试主体：最多 3 次，每次新建短超时会话
// （即用即毁，FRESH_CONNECT 语义）。invalidateOnFail 语义见 fetchOnceWith。
func (p *TokenPool) fetchTokenWith(ctx context.Context, proxyURI string, debugMode, invalidateOnFail bool) (string, error) {
	var nodeName string
	if p.source != nil {
		nodeName = p.source.NodeName(proxyURI)
	}
	if proxyURI == "" {
		nodeName = "直连 (Direct)"
	}
	reqIDTag := "recaptcha"
	if !invalidateOnFail {
		reqIDTag = "recaptcha-prefetch"
	}

	start := time.Now()
	// lastErr 保留最后一次底层失败原因，供上层做节点健康归因（原先只返回一句笼统文案，
	// 上报给节点池后无法区分"节点连不通"与"预算到点/被取消"）。
	var lastErr error
	// attemptsCompleted 统计"跑完过的尝试"（返回了非 ctx 类错误即算跑完）。
	// 预算到点且一个都没跑完 = 节点把请求挂住了，而不是预算不够——判别依据是
	// GetTokenShared 的 ctx 截止总是早于每次尝试新建会话的 HTTP 超时，所以节点卡死
	// 必然表现为 ctx 错误；而"预算太小但节点正常"时尝试会完成、只是慢。
	attemptsCompleted := 0
	for retry := 0; retry < 3; retry++ {
		// 【核心修改：将具体的节点名称明确输出在日志归属中】
		if debugMode {
			log.Printf("[Recaptcha] [节点: %s] 开始获取 reCAPTCHA token (尝试 %d/3)", nodeName, retry+1)
		}
		token, err := fetchOnceWith(ctx, p.net, proxyURI, reqIDTag, invalidateOnFail)
		if err == nil && token != "" {
			elapsed := time.Since(start)
			if debugMode {
				log.Printf("[Recaptcha] [节点: %s] 成功获取 reCAPTCHA token, 耗时: %d ms", nodeName, elapsed.Milliseconds())
			}
			return token, nil
		}
		if err != nil {
			lastErr = err
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				attemptsCompleted++
			}
		} else {
			lastErr = fmt.Errorf("empty token")
			attemptsCompleted++
		}
		if retry < 2 {
			delay := time.Duration((retry+1)*200) * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctxInterruptedError(nodeName, ctx, attemptsCompleted)
			case <-timer.C:
			}
		}
	}
	elapsed := time.Since(start)
	if debugMode {
		log.Printf("[Recaptcha] [节点: %s] 3次尝试后获取 reCAPTCHA token 失败, 耗时: %d ms", nodeName, elapsed.Milliseconds())
	}
	if ctx.Err() != nil {
		// 带节点名：预算到点的失败原先只返回裸 ctx.Err()，日志与实时记录流里看不出
		// 死在哪个节点上，容易被误读成"滑落直连"。
		return "", ctxInterruptedError(nodeName, ctx, attemptsCompleted)
	}
	if lastErr != nil {
		return "", fmt.Errorf("节点 %s 3次重试后仍无法获取 recaptcha token: %w", nodeName, lastErr)
	}
	return "", fmt.Errorf("节点 %s 3次重试后仍无法获取 recaptcha token", nodeName)
}

// tokenHangError 标记"抓取挂住"：预算到点且本次一个尝试都没跑完。
//
// 这是节点卡死（连 HTTP 响应都拿不到），而不是"预算不够"——预算太小但节点正常时，
// 尝试会完成（只是慢）。上报节点池后按"先短冷却、累计仍挂则禁用"处置。
type tokenHangError struct {
	node string
	err  error
}

func (e *tokenHangError) Error() string {
	return fmt.Sprintf("节点 %s 抓取挂住（预算到点且未完成任何尝试）: %v", e.node, e.err)
}

func (e *tokenHangError) Unwrap() error { return e.err }

// ctxInterruptedError 把"抓取被 ctx 打断"细分成两类并带上节点名：
//   - ctx 被取消（竞速其它候选胜出 / 请求中断 / 客户端断开）→ 与节点无关，上层不上报；
//   - 预算到点，且一个尝试都没跑完 → tokenHangError（节点挂住）；
//   - 预算到点但尝试有完成 → 普通超时错误（预算问题，不归因节点）。
func ctxInterruptedError(node string, ctx context.Context, attemptsCompleted int) error {
	err := ctx.Err()
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("节点 %s 抓取被取消（竞速其它候选胜出或请求中断）: %w", node, err)
	}
	if attemptsCompleted == 0 {
		return &tokenHangError{node: node, err: err}
	}
	return fmt.Errorf("节点 %s 抓取超时（单轮预算到点）: %w", node, err)
}

// FetchRecaptchaTokenWithSession 在有 Session 的上下文中执行 anchor→reload 全流程。
// 这是共享核心操作，生产路径和节点测速路径均调用。
// 错误均为非 nil（网络失败、正则解析失败、reload 非 200），调用方按自身语义处理。
func FetchRecaptchaTokenWithSession(ctx context.Context, sess *transport.Session) (string, error) {
	ver, ok := currentVersion(ctx, sess)
	if !ok {
		ver = recaptchaVFallback // 动态拉取失败降级到硬编码（比硬失败更稳）
	}

	cb := randomString(10)
	// hl 语言指纹与请求头 Accept-Language 同源（fingerprint 包的地区预设），
	// 未启用指纹开关时取既有硬编码基线 zh-CN。
	hl := transport.FingerprintHL()
	anchorURL := fmt.Sprintf(
		"%s/recaptcha/enterprise/anchor?ar=1&k=%s&co=%s&hl=%s&v=%s&size=invisible&anchor-ms=20000&execute-ms=15000&cb=%s",
		recaptchaBase, siteKey, recaptchaCo, hl, ver, cb,
	)

	_, anchorBody, err := sess.DoAndRead(ctx, "GET", anchorURL, transport.AnchorHeaders(), nil)
	if err != nil {
		return "", fmt.Errorf("GET anchor 失败: %w", err)
	}
	m := tokenRe.FindSubmatch(anchorBody)
	if m == nil {
		bodyStr := string(anchorBody)
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500] + "..."
		}
		log.Printf("[Recaptcha] anchor token正则匹配失败, body前缀: %s", bodyStr)
		return "", fmt.Errorf("从 anchor HTML 解析 recaptcha-token 失败")
	}
	baseToken := string(m[1])

	form := url.Values{
		"v":      {ver},
		"reason": {"q"},
		"k":      {siteKey},
		"c":      {baseToken},
		"co":     {recaptchaCo},
		"hl":     {transport.FingerprintHL()},
		"size":   {"invisible"},
		"vh":     {recaptchaVh},
		"chr":    {""},
		"bg":     {""},
	}
	reloadURL := recaptchaBase + "/recaptcha/enterprise/reload?k=" + siteKey
	header := transport.XHRHeaders(
		"application/x-www-form-urlencoded;charset=UTF-8", "*/*",
		recaptchaBase, anchorURL, "same-origin",
	)

	status, reloadBody, err := sess.DoAndRead(ctx, "POST", reloadURL, header, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("POST reload 失败: %w", err)
	}
	if status != 200 {
		bodyStr := string(reloadBody)
		if len(bodyStr) > 200 {
			bodyStr = bodyStr[:200]
		}
		log.Printf("[Recaptcha] Reload 失败, HTTP 状态码: %d, 响应体前200字符: %s", status, bodyStr)
		return "", fmt.Errorf("reload 返回非 200 状态码: %d", status)
	}
	rm := rrespRe.FindSubmatch(reloadBody)
	if rm == nil {
		bodyStr := string(reloadBody)
		if len(bodyStr) > 200 {
			bodyStr = bodyStr[:200]
		}
		log.Printf("[Recaptcha] Reload 响应解析 rresp 失败, HTTP 状态码: %d, 响应体前200字符: %s", status, bodyStr)
		return "", fmt.Errorf("从 reload 响应解析 rresp 失败")
	}
	return string(rm[1]), nil
}

// fetchOnceWith 是抓取单份 token 的底层实现。invalidateOnFail 控制失败时是否失效
// 全局 enterprise.js 版本缓存：请求路径为 true；预取池为 false——后台补货失败是
// 常态（网络抖动/限流），不应把后台失败放大成请求路径重拉脚本的额外延迟。
//
// 返回底层错误而不是布尔：失败原因要能上报给出口节点池做健康归因（见
// TokenPool.reportTokenFetch），"抓不到 token"这句笼统结论无法区分是节点连不通
// 还是预算到点/被取消。
func fetchOnceWith(ctx context.Context, net *transport.NetworkClient, proxyURI, reqID string, invalidateOnFail bool) (string, error) {
	sess, err := net.CreateSession(15, proxyURI, reqID)
	if err != nil {
		return "", err
	}
	defer sess.Close()

	token, err := FetchRecaptchaTokenWithSession(ctx, sess)
	if err != nil {
		if invalidateOnFail {
			invalidateVersion()
		}
		return "", err
	}
	return token, nil
}
