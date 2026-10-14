package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// TestAdminNodeFaults_ListAndClear 覆盖「故障记录」接口的端到端行为：
// 失败上报 → 异步落库 → GET 分类统计与聚合明细 → DELETE 清空。
//
// 评审要求的具体字段都在断言里：完整错误码、HTTP 状态码、脱敏原文、首次/最近发生时间、
// 节点与发生次数，以及"只保留 1 天"的告知字段。
func TestAdminNodeFaults_ListAndClear(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "faults-api.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mgr := exitpool.NewManager(database, nil, exitpool.Hooks{}, func() int { return 1 })
	mgr.StartHealthWorker()
	t.Cleanup(mgr.StopHealthWorker)
	mgr.MergeNodes([]exitpool.Node{{RawURI: "u1", Type: "vless", Name: "node-1"}}) //nolint:exhaustruct

	// 一条带敏感 query 的 403 失败（原文必须被脱敏后入库），连报两次验证聚合计数。
	const secret = "SECRET_KEY_12345"
	errStr := `Post "https://console.example/v3/batchGraphql?key=` + secret + `&d=1": unexpected HTTP response status: 403`
	mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: errStr, Target: "console.example", Source: nodestore.ProbeSourceRequest})
	mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: errStr, Target: "console.example", Source: nodestore.ProbeSourceRequest})
	// 再加一条网络类失败（不同类别，应另成一行）。
	mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: "dial tcp: connect: connection refused", Source: nodestore.ProbeSourceRequest})

	// 排空异步健康写队列，保证故障已落库。
	mgr.StopHealthWorker()

	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Exit: mgr}}}

	rec := httptest.NewRecorder()
	adm.adminGetNodeFaults(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/faults", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", rec.Code)
	}
	var resp nodeFaultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.RetentionDays != db.FaultLogRetentionDays {
		t.Errorf("retention_days=%d, want %d（面板据此告知用户记录只保留 1 天）", resp.RetentionDays, db.FaultLogRetentionDays)
	}
	if len(resp.Classes) != 2 {
		t.Fatalf("应有 2 个失败类别, got %d: %+v", len(resp.Classes), resp.Classes)
	}
	byClass := map[string]faultClassView{}
	for _, c := range resp.Classes {
		byClass[c.Class] = c
	}
	if c := byClass[string(nodestore.FailurePermission)]; c.Count != 2 {
		t.Errorf("权限类次数=%d, want 2（同因两次应聚合计数）", c.Count)
	}
	if c := byClass[string(nodestore.FailureNodeNetwork)]; c.Count != 1 {
		t.Errorf("网络类次数=%d, want 1", c.Count)
	}

	if len(resp.Rows) != 2 {
		t.Fatalf("应有 2 条聚合记录, got %d", len(resp.Rows))
	}
	var perm, net faultRowView
	for _, r := range resp.Rows {
		if r.FailureClass == string(nodestore.FailurePermission) {
			perm = r
		}
		if r.FailureClass == string(nodestore.FailureNodeNetwork) {
			net = r
		}
	}
	if perm.NodeName != "node-1" || perm.NodeURI != "u1" {
		t.Errorf("应能定位到节点: %+v", perm)
	}
	if perm.HTTPStatus != 403 {
		t.Errorf("HTTP 状态码=%d, want 403（面板要展示完整状态码）", perm.HTTPStatus)
	}
	if perm.ErrorCode != "403" {
		t.Errorf("错误码=%q, want 403", perm.ErrorCode)
	}
	if perm.Count != 2 {
		t.Errorf("发生次数=%d, want 2", perm.Count)
	}
	if perm.FirstSeenAt == 0 || perm.LastSeenAt == 0 {
		t.Errorf("首次/最近发生时间必须可见: %+v", perm)
	}
	if perm.Target != "console.example" {
		t.Errorf("探测目标=%q, want console.example", perm.Target)
	}
	if perm.Source != nodestore.ProbeSourceRequest {
		t.Errorf("来源=%q, want %q", perm.Source, nodestore.ProbeSourceRequest)
	}
	if perm.Scope != "exit" {
		t.Errorf("所属 scope=%q, want exit（出口池写入的记录）", perm.Scope)
	}
	// 脱敏：原文里不能出现凭据，但必须保留域名与状态码等定位信息。
	if strings.Contains(perm.RawError, secret) {
		t.Errorf("原始错误泄露凭据: %q", perm.RawError)
	}
	if !strings.Contains(perm.RawError, "<redacted>") {
		t.Errorf("URL query 应被替换为 <redacted>: %q", perm.RawError)
	}
	if !strings.Contains(perm.RawError, "console.example") || !strings.Contains(perm.RawError, "403") {
		t.Errorf("脱敏不应抹掉域名/状态码等定位信息: %q", perm.RawError)
	}
	if net.RawError == "" {
		t.Error("网络类失败也必须有原文（已分类的错误同样保留详情）")
	}

	// DELETE 清空。
	rec = httptest.NewRecorder()
	adm.adminClearNodeFaults(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/nodes/faults", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d, want 200", rec.Code)
	}
	var delResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if delResp["deleted"].(float64) != 2 {
		t.Errorf("deleted=%v, want 2", delResp["deleted"])
	}

	rec = httptest.NewRecorder()
	adm.adminGetNodeFaults(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/faults", nil))
	resp = nodeFaultsResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode after clear: %v", err)
	}
	if len(resp.Rows) != 0 || len(resp.Classes) != 0 {
		t.Errorf("清空后应为空: rows=%d classes=%d", len(resp.Rows), len(resp.Classes))
	}
}

// TestAdminNodeFaults_TruncatedListDoesNotClaimCompleteness 列表被 limit 截断时，
// 接口必须同时给出全表真实总量并标记 truncated —— 否则面板会把"最近 N 条"显示成
// "共 N 次"，让用户误以为看到了全部历史（保留期只有 1 天，这个错觉代价很高）。
func TestAdminNodeFaults_TruncatedListDoesNotClaimCompleteness(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "faults-trunc.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mgr := exitpool.NewManager(database, nil, exitpool.Hooks{}, func() int { return 2 })
	mgr.StartHealthWorker()
	t.Cleanup(mgr.StopHealthWorker)
	mgr.MergeNodes([]exitpool.Node{{RawURI: "u1", Type: "vless", Name: "node-1"}}) //nolint:exhaustruct

	// 三个不同类别（签名也不同）→ 三行聚合记录
	for _, e := range []string{
		"dial tcp: connection refused",
		"unexpected HTTP response status: 403",
		"lookup a.b: no such host",
	} {
		mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: e, Source: nodestore.ProbeSourceRequest})
	}
	mgr.StopHealthWorker()

	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Exit: mgr}}}

	rec := httptest.NewRecorder()
	adm.adminGetNodeFaults(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/faults?limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d", rec.Code)
	}
	var resp nodeFaultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Rows) != 1 || resp.Total != 1 {
		t.Fatalf("limit=1 时应只返回 1 行, got rows=%d total=%d", len(resp.Rows), resp.Total)
	}
	if resp.Limit != 1 {
		t.Errorf("Limit=%d, want 1（面板要据此说明列表上限）", resp.Limit)
	}
	if resp.TotalRows != 3 {
		t.Errorf("TotalRows=%d, want 3（全表真实行数，不受 limit 影响）", resp.TotalRows)
	}
	if resp.TotalCount != 3 {
		t.Errorf("TotalCount=%d, want 3（全表累计发生次数）", resp.TotalCount)
	}
	if !resp.Truncated {
		t.Error("Truncated 应为 true（真实总量大于返回行数）")
	}
	// 分类统计走全表 GROUP BY，不受 limit 影响 —— 面板可安全声称"分类统计为全量"。
	if len(resp.Classes) != 3 {
		t.Errorf("分类统计应为全量 3 组, got %d", len(resp.Classes))
	}

	// 不传 limit 时用默认上限：数据量小于上限 → 不标记截断。
	rec = httptest.NewRecorder()
	adm.adminGetNodeFaults(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/faults", nil))
	resp = nodeFaultsResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Truncated {
		t.Error("数据量未超上限时不应标记截断")
	}
	if resp.TotalRows != 3 || resp.Total != 3 {
		t.Errorf("未截断时 Total 应等于 TotalRows: total=%d total_rows=%d", resp.Total, resp.TotalRows)
	}
}

// TestAdminNodeFaults_SourceFaithful 记录里的「来源」必须反映事件真实出处：
// 同一个节点由请求路径与自动探测分别失败时，两条记录的 source 应各自正确。
func TestAdminNodeFaults_SourceFaithful(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "faults-source.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mgr := exitpool.NewManager(database, nil, exitpool.Hooks{}, func() int { return 2 })
	mgr.StartHealthWorker()
	mgr.MergeNodes([]exitpool.Node{{RawURI: "u1", Type: "vless", Name: "node-1"}}) //nolint:exhaustruct

	mgr.ReportProbe("u1", exitpool.ProbeResult{
		OK: false, ErrStr: "dial tcp: connection refused", Source: nodestore.ProbeSourceRequest,
	})
	mgr.ReportProbe("u1", exitpool.ProbeResult{
		OK: false, ErrStr: "wsarecv: the connected host has failed to respond",
		Source: nodestore.ProbeSourceAutoProbe,
	})
	mgr.ReportProbe("u1", exitpool.ProbeResult{
		OK: false, ErrStr: "unexpected HTTP response status: 403", Source: nodestore.ProbeSourceAdminBatch,
	})
	mgr.StopHealthWorker()

	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Exit: mgr}}}
	for _, p := range getFaultRows(t, adm) {
		if p.Scope != "exit" {
			t.Errorf("scope=%q, want exit", p.Scope)
		}
	}
	// 收集出现过的来源集合。注意不能用错误码做 key：网络类失败的错误码是空串，
	// 多条网络类记录会互相覆盖（这正是本用例第一次写错的地方）。
	seen := map[string]bool{}
	for _, p := range getFaultRows(t, adm) {
		seen[p.Source] = true
	}
	for _, src := range []string{
		nodestore.ProbeSourceRequest,
		nodestore.ProbeSourceAutoProbe,
		nodestore.ProbeSourceAdminBatch,
	} {
		if !seen[src] {
			t.Errorf("来源 %q 未在记录中体现（来源必须忠实于事件出处），seen=%v", src, seen)
		}
	}
}

func getFaultRows(t *testing.T, adm *AdminHandler) []faultRowView {
	t.Helper()
	rec := httptest.NewRecorder()
	adm.adminGetNodeFaults(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/faults", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d", rec.Code)
	}
	var resp nodeFaultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Rows
}

// TestAdminNodeFaults_NoManagerSafe 未装配节点池时接口必须安全返回空结果
// （测试装配与"仅前置池"场景都可能没有 Exit）。
func TestAdminNodeFaults_NoManagerSafe(t *testing.T) {
	//nolint:exhaustruct // 故意不注入 Exit
	adm := &AdminHandler{handler: handler{deps: ServerDeps{}}}

	rec := httptest.NewRecorder()
	adm.adminGetNodeFaults(rec, httptest.NewRequest(http.MethodGet, "/api/admin/nodes/faults", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", rec.Code)
	}
	var resp nodeFaultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RetentionDays != db.FaultLogRetentionDays {
		t.Errorf("即使无节点池也应返回保留期口径, got %d", resp.RetentionDays)
	}

	rec = httptest.NewRecorder()
	adm.adminClearNodeFaults(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/nodes/faults", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d, want 200", rec.Code)
	}
}

// TestAdminDisableAudit_MasksSecrets 回归保护：管理端禁用原因必须脱敏。
//
// 冒烟测试曾抓到真实泄露：直接把手测失败原文拼进 disable_reason 后，完整 URL 的 query
// （含 key/token）被持久化并经 /api/admin/nodes 原样下发到面板。禁用原因属于"会自动展示
// 并可能被复制外传"的字段，必须与故障记录同口径脱敏。
func TestAdminDisableAudit_MasksSecrets(t *testing.T) {
	const secret = "AIzaSyFAKEKEY1234567890"
	errStr := `Post "https://cloudconsole-pa.clients6.google.com/v3/batchGraphql?key=` + secret +
		`&prettyPrint=false": unexpected HTTP response status: 403`

	class, reason := adminDisableAudit("手动测试失败", errStr)

	if class != string(nodestore.FailurePermission) {
		t.Errorf("class=%q, want %q", class, nodestore.FailurePermission)
	}
	if strings.Contains(reason, secret) {
		t.Errorf("禁用原因泄露凭据: %q", reason)
	}
	if !strings.Contains(reason, "<redacted>") {
		t.Errorf("凭据应被替换为 <redacted>: %q", reason)
	}
	// 定位信息必须保留，否则管理员看不懂"为什么禁"。
	for _, want := range []string{"手动测试失败", "403", "cloudconsole-pa.clients6.google.com", "batchGraphql"} {
		if !strings.Contains(reason, want) {
			t.Errorf("禁用原因应保留 %q: %q", want, reason)
		}
	}
}

// TestAdminDisableAudit_EmptyError 空错误文本不应产生带悬空分隔符的原因。
func TestAdminDisableAudit_EmptyError(t *testing.T) {
	class, reason := adminDisableAudit("协议不支持或解析失败", "")
	if reason != "协议不支持或解析失败" {
		t.Errorf("空错误文本时原因应为纯前缀, got %q", reason)
	}
	if class != string(nodestore.FailureUnknown) {
		t.Errorf("class=%q, want other", class)
	}
}
