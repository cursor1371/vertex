package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/catalog"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// catalogMock 是可编程的目录 mock 上游：按请求路径与调用序号返回预设响应并计数。
type catalogMock struct {
	handle func(path string, body []byte, hit int) (status int, resp string)
	hits   []string
	mu     sync.Mutex
}

func (m *catalogMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.hits = append(m.hits, r.URL.Path)
	hit := len(m.hits)
	m.mu.Unlock()

	status, resp := m.handle(r.URL.Path, body, hit)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(resp))
}

func (m *catalogMock) hitCount(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.hits {
		if strings.Contains(h, substr) {
			n++
		}
	}
	return n
}

// catalogPageJSON 构造一页 ListPublisherModel 响应（api 包测试使用）。
func catalogPageJSON(ids []string, nextToken string) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, `{"name":"publishers/google/models/`+id+`"}`)
	}
	return `[{"results":[{"data":{"ui":{"listPublisherModel":{"data":[` +
		strings.Join(items, ",") + `],"nextPageToken":"` + nextToken + `"}}}}]}]`
}

// catalogFetchFixture 建立目录拉取测试环境：临时 models.json/model_settings.json
// 与可编程 mock 上游，并返回 fixture 与模型目录路径。
func catalogFetchFixture(t *testing.T, mockHandler http.HandlerFunc, modelsJSON, settingsJSON string) (*testFixture, string) {
	t.Helper()
	dir := t.TempDir()
	if modelsJSON == "" {
		modelsJSON = `{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(modelsJSON), 0o644); err != nil {
		t.Fatalf("写入 models.json 失败: %v", err)
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "model_settings.json"), []byte(settingsJSON), 0o644); err != nil {
			t.Fatalf("写入 model_settings.json 失败: %v", err)
		}
	}
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	config.InvalidateModelSettingsCache()
	t.Cleanup(func() {
		config.InvalidateModelsCache()
		config.InvalidateModelSettingsCache()
	})

	f := newTestServerCustomMock(t, mockHandler, func(c *config.AppConfig) {
		c.AdminPassword = "test-admin-pw"
	})
	return f, dir
}

// overrideCatalogEndpoint 把目录端点指向 mock 上游（测试结束恢复）。
func overrideCatalogEndpoint(t *testing.T, url string) {
	t.Helper()
	prev, _ := catalogURLOverride.Load().(string)
	setCatalogURLOverride(url)
	t.Cleanup(func() { setCatalogURLOverride(prev) })
}

// overrideCatalogConsoleURL 把签名自愈入口页指向 mock（测试结束恢复）。
func overrideCatalogConsoleURL(t *testing.T, url string) {
	t.Helper()
	prev, _ := catalogConsoleURLOverride.Load().(string)
	setCatalogConsoleURLOverride(url)
	t.Cleanup(func() { setCatalogConsoleURLOverride(prev) })
}

// adminPostJSON 以已登录 cookie 向 admin 路径 POST JSON 并解析响应为 map。
func adminPostJSON(t *testing.T, srvURL, cookie, path string, payload any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srvURL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", uerr, string(body))
	}
	return resp.StatusCode, out
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return data
}

func asStringSlice(t *testing.T, v any) []string {
	t.Helper()
	arr, isArr := v.([]any)
	if !isArr {
		t.Fatalf("期望数组，got %T (%v)", v, v)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, isStr := item.(string)
		if !isStr {
			t.Fatalf("数组元素应为字符串: %v", item)
		}
		out = append(out, s)
	}
	return out
}

// TestAdminFetchUpstream_Success 成功路径：分页拉取 → 过滤 → 三类差异 →
// 快照写入（可信）→ 本地模型配置零改动。
func TestAdminFetchUpstream_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	mock := &catalogMock{handle: func(_ string, _ []byte, hit int) (int, string) {
		if hit == 1 {
			return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash", "gemini-3.7-flash"}, "P2")
		}
		return http.StatusOK, catalogPageJSON([]string{"veo-3.0-generate-preview"}, "")
	}}
	f, dir := catalogFetchFixture(t, mock.ServeHTTP,
		`{"models":["gemini-3.6-flash","gemini-4-argon"],"alias_map":{},"family_map":{}}`,
		`{"disabled_models":["gemini-old-thing"],"custom_models":["gemini-4-argon"],"model_options":{}}`)
	overrideCatalogEndpoint(t, f.mockUpstream.URL+"/batchGraphql")

	modelsBefore := readFileBytes(t, filepath.Join(dir, "models.json"))
	settingsBefore := readFileBytes(t, filepath.Join(dir, "model_settings.json"))

	cookie := adminLogin(t, f.server.URL)
	status, resp := adminPostJSON(t, f.server.URL, cookie, "/api/admin/models/fetch-upstream", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("状态码 %d (resp=%v)", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("ok 应为 true: %v", resp)
	}
	if resp["via"] != "前置代理/直连" {
		t.Fatalf("via=%v, want 前置代理/直连", resp["via"])
	}
	if resp["kept"] != float64(3) || resp["discovered"] != float64(3) {
		t.Fatalf("计数不符: kept=%v discovered=%v", resp["kept"], resp["discovered"])
	}
	if resp["pages"] != float64(2) {
		t.Fatalf("pages=%v, want 2", resp["pages"])
	}
	if resp["suspicious"] != false || resp["snapshot_written"] != true || resp["removed_confirmed"] != true {
		t.Fatalf("快照语义不符: %v", resp)
	}
	if got := asStringSlice(t, resp["new"]); len(got) != 2 || got[0] != "gemini-3.7-flash" || got[1] != "veo-3.0-generate-preview" {
		t.Fatalf("new=%v", got)
	}
	if got := asStringSlice(t, resp["existing"]); len(got) != 1 || got[0] != "gemini-3.6-flash" {
		t.Fatalf("existing=%v", got)
	}
	if got := asStringSlice(t, resp["removed"]); len(got) != 1 || got[0] != "gemini-old-thing" {
		t.Fatalf("removed=%v", got)
	}
	if got := asStringSlice(t, resp["custom_unseen"]); len(got) != 1 || got[0] != "gemini-4-argon" {
		t.Fatalf("custom_unseen=%v（本地自定义应单列）", got)
	}

	// 快照写入且可信；本地模型配置逐字节零改动。
	snap, err := config.LoadCatalogSnapshot()
	if err != nil || snap == nil {
		t.Fatalf("快照应已写入: %v %v", snap, err)
	}
	if !snap.Complete || len(snap.Models) != 3 || snap.Signature != catalog.DefaultSignature {
		t.Fatalf("快照内容不符: %+v", snap)
	}
	if len(snap.Models) != 3 || snap.Models[0] != "gemini-3.6-flash" || snap.Models[2] != "veo-3.0-generate-preview" {
		t.Fatalf("快照候选不符: %v", snap.Models)
	}
	if got := readFileBytes(t, filepath.Join(dir, "models.json")); string(got) != string(modelsBefore) {
		t.Fatalf("获取上游不得改动 models.json:\n%s", got)
	}
	if got := readFileBytes(t, filepath.Join(dir, "model_settings.json")); string(got) != string(settingsBefore) {
		t.Fatalf("获取上游不得改动 model_settings.json:\n%s", got)
	}
}

// TestAdminFetchUpstream_FailureKeepsSnapshotAndConfig 失败路径：
// ok=false + 可读错误；旧快照的 Models 保留、仅更新 last_error；本地配置零改动。
func TestAdminFetchUpstream_FailureKeepsSnapshotAndConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	mock := &catalogMock{handle: func(_ string, _ []byte, _ int) (int, string) {
		return http.StatusOK, `[{"results":[{"errors":[{"message":"upstream boom"}]}]}]`
	}}
	f, dir := catalogFetchFixture(t, mock.ServeHTTP,
		`{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`, "")

	// 预置可信旧快照
	oldModels := []string{"gemini-3.6-flash", "gemini-3.5-flash"}
	if err := config.SaveCatalogSnapshot(&config.CatalogSnapshot{
		FetchedAt: "2026-10-01T00:00:00Z", Source: catalog.Parent, Complete: true, Models: oldModels,
	}); err != nil {
		t.Fatalf("预置快照失败: %v", err)
	}
	overrideCatalogEndpoint(t, f.mockUpstream.URL+"/batchGraphql")

	modelsBefore := readFileBytes(t, filepath.Join(dir, "models.json"))
	cookie := adminLogin(t, f.server.URL)
	status, resp := adminPostJSON(t, f.server.URL, cookie, "/api/admin/models/fetch-upstream", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("状态码 %d (resp=%v)", status, resp)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("失败拉取 ok 应为 false: %v", resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "upstream boom") {
		t.Fatalf("错误应含上游 message: %v", resp["error"])
	}

	snap, err := config.LoadCatalogSnapshot()
	if err != nil || snap == nil {
		t.Fatalf("快照应存在: %v %v", snap, err)
	}
	if len(snap.Models) != 2 || snap.Models[1] != "gemini-3.5-flash" {
		t.Fatalf("失败不得覆盖旧快照候选: %v", snap.Models)
	}
	if !strings.Contains(snap.LastError, "upstream boom") {
		t.Fatalf("last_error 应记录失败原因: %q", snap.LastError)
	}
	if got := readFileBytes(t, filepath.Join(dir, "models.json")); string(got) != string(modelsBefore) {
		t.Fatalf("失败拉取不得改动 models.json:\n%s", got)
	}
}

// TestAdminFetchUpstream_SignatureSelfHeal 签名失效自愈：首次请求触发自愈
// （控制台页 → bundle → 新签名），重试成功并把新签名写入缓存文件。
func TestAdminFetchUpstream_SignatureSelfHeal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	var mockBase string
	mock := &catalogMock{handle: func(path string, body []byte, _ int) (int, string) {
		switch path {
		case "/console":
			return http.StatusOK, `<html><script src="` + mockBase + `/bundle/x.js"></script></html>`
		case "/bundle/x.js":
			return http.StatusOK, `function Y(){return request("ListPublisherModel",{},{},null,@Signature(bytes:"FRESH-SIG"));}`
		default:
			if strings.Contains(string(body), `"STALE"`) {
				return http.StatusOK, `[{"results":[{"errors":[{"message":"Query signature is invalid"}]}]}]`
			}
			return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash"}, "")
		}
	}}
	f, dir := catalogFetchFixture(t, mock.ServeHTTP,
		`{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`, "")
	mockBase = f.mockUpstream.URL
	overrideCatalogEndpoint(t, f.mockUpstream.URL+"/batchGraphql")
	overrideCatalogConsoleURL(t, f.mockUpstream.URL+"/console")

	// 预置过期签名缓存（与 bat 共用同一文件）
	if err := os.WriteFile(filepath.Join(dir, "model_catalog_sig.txt"), []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// bundle 地址前缀改写为 mock（离线测试）
	prevPrefix := catalog.BundleURLPrefix
	catalog.BundleURLPrefix = f.mockUpstream.URL + "/bundle"
	t.Cleanup(func() { catalog.BundleURLPrefix = prevPrefix })

	cookie := adminLogin(t, f.server.URL)
	status, resp := adminPostJSON(t, f.server.URL, cookie, "/api/admin/models/fetch-upstream", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("状态码 %d (resp=%v)", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("自愈后应成功: %v", resp)
	}
	sigRaw := strings.TrimSpace(string(readFileBytes(t, filepath.Join(dir, "model_catalog_sig.txt"))))
	if sigRaw != "FRESH-SIG" {
		t.Fatalf("新签名应写入缓存文件: %q", sigRaw)
	}
	if n := mock.hitCount("/bundle"); n != 1 {
		t.Fatalf("bundle 应只抓取一次: %d", n)
	}
}

// TestCatalogRefreshSignatureSingleflight 并发自愈去重：bundle 只抓一次，结果一致。
func TestCatalogRefreshSignatureSingleflight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	var mockBase string
	mock := &catalogMock{handle: func(path string, _ []byte, _ int) (int, string) {
		switch path {
		case "/console":
			return http.StatusOK, `<script src="` + mockBase + `/bundle/x.js"></script>`
		case "/bundle/x.js":
			time.Sleep(120 * time.Millisecond) // 拉长窗口，让第二个协程落在 singleflight 合并期内
			return http.StatusOK, `request("ListPublisherModel",{},{},null,@Signature(bytes:"FRESH-SIG"))`
		default:
			return http.StatusNotFound, "{}"
		}
	}}
	f, _ := catalogFetchFixture(t, mock.ServeHTTP, "", "")
	mockBase = f.mockUpstream.URL
	overrideCatalogConsoleURL(t, f.mockUpstream.URL+"/console")
	prevPrefix := catalog.BundleURLPrefix
	catalog.BundleURLPrefix = f.mockUpstream.URL + "/bundle"
	t.Cleanup(func() { catalog.BundleURLPrefix = prevPrefix })

	adm := &AdminHandler{handler: handler{vc: f.vc}}
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sig, err := adm.refreshCatalogSignature(context.Background(), "STALE")
			results[idx], errs[idx] = sig, err
		}(i)
	}
	wg.Wait()

	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("并发自愈第 %d 路失败: %v", i, errs[i])
		}
		if results[i] != "FRESH-SIG" {
			t.Fatalf("第 %d 路签名=%q", i, results[i])
		}
	}
	if n := mock.hitCount("/bundle"); n != 1 {
		t.Fatalf("并发自愈应只抓一次 bundle: %d", n)
	}
}

// TestAdminFetchUpstream_SuspiciousRequiresConfirmation 数量骤降：
// 首次拉取标记 suspicious 且不覆盖快照、removed 不可执行；accept_anomaly=true 后接受并记录时间。
func TestAdminFetchUpstream_SuspiciousRequiresConfirmation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	mock := &catalogMock{handle: func(_ string, _ []byte, _ int) (int, string) {
		return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash", "gemini-3.7-flash"}, "")
	}}
	f, _ := catalogFetchFixture(t, mock.ServeHTTP,
		`{"models":["gemini-3.6-flash","gemini-3.7-flash"],"alias_map":{},"family_map":{}}`, "")

	// 预置一个"大"可信快照（100 条），本次只发现 2 条 → 骤降
	oldModels := make([]string, 0, 100)
	for i := range 100 {
		oldModels = append(oldModels, "gemini-old-"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if err := config.SaveCatalogSnapshot(&config.CatalogSnapshot{
		FetchedAt: "2026-10-01T00:00:00Z", Source: catalog.Parent, Complete: true, Models: oldModels,
	}); err != nil {
		t.Fatalf("预置快照失败: %v", err)
	}
	overrideCatalogEndpoint(t, f.mockUpstream.URL+"/batchGraphql")

	cookie := adminLogin(t, f.server.URL)
	_, resp := adminPostJSON(t, f.server.URL, cookie, "/api/admin/models/fetch-upstream", map[string]any{})
	if resp["suspicious"] != true {
		t.Fatalf("骤降应标记 suspicious: %v", resp)
	}
	if resp["snapshot_written"] != false || resp["removed_confirmed"] != false {
		t.Fatalf("suspicious 不得写快照、removed 不可执行: %v", resp)
	}
	snap, _ := config.LoadCatalogSnapshot()
	if snap == nil || len(snap.Models) != 100 {
		t.Fatalf("suspicious 不得覆盖上次可信快照: %+v", snap)
	}

	// 管理员显式确认接受异常快照
	_, resp2 := adminPostJSON(t, f.server.URL, cookie, "/api/admin/models/fetch-upstream",
		map[string]any{"accept_anomaly": true})
	if resp2["ok"] != true || resp2["snapshot_written"] != true {
		t.Fatalf("确认后应写入快照: %v", resp2)
	}
	confirmedAt, _ := resp2["confirmed_at"].(string)
	if confirmedAt == "" {
		t.Fatalf("应记录确认时间: %v", resp2)
	}
	snap2, _ := config.LoadCatalogSnapshot()
	if snap2 == nil || len(snap2.Models) != 2 || snap2.ConfirmedAt != confirmedAt {
		t.Fatalf("确认后的快照不符: %+v", snap2)
	}
}

// TestAdminFetchUpstream_TimeoutWritesNoSnapshot 超时：返回可读错误、无快照候选写入、本地配置零改动。
func TestAdminFetchUpstream_TimeoutWritesNoSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	mock := &catalogMock{handle: func(_ string, _ []byte, _ int) (int, string) {
		time.Sleep(2 * time.Second)
		return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash"}, "")
	}}
	f, dir := catalogFetchFixture(t, mock.ServeHTTP,
		`{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`, "")
	overrideCatalogEndpoint(t, f.mockUpstream.URL+"/batchGraphql")

	prevTimeout := catalogFetchTotalTimeout
	catalogFetchTotalTimeout = 300 * time.Millisecond
	t.Cleanup(func() { catalogFetchTotalTimeout = prevTimeout })

	modelsBefore := readFileBytes(t, filepath.Join(dir, "models.json"))
	cookie := adminLogin(t, f.server.URL)
	_, resp := adminPostJSON(t, f.server.URL, cookie, "/api/admin/models/fetch-upstream", map[string]any{})
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("超时应返回 ok=false: %v", resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "超时") && !strings.Contains(msg, "取消") {
		t.Fatalf("错误应说明超时/取消: %v", resp["error"])
	}
	snap, _ := config.LoadCatalogSnapshot()
	if snap != nil && len(snap.Models) > 0 {
		t.Fatalf("超时不得写入快照候选: %+v", snap)
	}
	if got := readFileBytes(t, filepath.Join(dir, "models.json")); string(got) != string(modelsBefore) {
		t.Fatalf("超时不得改动 models.json:\n%s", got)
	}
}

// failSecondHopDialer 让所有"带节点 URI"的会话创建失败（第二跳启动失败），
// 用于验证目录拉取的节点失败回退；不带 URI 的前置代理/直连路径不受影响。
type failSecondHopDialer struct{ trackingDialer }

func (d *failSecondHopDialer) CreateDialer(string, string) (func(ctx context.Context, network, addr string) (net.Conn, error), func(), error) {
	return nil, nil, errors.New("节点解析失败（测试注入）")
}

// TestCatalogRunNodeFallback 节点失败回退：坏节点记录 attempt 后回退到下一个候选。
func TestCatalogRunNodeFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	mock := &catalogMock{handle: func(_ string, _ []byte, _ int) (int, string) {
		return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash"}, "")
	}}
	f := newTestServerCustomMock(t, mock.ServeHTTP, nil, &failSecondHopDialer{})
	overrideCatalogEndpoint(t, f.mockUpstream.URL+"/batchGraphql")

	adm := &AdminHandler{handler: handler{vc: f.vc}}
	calls := int32(0)
	via, attempts, err := adm.catalogRunWithCandidates(context.Background(),
		[]catalogCandidate{{rawURI: "socks5://127.0.0.1:1", name: "坏节点"}, {rawURI: "", name: "前置代理/直连"}},
		"test-catalog", 5, func(doer catalog.Doer) error {
			atomic.AddInt32(&calls, 1)
			_, ferr := catalog.Fetch(context.Background(), doer, catalogEndpoint(), catalog.DefaultSignature)
			return ferr
		})
	if err != nil {
		t.Fatalf("应回退成功: %v (attempts=%v)", err, attempts)
	}
	if via != "前置代理/直连" {
		t.Fatalf("via=%q", via)
	}
	if len(attempts) != 1 || attempts[0].Target != "坏节点" || attempts[0].Error == "" {
		t.Fatalf("应记录坏节点的失败摘要: %v", attempts)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("只有成功候选应真正执行拉取: %d", calls)
	}
}

// TestAdminFetchUpstream_RequiresAuth 未登录请求被拒（requireAdmin 覆盖新路由）。
func TestAdminFetchUpstream_RequiresAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	mock := &catalogMock{handle: func(_ string, _ []byte, _ int) (int, string) {
		return http.StatusOK, catalogPageJSON([]string{"gemini-3.6-flash"}, "")
	}}
	f, _ := catalogFetchFixture(t, mock.ServeHTTP, "", "")

	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/admin/models/fetch-upstream", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", resp.StatusCode)
	}
	if n := mock.hitCount("/"); n != 0 {
		t.Fatalf("未登录不得触发任何上游请求: %d", n)
	}
}
