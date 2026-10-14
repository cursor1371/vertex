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

// newPolicyFixture 组装"真实 DB + 策略解析器 + 管理器"的测试夹具。
func newPolicyFixture(t *testing.T) (*AdminHandler, *exitpool.Manager, *nodestore.PolicySet) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "policies.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mgr := exitpool.NewManager(database, nil, exitpool.Hooks{}, func() int { return 2 })
	set := nodestore.NewPolicySet(func() int { return 2 })
	mgr.SetPolicySet(set)
	//nolint:exhaustruct // 仅注入被测路径消费的依赖
	adm := &AdminHandler{handler: handler{deps: ServerDeps{Exit: mgr}}}
	return adm, mgr, set
}

func getPolicies(t *testing.T, adm *AdminHandler) faultPoliciesResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	adm.adminGetFaultPolicies(rec, httptest.NewRequest(http.MethodGet, "/api/admin/fault-policies", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", rec.Code)
	}
	var resp faultPoliciesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func putPolicies(t *testing.T, adm *AdminHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/fault-policies", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	adm.adminPutFaultPolicies(rec, req)
	return rec
}

// TestFaultPolicies_ListShowsDefaultsAndSemantics 列表必须给出：全部已知类别、
// 生效动作与标签、默认动作、阈值与"是否跟随全局"、来源（默认/自定义）、以及语义提示。
// 面板据此渲染，缺任何一项都会让用户看不清"当前到底怎么处理"。
func TestFaultPolicies_ListShowsDefaultsAndSemantics(t *testing.T) {
	adm, _, _ := newPolicyFixture(t)
	resp := getPolicies(t, adm)

	if len(resp.Policies) != len(nodestore.KnownFailureClasses()) {
		t.Fatalf("应列出全部内置类别, got %d want %d", len(resp.Policies), len(nodestore.KnownFailureClasses()))
	}
	byClass := map[string]policyView{}
	for _, p := range resp.Policies {
		byClass[p.Class] = p
	}
	// 默认动作：403/网络类/auth 为"禁用后恢复探测"，DNS 为"不禁用"。
	for _, c := range []string{"node_network", "permission", "auth"} {
		p := byClass[c]
		if p.Action != string(nodestore.PolicyDisableThenProbe) {
			t.Errorf("%s 默认动作=%q, want disable_then_probe", c, p.Action)
		}
		if p.Custom {
			t.Errorf("%s 初始不应是自定义", c)
		}
		if p.Note == "" {
			t.Errorf("%s 必须带语义提示（面板要解释三种动作的区别）", c)
		}
	}
	if p := byClass["dns"]; p.Action != string(nodestore.PolicyRecordOnly) {
		t.Errorf("dns 默认动作=%q, want record_only", p.Action)
	}
	if resp.GlobalProbeFailThreshold != 2 {
		t.Errorf("全局默认阈值=%d, want 2", resp.GlobalProbeFailThreshold)
	}
	if resp.EntryPoolProbes {
		t.Error("前置池第一版不执行自动探测，entry_pool_probes 应为 false（UI 据此提示）")
	}
}

// TestFaultPolicies_PutPersistsAndTakesEffect PUT 落库 + 即时生效（无需重启）+
// 解析器与管理器口径一致。
func TestFaultPolicies_PutPersistsAndTakesEffect(t *testing.T) {
	adm, mgr, set := newPolicyFixture(t)

	rec := putPolicies(t, adm, `{"policies":[{"class":"node_network","action":"single_shot","probe_fail_threshold":4}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 解析器即时生效。注意：单次禁用不参与探测，阈值对它是无意义的，
	// 故解析结果统一上报为"跟随全局"——真正要断言的是动作已切换。
	pol := set.For(nodestore.FailureNodeNetwork)
	if pol.Action != nodestore.PolicySingleShot {
		t.Fatalf("PUT 后动作未生效: %+v", pol)
	}
	// 用户填的阈值仍原样保留在库行里（切回探测动作时立刻可用，不必重填）。
	stored, err := mgr.PolicyRows()
	if err != nil {
		t.Fatalf("PolicyRows: %v", err)
	}
	if len(stored) != 1 || stored[0].ProbeFailThreshold != 4 {
		t.Fatalf("阈值应原样落库: %+v", stored)
	}
	// 池内判定同步（同一实例）
	mgr.MergeNodes([]exitpool.Node{{RawURI: "u1", Name: "n1"}}) //nolint:exhaustruct
	if !mgr.ReportProbe("u1", exitpool.ProbeResult{OK: false, ErrStr: "dial tcp: connection refused"}) {
		t.Fatal("单次禁用策略下首次失败应摘出")
	}
	if src := mgr.LoadNodes()[0].DisableSource; src != nodestore.DisableSourceSingleShot {
		t.Errorf("来源=%q, want single_shot", src)
	}

	// 落库：用库里的行重建一个新解析器（模拟重启）后仍是自定义值
	again := nodestore.NewPolicySet(func() int { return 2 })
	again.Replace(nodestore.PolicyRowsToSet(stored))
	if got := again.For(nodestore.FailureNodeNetwork); got.Action != nodestore.PolicySingleShot {
		t.Errorf("重启后策略未保留: %+v", got)
	}

	// GET 应显示为自定义
	for _, p := range getPolicies(t, adm).Policies {
		if p.Class == "node_network" && !p.Custom {
			t.Errorf("GET 应把该类别标记为自定义: %+v", p)
		}
	}
}

// TestFaultPolicies_PutValidation 非法输入必须拒绝且不落库（不要静默按可疑值执行）。
func TestFaultPolicies_PutValidation(t *testing.T) {
	adm, mgr, _ := newPolicyFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"未知类别", `{"policies":[{"class":"not_a_class","action":"record_only"}]}`},
		{"非法动作", `{"policies":[{"class":"node_network","action":"disable_everything"}]}`},
		{"阈值为负", `{"policies":[{"class":"node_network","action":"record_only","probe_fail_threshold":-1}]}`},
		{"阈值超上限", `{"policies":[{"class":"node_network","action":"record_only","probe_fail_threshold":11}]}`},
		{"空列表", `{"policies":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := putPolicies(t, adm, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			rows, err := mgr.PolicyRows()
			if err != nil {
				t.Fatalf("PolicyRows: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("非法输入不得落库, got %+v", rows)
			}
		})
	}
}

// TestFaultPolicies_PutAcceptsZeroAndOmittedThreshold 阈值的两种边界写法：
// 显式 0 = 无限探测；省略 = 跟随全局。
func TestFaultPolicies_PutAcceptsZeroAndOmittedThreshold(t *testing.T) {
	adm, _, set := newPolicyFixture(t)

	if rec := putPolicies(t, adm, `{"policies":[{"class":"node_network","action":"disable_then_probe","probe_fail_threshold":0}]}`); rec.Code != http.StatusOK {
		t.Fatalf("显式 0 应被接受, status=%d", rec.Code)
	}
	pol := set.For(nodestore.FailureNodeNetwork)
	if pol.ProbeFailThreshold != nodestore.ProbeThresholdUnlimited || pol.ThresholdFollowsGlobal {
		t.Errorf("显式 0 应解析为无限探测且非跟随全局: %+v", pol)
	}

	// 省略阈值 → 跟随全局（全局 2）
	if rec := putPolicies(t, adm, `{"policies":[{"class":"permission","action":"disable_then_probe"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("省略阈值应被接受, status=%d", rec.Code)
	}
	if got := set.For(nodestore.FailurePermission); !got.ThresholdFollowsGlobal || got.ProbeFailThreshold != 2 {
		t.Errorf("省略阈值应跟随全局 2: %+v", got)
	}
}

// TestFaultPolicies_DeleteResetsToDefault DELETE = 重置全部自定义，回落内置默认。
func TestFaultPolicies_DeleteResetsToDefault(t *testing.T) {
	adm, _, set := newPolicyFixture(t)

	if rec := putPolicies(t, adm, `{"policies":[{"class":"dns","action":"disable_then_probe","probe_fail_threshold":3}]}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d", rec.Code)
	}
	if got := set.For(nodestore.FailureDNS); got.Action != nodestore.PolicyDisableThenProbe {
		t.Fatalf("前置条件：dns 应已被改成禁用, got %q", got.Action)
	}

	rec := httptest.NewRecorder()
	adm.adminDeleteFaultPolicies(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/fault-policies", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d", rec.Code)
	}
	var del map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &del); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if del["reset"].(float64) != 1 {
		t.Errorf("reset=%v, want 1", del["reset"])
	}
	if got := set.For(nodestore.FailureDNS); got.Action != nodestore.PolicyRecordOnly {
		t.Errorf("重置后应回落内置默认（dns 不禁用）, got %q", got.Action)
	}
	// 重置后 GET 不再标记自定义
	for _, p := range getPolicies(t, adm).Policies {
		if p.Class == "dns" && p.Custom {
			t.Error("重置后不应再标记为自定义")
		}
	}
}

// TestFaultPolicies_DeleteSingleClassRemovesRow 单类别"还原默认"必须是真正的**删行**：
// 数据库里不再有该行、GET 显示 Custom=false、生效策略等于内置默认。
// 这条用例直接守住"稀疏覆盖"设计——曾经的做法是把默认动作写回成自定义行，
// 结果库里残留记录、刷新后仍显示"自定义"。
func TestFaultPolicies_DeleteSingleClassRemovesRow(t *testing.T) {
	adm, mgr, set := newPolicyFixture(t)

	// 先自定义两个类别，确认删一个不会误伤另一个。
	if rec := putPolicies(t, adm, `{"policies":[{"class":"node_network","action":"record_only"},{"class":"dns","action":"disable_then_probe","probe_fail_threshold":5}]}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d", rec.Code)
	}
	rows, err := mgr.PolicyRows()
	if err != nil {
		t.Fatalf("PolicyRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("前置条件：应有 2 条自定义行, got %d", len(rows))
	}

	// 还原 node_network
	rec := httptest.NewRecorder()
	adm.adminDeleteFaultPolicies(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/fault-policies?class=node_network", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE 单类别 status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1) 库里那一行真的没了，另一条仍在
	rows, err = mgr.PolicyRows()
	if err != nil {
		t.Fatalf("PolicyRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Class != nodestore.FailureDNS {
		t.Fatalf("应只剩 dns 的自定义行, got %+v", rows)
	}

	// 2) 生效策略回到内置默认
	pol := set.For(nodestore.FailureNodeNetwork)
	if pol.Action != nodestore.BuiltinPolicyAction(nodestore.FailureNodeNetwork) {
		t.Errorf("生效动作应回落内置默认 %q, got %q", nodestore.BuiltinPolicyAction(nodestore.FailureNodeNetwork), pol.Action)
	}
	if !pol.ActionIsDefault || !pol.ThresholdFollowsGlobal {
		t.Errorf("应标记为默认动作且阈值跟随全局: %+v", pol)
	}

	// 3) GET 显示 Custom=false（页面刷新后不会再显示"自定义"）
	for _, p := range getPolicies(t, adm).Policies {
		if p.Class == "node_network" && p.Custom {
			t.Error("还原后该类别不应再标记为自定义")
		}
		if p.Class == "dns" && !p.Custom {
			t.Error("未被还原的类别应仍标记为自定义")
		}
	}

	// 4) 幂等：再删一次不报错
	rec = httptest.NewRecorder()
	adm.adminDeleteFaultPolicies(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/fault-policies?class=node_network", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("重复还原应成功（幂等）, status=%d", rec.Code)
	}

	// 5) 未知类别应拒绝
	rec = httptest.NewRecorder()
	adm.adminDeleteFaultPolicies(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/fault-policies?class=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知类别应 400, got %d", rec.Code)
	}
}

// TestFaultPolicies_NoManagerSafe 未装配节点池时接口安全返回（测试装配场景）。
func TestFaultPolicies_NoManagerSafe(t *testing.T) {
	//nolint:exhaustruct // 故意不注入 Exit
	adm := &AdminHandler{handler: handler{deps: ServerDeps{}}}

	rec := httptest.NewRecorder()
	adm.adminGetFaultPolicies(rec, httptest.NewRequest(http.MethodGet, "/api/admin/fault-policies", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", rec.Code)
	}
	var resp faultPoliciesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Policies) != 0 {
		t.Errorf("无节点池时不应列出策略, got %d", len(resp.Policies))
	}
	rec = httptest.NewRecorder()
	adm.adminDeleteFaultPolicies(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/fault-policies", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无节点池时 DELETE 应返回 503, got %d", rec.Code)
	}
}
