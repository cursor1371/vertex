// Package catalog 负责从上游 Google Model Garden 目录拉取模型清单
// （batchGraphql 的 ListPublisherModel 操作），并做过滤、分类与差异计算。
//
// 网络访问通过注入的 Doer 完成（api 层用节点池会话实现），本包只承载协议与纯逻辑，
// 使分页解析、过滤规则、差异分类与签名自愈都可以离线测试。
// 请求构造与 scripts/更新模型.bat 保持一致（同端点、同查询签名与 requestContext 常量）。
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/jsonx"
)

const (
	// DefaultSignature 是 ListPublisherModel 的内置持久化查询签名。
	// Google 滚动更新前端后可能失效，失效时由 FindFreshSignature 从上游 bundle 重新提取；
	// 提取结果缓存在 config/model_catalog_sig.txt（与更新模型.bat 共用同一文件）。
	DefaultSignature = "2/gBk5XPNYZ1xSZpynVBEl2/GnZ5By2c1WQrVqJfS4DFw="

	// Parent 是目录的发布者根；PageSize 为单页条数；MaxPages 为分页上限
	// （防御上游 nextPageToken 异常导致的无限翻页；触顶视为结果不完整并报错）。
	Parent   = "publishers/google"
	PageSize = 100
	MaxPages = 30

	// UserAgent 与更新模型.bat 一致（Chrome 桌面 UA）。
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

	// ConsolePageURL 是签名自愈时抓取的控制台页面（bundle 引用的来源）。
	ConsolePageURL = "https://console.cloud.google.com/agent-platform/studio/multimodal"

	// minKeptRatioPercent 是快照合理性阈值：本次候选数低于上次可信快照的该百分比
	// 即视为可疑（可能是分页失败、过滤规则变化或上游结构变化），不覆盖快照并等待人工确认。
	minKeptRatioPercent = 50
)

// operationNameListPublisherModel 是目录查询的操作名。
const operationNameListPublisherModel = "ListPublisherModel"

// skipPatterns 是非生成式条目模式：这些模型不是 generateContent 能服务的，不进模型清单
// （与更新模型.bat 的 SkipPatterns 一致）。
//
//nolint:gochecknoglobals // 只读模式表
var skipPatterns = []string{
	"embedding", "-live", "live-", "transcribe", "robotics", "computer-use", "native-audio", "-info",
}

// Doer 是一次 HTTP 交互的最小契约（api 层用节点池会话实现，测试用 httptest 实现）。
type Doer interface {
	Do(ctx context.Context, method, url string, header http.Header, body []byte) (status int, respBody []byte, err error)
}

// UpstreamError 是上游返回的错误（含分页页码与原始 message），便于 UI 展示可理解的原因。
type UpstreamError struct {
	Message string
	Page    int
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("上游目录请求失败（第 %d 页）: %s", e.Page, e.Message)
}

// Result 是一次完整目录拉取的结果。
type Result struct {
	// All 是上游返回的全部条目 ID（已剥离 publishers/*/models/ 前缀，去重保序，未过滤）。
	All []string
	// Pages 是实际翻页次数。
	Pages int
}

// Entry 是目录条目的分类结果（家族与可用性）。
type Entry struct {
	ID                string `json:"id"`
	Family            string `json:"family"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Supported         bool   `json:"supported"`
}

// Classify 按名字启发式给出目录条目的建议家族与可用性。
//
// 目录条目没有本地 family_map 覆盖，只按名字推断；视频家族（vedio）后端尚未实现：
// supported=false 并给出明确原因（"目录可见 / 暂不可用"）。
func Classify(id string) Entry {
	fam := config.HeuristicFamilyName(id)
	e := Entry{ID: id, Family: fam, Supported: fam != config.FamilyNameVideo}
	if !e.Supported {
		e.UnavailableReason = "视频生成 API 尚未实现"
	}
	return e
}

// buildRequestBody 构造单页请求体（与更新模型.bat 的 Invoke-CatalogPage 完全一致）。
func buildRequestBody(signature, pageToken string) ([]byte, error) {
	vars := map[string]any{
		"parent":   Parent,
		"pageSize": PageSize,
	}
	if pageToken != "" {
		vars["pageToken"] = pageToken
	}
	payload := map[string]any{
		"requestContext": map[string]any{
			"clientVersion":    "boq_cloud-boq-clientweb-vertexaistudio_20260630.00_p0",
			"pagePath":         "/agent-platform/studio/multimodal",
			"pageViewId":       1234567890123456,
			"trackingId":       "d1234567890123456",
			"backendOverrides": map[string]any{},
			"clientSessionId":  "00000000-0000-4000-8000-000000000000",
			"selectedPurview":  map[string]any{},
			"jurisdiction":     "global",
		},
		"querySignature": signature,
		"operationName":  operationNameListPublisherModel,
		"variables":      vars,
	}
	b, err := jsonx.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return b, nil
}

// listPublisherModelResponse 对应 batchGraphql 单页响应：
//
//	[{"results":[{"data":{"ui":{"listPublisherModel":{"data":[{"name":"publishers/google/models/x"}],
//	                  "nextPageToken":"..."}}}}]}]
//
// 错误形如 [{"results":[{"errors":[{"message":"..."}]}]}]。
type listPublisherModelResponse []struct {
	Results []struct {
		Data *struct {
			UI struct {
				ListPublisherModel struct { //nolint:govet // 字段顺序由上游 JSON 结构决定，不可重排
					Data []struct {
						Name string `json:"name"`
					} `json:"data"`
					NextPageToken string `json:"nextPageToken"`
				} `json:"listPublisherModel"`
			} `json:"ui"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"results"`
}

// stripPublisherPrefix 去掉 publishers/<publisher>/models/ 前缀，只保留模型 ID。
func stripPublisherPrefix(name string) string {
	n := strings.TrimSpace(name)
	if idx := strings.LastIndex(n, "/models/"); idx != -1 {
		return n[idx+len("/models/"):]
	}
	return n
}

// parsePage 解析单页响应：返回条目 ID 列表与下一页 token。
func parsePage(raw []byte) (ids []string, nextToken string, err error) {
	var parsed listPublisherModelResponse
	if uerr := json.Unmarshal(raw, &parsed); uerr != nil {
		return nil, "", fmt.Errorf("解析上游响应失败: %w", uerr)
	}
	if len(parsed) == 0 || len(parsed[0].Results) == 0 {
		return nil, "", errors.New("上游响应缺少 results 段")
	}
	first := parsed[0].Results[0]
	if len(first.Errors) > 0 && strings.TrimSpace(first.Errors[0].Message) != "" {
		return nil, "", errors.New(strings.TrimSpace(first.Errors[0].Message))
	}
	if first.Data == nil {
		// data 缺失（如空目录/结构变化）：视为空页，交由上层判断完整性。
		return nil, "", nil
	}
	node := first.Data.UI.ListPublisherModel
	for _, item := range node.Data {
		if id := stripPublisherPrefix(item.Name); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, strings.TrimSpace(node.NextPageToken), nil
}

// upstreamMessage 尽力从错误响应体中提取可读 message（errors[0].message）。
func upstreamMessage(raw []byte) string {
	var parsed listPublisherModelResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	if len(parsed) == 0 || len(parsed[0].Results) == 0 {
		return ""
	}
	if errs := parsed[0].Results[0].Errors; len(errs) > 0 {
		return strings.TrimSpace(errs[0].Message)
	}
	return ""
}

// requestHeaders 构造目录请求头（与更新模型.bat 一致）。
func requestHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("Origin", "https://console.cloud.google.com")
	h.Set("Referer", "https://console.cloud.google.com/")
	h.Set("User-Agent", UserAgent)
	return h
}

// Fetch 拉取完整目录（自动翻页）。
//
// 完整性约束：任一分页失败、或翻页达到 MaxPages 仍未结束，都返回错误——
// 不完整的目录绝不能进入快照与差异计算（避免把"临时缺失"误判为"已下架"）。
// ctx 为父级取消上下文：取消后立即返回，不再发起后续分页。
func Fetch(ctx context.Context, doer Doer, endpoint, signature string) (Result, error) {
	var all []string
	seen := make(map[string]bool)
	token := ""
	for page := 1; page <= MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("error: %w", err)
		}
		body, err := buildRequestBody(signature, token)
		if err != nil {
			return Result{}, err
		}
		status, respBody, err := doer.Do(ctx, http.MethodPost, endpoint, requestHeaders(), body)
		if err != nil {
			return Result{}, fmt.Errorf("error: %w", err)
		}
		if status != http.StatusOK {
			msg := upstreamMessage(respBody)
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", status)
			}
			return Result{}, &UpstreamError{Page: page, Message: msg}
		}
		ids, next, err := parsePage(respBody)
		if err != nil {
			return Result{}, &UpstreamError{Page: page, Message: err.Error()}
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				all = append(all, id)
			}
		}
		if next == "" {
			return Result{All: all, Pages: page}, nil
		}
		token = next
	}
	return Result{}, fmt.Errorf("目录翻页达到上限 %d 页仍未结束，结果可能不完整", MaxPages)
}

// FilterCatalog 从上游全部条目中筛出生成式候选：
// 保留 gemini-* 与 veo-*（视频模型纳入目录展示与分类），再剔除非生成式模式（skipPatterns）。
// 返回候选（去重、稳定排序）与被剔除条数（只计"非生成式/被模式剔除"；重复条目只去重不计数）。
func FilterCatalog(all []string) (kept []string, skipped int) {
	seen := make(map[string]bool, len(all))
	for _, id := range all {
		lower := strings.ToLower(strings.TrimSpace(id))
		if lower == "" {
			continue
		}
		if !strings.HasPrefix(lower, "gemini-") && !strings.HasPrefix(lower, "veo-") {
			skipped++
			continue
		}
		skip := false
		for _, p := range skipPatterns {
			if strings.Contains(lower, p) {
				skip = true
				break
			}
		}
		if skip {
			skipped++
			continue
		}
		if seen[lower] {
			continue
		}
		seen[lower] = true
		kept = append(kept, strings.TrimSpace(id))
	}
	sort.Strings(kept)
	return kept, skipped
}

// Diff 是本地已知与上游候选的三类差异（各列表稳定排序）。
type Diff struct {
	New      []string `json:"new"`
	Existing []string `json:"existing"`
	Removed  []string `json:"removed"`
}

// ComputeDiff 计算差异：local 为本地已知模型（models ∪ disabled ∪ custom），
// upstream 为上游候选。New=上游有本地无；Existing=两侧都有；Removed=本地有上游无。
func ComputeDiff(local, upstream []string) Diff {
	up := make(map[string]bool, len(upstream))
	for _, m := range upstream {
		up[m] = true
	}
	lo := make(map[string]bool, len(local))
	for _, m := range local {
		lo[m] = true
	}
	d := Diff{New: []string{}, Existing: []string{}, Removed: []string{}}
	for m := range lo {
		if up[m] {
			d.Existing = append(d.Existing, m)
		} else {
			d.Removed = append(d.Removed, m)
		}
	}
	for m := range up {
		if !lo[m] {
			d.New = append(d.New, m)
		}
	}
	sort.Strings(d.New)
	sort.Strings(d.Existing)
	sort.Strings(d.Removed)
	return d
}

// IsSuspicious 判定本次候选数相对上次可信快照是否异常骤降
// （低于上一快照的 minKeptRatioPercent%）：可能为分页失败/过滤规则变化/上游结构变化，
// 此时不得覆盖可信快照、不得提供可执行的批量移除，等待管理员显式确认。
// prevCount <= 0（无历史快照）不判可疑。
func IsSuspicious(prevCount, newCount int) bool {
	if prevCount <= 0 {
		return false
	}
	return newCount*100 < prevCount*minKeptRatioPercent
}

// IsSignatureError 判定上游错误信息是否指向查询签名失效（与更新模型.bat 的判据一致）：
// 命中时交由上层做一次签名自愈重试。
func IsSignatureError(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "signature") || strings.Contains(l, "not found") || strings.Contains(l, "not be found")
}

// BundleURLPrefix 是上游前端 bundle 的固定地址前缀（Google 打包器固定域名与路径），
// 单独导出为变量而非嵌进正则：离线测试可改写成 mock 地址，避免测试依赖外网。
//
//nolint:gochecknoglobals // 测试可覆盖的地址前缀
var BundleURLPrefix = "https://www.gstatic.com/_/mss/boq-cloud-client/_/js/k=boq-cloud-client.AiPlatformMicroUi"

// 签名自愈用的正则（与更新模型.bat 的 Find-FreshSignature 一致）。
//
// 说明：bat 的 .NET 正则给"操作名 → 签名"之间留 4000 字符窗口；Go 的 RE2 上限为
// 1000（{0,4000} 会直接编译失败）。这里改用同一行内的惰性无界匹配：RE2 保证线性时间，
// 惰性匹配天然取"锚点之后最近的 @Signature"，与 bat 的语义一致且不受窗口限制。
//
//nolint:gochecknoglobals // 编译期常量正则
var (
	htmlEscapedEq    = regexp.MustCompile(`\\{1,2}u003d`)
	htmlEscapedAmp   = regexp.MustCompile(`\\{1,2}u0026`)
	signaturePattern = regexp.MustCompile(`request\("ListPublisherModel",[^\r\n]*?@Signature\(bytes:\s*"([^"]+)"\)`)
)

// htmlHeaders 是签名自愈抓控制台页面用的请求头。
func htmlHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "text/html,application/xhtml+xml")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("User-Agent", UserAgent)
	return h
}

// FindFreshSignature 在缓存/内置签名失效时，从上游前端 bundle 重新提取签名：
// 抓控制台页面（consolePageURL）→ 定位 AiPlatformMicroUi bundle → 正则提取
// ListPublisherModel 的 @Signature。consolePageURL 可注入，便于离线测试。
//
// 注意：必须锚定 request("ListPublisherModel", ——直接匹配操作名会命中其前缀
// ListPublisherModelRegional（另一个操作，签名不同）；惰性匹配取最近的 @Signature。
func FindFreshSignature(ctx context.Context, doer Doer, consolePageURL string) (string, error) {
	status, html, err := doer.Do(ctx, http.MethodGet, consolePageURL, htmlHeaders(), nil)
	if err != nil {
		return "", fmt.Errorf("error: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("抓取控制台页面失败: HTTP %d", status)
	}
	unescaped := htmlEscapedAmp.ReplaceAllString(htmlEscapedEq.ReplaceAllString(string(html), "="), "&")

	// 惰性路径编译：BundleURLPrefix 可被测试覆盖，故不能预编译成包级常量正则。
	bundlePattern := regexp.MustCompile(regexp.QuoteMeta(BundleURLPrefix) + `[^"'\\ ]+`)
	bundleURL := bundlePattern.FindString(unescaped)
	if bundleURL == "" {
		return "", errors.New("未能在控制台页面定位 AiPlatformMicroUi bundle")
	}
	jsHeaders := http.Header{}
	jsHeaders.Set("Accept", "*/*")
	jsHeaders.Set("User-Agent", UserAgent)
	status, js, err := doer.Do(ctx, http.MethodGet, bundleURL, jsHeaders, nil)
	if err != nil {
		return "", fmt.Errorf("error: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("抓取上游 bundle 失败: HTTP %d", status)
	}
	m := signaturePattern.FindStringSubmatch(string(js))
	if m == nil {
		return "", errors.New("未能在 bundle 中提取 ListPublisherModel 签名")
	}
	return m[1], nil
}
