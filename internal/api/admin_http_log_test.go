package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// newHTTPLogFixture 构造带内存 http_log 仓储的 AdminHandler 测试夹具。
func newHTTPLogFixture(t *testing.T) (*AdminHandler, *db.HTTPLogStore) {
	t.Helper()
	store, err := db.NewMemoryHTTPLogStore()
	if err != nil {
		t.Fatalf("NewMemoryHTTPLogStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adm := &AdminHandler{handler: handler{
		deps: ServerDeps{
			Cfg:     config.StaticProvider(config.DefaultConfig()),
			HTTPLog: store,
		},
	}}
	return adm, store
}

func TestAdminHTTPLogListAndDetail(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	recs := []db.HTTPLogRecord{
		{TS: base, RequestID: "ok-req", Scope: "batchgraphql", NodeURI: "socks5://n", NodeName: "node-a",
			PeerIP: "1.2.3.4", TCPReused: true, HTTPVersion: "HTTP/2", FinalURL: "https://x?key=SECRET",
			RequestBody: `{"prompt":"hi"}`, ResponseBody: `{"ok":true}`, StatusCode: 200, Success: true, ElapsedMs: 42},
		{TS: base - 1, RequestID: "fail-req", Scope: "batchgraphql", NodeURI: "socks5://n", NodeName: "node-a",
			StatusCode: 500, Success: false, ElapsedMs: 13},
	}
	if err := store.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// 列表（全部）
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/http-log?page=1&limit=20", nil)
	adm.adminGetHTTPLog(w, r)
	if w.Code != 200 {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("list total=%d items=%d, want 2/2", list.Total, len(list.Items))
	}
	// 时间倒序：ok-req 最新在前
	if list.Items[0]["request_id"] != "ok-req" {
		t.Fatalf("first item=%v, want ok-req", list.Items[0]["request_id"])
	}
	if list.Items[0]["tcp_reused"] != true || list.Items[0]["peer_ip"] != "1.2.3.4" {
		t.Fatalf("network metadata missing: %v", list.Items[0])
	}

	// 按成功筛选
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/admin/http-log?success=1", nil)
	adm.adminGetHTTPLog(w2, r2)
	var list2 struct {
		Total int64 `json:"total"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &list2)
	if list2.Total != 1 {
		t.Fatalf("success filter total=%d, want 1", list2.Total)
	}

	// 详情
	id := int64(list.Items[0]["id"].(float64))
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest("GET", "/api/admin/http-log/detail?id="+strconv.FormatInt(id, 10), nil)
	adm.adminGetHTTPLogDetail(w3, r3)
	if w3.Code != 200 {
		t.Fatalf("detail status=%d body=%s", w3.Code, w3.Body.String())
	}
	var detail struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if detail.Item["request_body"] != `{"prompt":"hi"}` || detail.Item["response_body"] != `{"ok":true}` {
		t.Fatalf("detail bodies mismatch: %v", detail.Item)
	}
	if detail.Item["final_url"] != "https://x?key=SECRET" {
		t.Fatalf("final_url not preserved: %v", detail.Item["final_url"])
	}
}

func TestAdminHTTPLogUnavailableStore(t *testing.T) {
	adm := &AdminHandler{handler: handler{deps: ServerDeps{}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/http-log", nil)
	adm.adminGetHTTPLog(w, r)
	if w.Code != 500 {
		t.Fatalf("unavailable store status=%d, want 500", w.Code)
	}
}

func TestAdminHTTPLogReset(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	if err := store.InsertBatch([]db.HTTPLogRecord{
		{TS: base, RequestID: "r1", Scope: "batchgraphql", Success: true, StatusCode: 200},
		{TS: base - 1, RequestID: "r2", Scope: "recaptcha", Success: false, StatusCode: 500},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/http-log/reset", nil)
	adm.adminResetHTTPLog(w, r)
	if w.Code != 200 {
		t.Fatalf("reset status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK      bool  `json:"ok"`
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal reset: %v", err)
	}
	if !resp.OK || resp.Deleted != 2 {
		t.Fatalf("reset response mismatch: %+v", resp)
	}

	pg, err := store.History(1, 20, db.HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History after reset: %v", err)
	}
	if pg.Total != 0 {
		t.Fatalf("after reset total=%d, want 0", pg.Total)
	}
}

func TestAdminHTTPLogExportJSON(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	_ = store.InsertBatch([]db.HTTPLogRecord{
		{TS: base, RequestID: "e1", Scope: "batchgraphql", Success: true, StatusCode: 200, Error: ""},
		{TS: base - 1, RequestID: "e2", Scope: "batchgraphql", Success: false, StatusCode: 500, Error: "boom"},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/http-log/export?format=json", nil)
	adm.adminExportHTTPLog(w, r)
	if w.Code != 200 {
		t.Fatalf("export status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("导出应设置 Content-Disposition attachment")
	}
	var payload struct {
		Count int              `json:"count"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	if payload.Count != 2 || len(payload.Items) != 2 {
		t.Fatalf("export count=%d items=%d, want 2/2", payload.Count, len(payload.Items))
	}
	// 时间升序：e2 在前
	if payload.Items[0]["request_id"] != "e2" || payload.Items[0]["error"] != "boom" {
		t.Fatalf("export order/error mismatch: %+v", payload.Items[0])
	}
}

func TestAdminHTTPLogExportCSV(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	_ = store.InsertBatch([]db.HTTPLogRecord{
		{TS: base, RequestID: "c1", Scope: "batchgraphql", Success: true, StatusCode: 200},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/http-log/export?format=csv", nil)
	adm.adminExportHTTPLog(w, r)
	if w.Code != 200 {
		t.Fatalf("export csv status=%d body=%s", w.Code, w.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 { // 表头 + 1 行
		t.Fatalf("csv lines=%d, want 2: %s", len(lines), w.Body.String())
	}
	if !strings.Contains(lines[0], "request_id") || !strings.Contains(lines[1], "c1") {
		t.Fatalf("csv content mismatch: %s", w.Body.String())
	}
}

func TestAdminHTTPLogImport(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	// 预置一条，验证导入幂等跳过
	_ = store.InsertBatch([]db.HTTPLogRecord{{TS: base, RequestID: "dup-1", Scope: "batchgraphql", Success: true, StatusCode: 200}})

	body := `{"items":[
		{"request_id":"dup-1","scope":"batchgraphql","ts":` + strconv.FormatInt(base, 10) + `,"success":true,"status_code":200},
		{"request_id":"new-1","scope":"recaptcha","ts":` + strconv.FormatInt(base, 10) + `,"success":false,"status_code":500,"error":"timeout"}
	]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/http-log/import", strings.NewReader(body))
	adm.adminImportHTTPLog(w, r)
	if w.Code != 200 {
		t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK       bool `json:"ok"`
		Inserted int  `json:"inserted"`
		Skipped  int  `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal import: %v", err)
	}
	if !resp.OK || resp.Inserted != 1 || resp.Skipped != 1 {
		t.Fatalf("import result mismatch: %+v", resp)
	}

	pg, err := store.History(1, 20, db.HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if pg.Total != 2 {
		t.Fatalf("after import total=%d, want 2", pg.Total)
	}
}

func TestAdminHTTPLogImportInvalidJSON(t *testing.T) {
	adm, _ := newHTTPLogFixture(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/http-log/import", strings.NewReader(`{bad json`))
	adm.adminImportHTTPLog(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid import status=%d, want 400", w.Code)
	}
}

func TestAdminHTTPLogNewFieldsAndFilters(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	recs := []db.HTTPLogRecord{
		{TS: base, RequestID: "nf-1", Scope: "batchgraphql", NodeName: "node-a", PeerIP: "1.2.3.4",
			StatusCode: 200, Success: true, ResponseCode: 0, ResponseResult: "STOP"},
		{TS: base - 1, RequestID: "nf-2", Scope: "batchgraphql", NodeName: "node-b", PeerIP: "5.6.7.8",
			StatusCode: 200, Success: false, ResponseCode: 8,
			ResponseResult: "DATA_FETCHING_EXCEPTION", ResponseErrorType: "DATA_FETCHING_EXCEPTION",
			ResponseErrorMessage: "Resource has been exhausted (e.g. check quota)."},
	}
	if err := store.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// 列表应输出新字段。
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/http-log?page=1&limit=20", nil)
	adm.adminGetHTTPLog(w, r)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items=%d, want 2", len(list.Items))
	}
	first := list.Items[0] // 时间倒序：nf-1 在前
	if _, ok := first["response_error_type"]; !ok {
		t.Fatalf("response_error_type missing in output: %v", first)
	}
	if first["response_result"] != "STOP" {
		t.Fatalf("response_result=%v, want STOP", first["response_result"])
	}

	// 按节点筛选（node-b → nf-2）
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/admin/http-log?node=node-b", nil)
	adm.adminGetHTTPLog(w2, r2)
	var l2 struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &l2)
	if l2.Total != 1 || l2.Items[0]["request_id"] != "nf-2" {
		t.Fatalf("node filter total=%d, want 1 nf-2", l2.Total)
	}

	// 按对端 IP 筛选
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest("GET", "/api/admin/http-log?peer_ip=5.6.7", nil)
	adm.adminGetHTTPLog(w3, r3)
	var l3 struct {
		Total int64 `json:"total"`
	}
	_ = json.Unmarshal(w3.Body.Bytes(), &l3)
	if l3.Total != 1 {
		t.Fatalf("peer_ip filter total=%d, want 1", l3.Total)
	}

	// 按业务响应码筛选
	w4 := httptest.NewRecorder()
	r4 := httptest.NewRequest("GET", "/api/admin/http-log?response_code=8", nil)
	adm.adminGetHTTPLog(w4, r4)
	var l4 struct {
		Total int64 `json:"total"`
	}
	_ = json.Unmarshal(w4.Body.Bytes(), &l4)
	if l4.Total != 1 {
		t.Fatalf("response_code filter total=%d, want 1", l4.Total)
	}

	// 按失败类型筛选
	w5 := httptest.NewRecorder()
	r5 := httptest.NewRequest("GET", "/api/admin/http-log?error_type=DATA_FETCH", nil)
	adm.adminGetHTTPLog(w5, r5)
	var l5 struct {
		Total int64 `json:"total"`
	}
	_ = json.Unmarshal(w5.Body.Bytes(), &l5)
	if l5.Total != 1 {
		t.Fatalf("error_type filter total=%d, want 1", l5.Total)
	}

	// 按时间范围筛选（from=base 命中 nf-1，nf-2 在 base-1 被排除）
	from := strconv.FormatInt(base, 10)
	to := strconv.FormatInt(base+5, 10)
	w6 := httptest.NewRecorder()
	r6 := httptest.NewRequest("GET", "/api/admin/http-log?from="+from+"&to="+to, nil)
	adm.adminGetHTTPLog(w6, r6)
	var l6 struct {
		Total int64 `json:"total"`
	}
	_ = json.Unmarshal(w6.Body.Bytes(), &l6)
	if l6.Total != 1 {
		t.Fatalf("time filter total=%d, want 1", l6.Total)
	}
}

func TestAdminHTTPLogExportIncludesErrorFields(t *testing.T) {
	adm, store := newHTTPLogFixture(t)
	base := time.Now().Unix()
	_ = store.InsertBatch([]db.HTTPLogRecord{{
		TS: base, RequestID: "exp-err", Scope: "batchgraphql",
		StatusCode: 200, Success: false, ResponseCode: 8,
		ResponseErrorType:    "DATA_FETCHING_EXCEPTION",
		ResponseErrorMessage: "quota exhausted",
	}})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/http-log/export?format=csv", nil)
	adm.adminExportHTTPLog(w, r)
	if w.Code != 200 {
		t.Fatalf("export status=%d body=%s", w.Code, w.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("csv lines=%d, want 2", len(lines))
	}
	if !strings.Contains(lines[0], "response_error_type") || !strings.Contains(lines[0], "response_error_message") {
		t.Fatalf("csv header missing error fields: %s", lines[0])
	}
	if !strings.Contains(lines[1], "DATA_FETCHING_EXCEPTION") || !strings.Contains(lines[1], "quota exhausted") {
		t.Fatalf("csv row missing error values: %s", lines[1])
	}
}
