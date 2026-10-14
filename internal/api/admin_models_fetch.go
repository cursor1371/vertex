package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/bsfdsagfadg/vertex/internal/engine/catalog"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/netx"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

// catalogFetchTotalTimeout 是整次"获取上游模型"的总截止时间（父级 ctx）：
// 覆盖节点尝试、全部分页与签名自愈。自愈的单请求上限只是该父级约束内的
// 单请求上限，实际运行时间不会超过本截止时间。
//
//nolint:gochecknoglobals // 测试可缩短以覆盖超时路径
var catalogFetchTotalTimeout = 120 * time.Second

// 会话超时（秒）：目录拉取与自愈 bundle 分别给不同额度（bundle 体积大）。
const (
	catalogSessionTimeoutSec     = 60
	catalogHealSessionTimeoutSec = 180
)

// catalogURLOverride 允许测试把目录端点指向 mock 上游（生产恒为空）。
//
//nolint:gochecknoglobals // 测试注入点
var catalogURLOverride atomic.Value

// catalogConsoleURLOverride 允许测试把签名自愈的入口页面指向 mock（生产恒为空）。
//
//nolint:gochecknoglobals // 测试注入点
var catalogConsoleURLOverride atomic.Value

// setCatalogURLOverride 仅供测试使用：设置目录端点覆盖（空串恢复默认）。
func setCatalogURLOverride(u string) { catalogURLOverride.Store(u) }

// setCatalogConsoleURLOverride 仅供测试使用：设置控制台页面覆盖。
func setCatalogConsoleURLOverride(u string) { catalogConsoleURLOverride.Store(u) }

// catalogEndpoint 返回目录拉取端点（含匿名 key 与 prettyPrint），测试可覆盖。
func catalogEndpoint() string {
	if v, ok := catalogURLOverride.Load().(string); ok && v != "" {
		return v
	}
	return vertex.AnonBatchGraphqlURL() + "?key=" + vertex.AnonAPIKey() + "&prettyPrint=false"
}

// catalogConsoleURL 返回签名自愈的控制台入口页，测试可覆盖。
func catalogConsoleURL() string {
	if v, ok := catalogConsoleURLOverride.Load().(string); ok && v != "" {
		return v
	}
	return catalog.ConsolePageURL
}

// sessionDoer 把一次节点会话适配为 catalog.Doer。
type sessionDoer struct{ sess *transport.Session }

func (d sessionDoer) Do(ctx context.Context, method, url string, header http.Header, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return d.sess.DoAndRead(ctx, method, url, toSessionHeader(header), reader)
}

// toSessionHeader 把 net/http 请求头转换为会话层（tls-client 的 fhttp）头。
// 二者底层都是 map[string][]string，此处只做类型搬运，避免 fhttp 类型渗入 catalog 包。
func toSessionHeader(h http.Header) transport.Header {
	out := transport.Header{}
	for k, vs := range h {
		out[k] = append(out[k], vs...)
	}
	return out
}

// directDoer 是节点会话不可用时的直连兜底（系统代理/直连），与更新模型.bat 的
// "未配置代理则直连"语义一致。
type directDoer struct{ client *http.Client }

func (d directDoer) Do(ctx context.Context, method, url string, header http.Header, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("error: %w", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("error: %w", err)
	}
	return resp.StatusCode, data, nil
}

// catalogCandidate 是一次拉取尝试目标。
type catalogCandidate struct {
	rawURI string
	name   string
}

// catalogAttempt 是一次尝试的结果摘要（失败诊断用；成功来源记录在 via）。
type catalogAttempt struct {
	Target string `json:"target"`
	Error  string `json:"error,omitempty"`
}

// catalogCandidates 构造顺序尝试目标：严格节点池 → 宽松节点池 → 前置代理/直连兜底。
//
// 节点池的 SelectForParallel 系列只返回候选（不发请求）；实际请求由 catalogRun
// 按顺序发起：首个成功即停止，全部失败返回逐条摘要。节点池为空时自然只剩兜底项。
func (adm *AdminHandler) catalogCandidates() []catalogCandidate {
	out := make([]catalogCandidate, 0, 4)
	seen := make(map[string]bool, 4)
	add := func(nodes []exitpool.Node) {
		for _, n := range nodes {
			if n.RawURI == "" || seen[n.RawURI] {
				continue
			}
			seen[n.RawURI] = true
			name := strings.TrimSpace(n.Name)
			if name == "" {
				name = "未命名节点"
			}
			out = append(out, catalogCandidate{rawURI: n.RawURI, name: name})
		}
	}
	if adm.deps.Exit != nil {
		add(adm.deps.Exit.SelectForParallel(3, false))
		add(adm.deps.Exit.SelectForParallelRelaxed(3, false))
	}
	out = append(out, catalogCandidate{rawURI: "", name: "前置代理/直连"})
	return out
}

// catalogRun 以候选顺序执行一次多请求操作（目录拉取或签名自愈）。
// 所有尝试共用同一父级 ctx；ctx 取消后立即停止且不再发起新请求。
func (adm *AdminHandler) catalogRun(ctx context.Context, stage string, timeoutSec int,
	fn func(doer catalog.Doer) error) (via string, attempts []catalogAttempt, err error) {
	return adm.catalogRunWithCandidates(ctx, adm.catalogCandidates(), stage, timeoutSec, fn)
}

// catalogRunWithCandidates 是 catalogRun 的可测试内核（候选列表可注入）。
func (adm *AdminHandler) catalogRunWithCandidates(ctx context.Context, candidates []catalogCandidate,
	stage string, timeoutSec int, fn func(doer catalog.Doer) error) (via string, attempts []catalogAttempt, err error) {
	attempts = []catalogAttempt{}
	var lastErr error
	for _, c := range candidates {
		if cerr := ctx.Err(); cerr != nil {
			return "", attempts, cerr
		}
		var doer catalog.Doer
		var closeSess func()
		if adm.vc != nil && adm.vc.Net() != nil {
			sess, serr := adm.vc.Net().CreateSession(timeoutSec, c.rawURI, stage)
			if serr != nil {
				attempts = append(attempts, catalogAttempt{Target: c.name, Error: serr.Error()})
				lastErr = serr
				continue
			}
			doer = sessionDoer{sess: sess}
			closeSess = sess.Close
		} else if c.rawURI == "" {
			// 节点会话不可用（测试/极简装配）时的直连兜底。
			doer = directDoer{client: netx.NewHTTPClient(time.Duration(timeoutSec) * time.Second)}
			closeSess = func() {}
		} else {
			attempts = append(attempts, catalogAttempt{Target: c.name, Error: "节点会话不可用"})
			lastErr = errors.New("节点会话不可用")
			continue
		}

		rerr := fn(doer)
		closeSess()
		if rerr == nil {
			return c.name, attempts, nil
		}
		attempts = append(attempts, catalogAttempt{Target: c.name, Error: rerr.Error()})
		lastErr = rerr
		if ctx.Err() != nil {
			return "", attempts, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的拉取通道")
	}
	return "", attempts, lastErr
}

// fetchCatalogOnce 用指定签名拉取一次完整目录。
func (adm *AdminHandler) fetchCatalogOnce(ctx context.Context, sig string) (catalog.Result, string, []catalogAttempt, error) {
	var res catalog.Result
	via, attempts, err := adm.catalogRun(ctx, "admin-model-catalog", catalogSessionTimeoutSec, func(doer catalog.Doer) error {
		r, ferr := catalog.Fetch(ctx, doer, catalogEndpoint(), sig)
		if ferr != nil {
			return ferr
		}
		res = r
		return nil
	})
	return res, via, attempts, err
}

// catalogSigGroup 是签名自愈的单飞去重组：并发自愈只抓一次大体积 bundle。
//
//nolint:gochecknoglobals // 签名自愈去重
var catalogSigGroup singleflight.Group

// refreshCatalogSignature 重新提取查询签名（singleflight 去重 + 获锁后重读缓存）。
func (adm *AdminHandler) refreshCatalogSignature(ctx context.Context, staleSig string) (string, error) {
	v, err, _ := catalogSigGroup.Do("refresh", func() (any, error) {
		// 获锁后重读缓存：并发请求里他者可能已刷新，直接复用避免重复抓 bundle。
		if cached := config.LoadCatalogSignature(); cached != "" && cached != staleSig {
			return cached, nil
		}
		var sig string
		_, _, ferr := adm.catalogRun(ctx, "admin-model-catalog-heal", catalogHealSessionTimeoutSec,
			func(doer catalog.Doer) error {
				s, herr := catalog.FindFreshSignature(ctx, doer, catalogConsoleURL())
				if herr != nil {
					return herr
				}
				sig = s
				return nil
			})
		if ferr != nil {
			return "", ferr
		}
		if strings.TrimSpace(sig) == "" {
			return "", errors.New("签名提取结果为空")
		}
		if werr := config.SaveCatalogSignature(sig); werr != nil {
			log.Printf("[Admin] [ModelCatalog] 签名缓存写入失败（本次仍使用新签名）: %v", werr)
		}
		return sig, nil
	})
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return s, nil
}

// splitCatalogDiff 把差异中的 Removed 拆为两组：
//   - removed：上游确认缺失、可执行移除的模型；
//   - customUnseen：本地自定义（custom_models）且上游未发现——单独展示、默认保留，
//     不给移除勾选（避免把"上游目录没收录的自定义模型"误当普通下架处理）。
func splitCatalogDiff(d catalog.Diff, custom []string) (removed, customUnseen []string) {
	customSet := make(map[string]bool, len(custom))
	for _, m := range custom {
		customSet[m] = true
	}
	removed = make([]string, 0, len(d.Removed))
	customUnseen = make([]string, 0)
	for _, m := range d.Removed {
		if customSet[m] {
			customUnseen = append(customUnseen, m)
			continue
		}
		removed = append(removed, m)
	}
	return removed, customUnseen
}

// catalogView 汇总上游目录快照状态与三类差异（供 GET /api/admin/models）。
//
// 无可信快照（文件不存在）时返回 (nil, nil)：前端据此提示"尚未获取过上游目录"。
// removed_confirmed 仅在快照完整、无失败记录时置真——"已移除模型"只在此刻可执行；
// 最近一次拉取失败（stale）时只提示、不给移除勾选。
func catalogView() (status map[string]any, diff map[string]any) {
	snap, err := config.LoadCatalogSnapshot()
	if err != nil || snap == nil {
		return nil, nil
	}
	trusted := snap.Complete && len(snap.Models) > 0 && snap.LastError == ""
	status = map[string]any{
		"fetched_at":   snap.FetchedAt,
		"count":        len(snap.Models),
		"pages":        snap.Pages,
		"stale":        snap.LastError != "",
		"last_error":   snap.LastError,
		"confirmed_at": snap.ConfirmedAt,
	}
	d := catalog.ComputeDiff(config.KnownModels(), snap.Models)
	removed, customUnseen := splitCatalogDiff(d, config.CustomModels())
	diff = map[string]any{
		"new":               d.New,
		"existing":          d.Existing,
		"removed":           removed,
		"custom_unseen":     customUnseen,
		"removed_confirmed": trusted,
	}
	return status, diff
}

// recordCatalogFailure 把失败原因写入快照的 last_error（不覆盖 Models；无快照时新建空快照）。
//
// 语义：最近一次拉取失败后，快照的"可信集合"判定失效（Complete && LastError == ""），
// 据此计算的「已移除模型」不再可执行；旧 Models 保留供展示与回退。
func (adm *AdminHandler) recordCatalogFailure(msg string) {
	prev, lerr := config.LoadCatalogSnapshot()
	if lerr != nil || prev == nil {
		prev = &config.CatalogSnapshot{Source: catalog.Parent, Models: []string{}}
	}
	prev.LastError = msg
	if werr := config.SaveCatalogSnapshot(prev); werr != nil {
		log.Printf("[Admin] [ModelCatalog] 失败原因写入快照失败: %v", werr)
	}
}

// catalogLastSuccessAt 返回最近一次可信快照的拉取时间（无可信快照返回空串）。
func catalogLastSuccessAt() string {
	snap, err := config.LoadCatalogSnapshot()
	if err != nil || snap == nil || !snap.Complete || len(snap.Models) == 0 {
		return ""
	}
	return snap.FetchedAt
}

// adminFetchUpstreamModels 处理 POST /api/admin/models/fetch-upstream。
//
// 由服务端经节点池中的节点访问上游模型目录（不硬编码本地代理地址），
// 只写目录快照（config/model_catalog.json），**绝不改动本地模型配置**：
// 导入新模型、移除本地模型均由用户在管理面板显式确认后另行保存。
//
// 安全语义：
//   - 只有完整分页成功、过滤后非空、且通过合理性校验的结果才写入快照；
//   - 候选数相对上次可信快照骤降（<50%）标记 suspicious：不覆盖快照、removed 不可执行，
//     由管理员带 accept_anomaly=true 重发显式确认后接受（记录 confirmed_at）；
//   - 失败/取消/超时一律不写快照（仅记录 last_error），本地配置不受影响。
func (adm *AdminHandler) adminFetchUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	var body struct {
		AcceptAnomaly bool `json:"accept_anomaly"`
	}
	if raw, rerr := io.ReadAll(r.Body); rerr == nil && len(bytes.TrimSpace(raw)) > 0 {
		if uerr := json.Unmarshal(raw, &body); uerr != nil {
			writeJSON(w, http.StatusBadRequest, adminErr("请求体解析失败 (invalid JSON)"))
			return
		}
	}

	// 总截止时间：覆盖节点尝试、分页与签名自愈。
	ctx, cancel := context.WithTimeout(r.Context(), catalogFetchTotalTimeout)
	defer cancel()

	sig := config.LoadCatalogSignature()
	if sig == "" {
		sig = catalog.DefaultSignature
	}

	res, via, attempts, err := adm.fetchCatalogOnce(ctx, sig)
	if err != nil && catalog.IsSignatureError(err.Error()) {
		log.Printf("[Admin] [ModelCatalog] 查询签名疑似失效，尝试自愈: %v", err)
		if newSig, herr := adm.refreshCatalogSignature(ctx, sig); herr == nil {
			if newSig != "" && newSig != sig {
				sig = newSig
				res, via, attempts, err = adm.fetchCatalogOnce(ctx, sig)
			}
		} else {
			log.Printf("[Admin] [ModelCatalog] 签名自愈失败: %v", herr)
		}
	}

	if err != nil {
		msg := err.Error()
		if ctx.Err() != nil {
			msg = "拉取超时或被取消：" + msg
		}
		adm.recordCatalogFailure(msg)
		log.Printf("[Admin] [ModelCatalog] 获取上游模型失败: %s", msg)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              false,
			"error":           msg,
			"via":             via,
			"attempts":        attempts,
			"last_success_at": catalogLastSuccessAt(),
		})
		return
	}

	// 取消/超时后不得提交快照（即便本轮已收到数据）。
	if ctx.Err() != nil {
		msg := "拉取已被取消或超时，未更新快照"
		adm.recordCatalogFailure(msg + "：" + ctx.Err().Error())
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": msg, "via": via, "attempts": attempts,
			"last_success_at": catalogLastSuccessAt(),
		})
		return
	}

	kept, skipped := catalog.FilterCatalog(res.All)
	if len(kept) == 0 {
		msg := fmt.Sprintf("上游目录过滤后为空（发现 %d 条），疑似上游异常，未更新快照", len(res.All))
		adm.recordCatalogFailure(msg)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": msg, "via": via, "attempts": attempts,
			"last_success_at": catalogLastSuccessAt(),
		})
		return
	}

	now := time.Now().UTC()
	prev, _ := config.LoadCatalogSnapshot()
	suspicious := prev != nil && prev.Complete && len(prev.Models) > 0 &&
		catalog.IsSuspicious(len(prev.Models), len(kept))

	confirmedAt := ""
	snapWritten := false
	if !suspicious || body.AcceptAnomaly {
		if suspicious {
			// 管理员显式确认接受异常快照：记录确认时间作为审计。
			confirmedAt = now.Format(time.RFC3339)
			log.Printf("[Admin] [ModelCatalog] 管理员确认接受异常快照（%d → %d）",
				len(prev.Models), len(kept))
		}
		snap := &config.CatalogSnapshot{
			FetchedAt:    now.Format(time.RFC3339),
			Source:       catalog.Parent,
			Signature:    sig,
			Pages:        res.Pages,
			Complete:     true,
			ConfirmedAt:  confirmedAt,
			Models:       kept,
			SkippedCount: skipped,
		}
		if werr := config.SaveCatalogSnapshot(snap); werr != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("快照写入失败 (failed to write catalog snapshot)"))
			return
		}
		snapWritten = true
	}

	// 差异分类：本地已知集合 vs 本次候选。本地自定义且上游未发现的模型单独归类，
	// 默认保留、不提供移除勾选（见 splitCatalogDiff）。
	diff := catalog.ComputeDiff(config.KnownModels(), kept)
	removed, customUnseen := splitCatalogDiff(diff, config.CustomModels())

	entries := make([]catalog.Entry, 0, len(kept))
	for _, id := range kept {
		entries = append(entries, catalog.Classify(id))
	}

	lastSuccess := catalogLastSuccessAt()
	log.Printf("[Admin] [ModelCatalog] 获取上游模型成功: via=%s pages=%d 发现=%d 候选=%d 跳过=%d 新增=%d 已移除=%d suspicious=%v snapshot_written=%v",
		via, res.Pages, len(res.All), len(kept), skipped, len(diff.New), len(removed), suspicious, snapWritten)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"fetched_at":        now.Format(time.RFC3339),
		"via":               via,
		"attempts":          attempts,
		"discovered":        len(res.All),
		"kept":              len(kept),
		"skipped":           skipped,
		"pages":             res.Pages,
		"new":               diff.New,
		"existing":          diff.Existing,
		"removed":           removed,
		"removed_confirmed": snapWritten,
		"custom_unseen":     customUnseen,
		"entries":           entries,
		"suspicious":        suspicious,
		"snapshot_written":  snapWritten,
		"confirmed_at":      confirmedAt,
		"last_success_at":   lastSuccess,
	})
}
