package db

import (
	"fmt"
	"testing"
	"time"
)

func memHTTPLogStore(t *testing.T) *HTTPLogStore {
	t.Helper()
	s, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewHTTPLogStore(s.db)
}

func TestHTTPLogInsertBatchAndHistory(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	recs := make([]HTTPLogRecord, 0, 25)
	for i := 0; i < 25; i++ {
		recs = append(recs, HTTPLogRecord{
			TS: base - int64(25-i), RequestID: fmt.Sprintf("req-%d", i),
			Scope: "batchgraphql", FinalURL: "https://example.com/" + fmt.Sprint(i),
			StatusCode: 200, Success: i%2 == 0,
		})
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page1, err := s.History(1, 10, HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History page1: %v", err)
	}
	if page1.Total != 25 || len(page1.Items) != 10 {
		t.Fatalf("page1 total=%d items=%d, want 25/10", page1.Total, len(page1.Items))
	}
	if page1.Items[0].RequestID != "req-24" {
		t.Fatalf("page1[0] = %q, want req-24 (最新)", page1.Items[0].RequestID)
	}
	if !page1.Items[0].Success {
		t.Fatalf("page1[0] should be success (even index)")
	}

	page3, err := s.History(3, 10, HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History page3: %v", err)
	}
	if len(page3.Items) != 5 {
		t.Fatalf("page3 items=%d, want 5", len(page3.Items))
	}
}

func TestHTTPLogFilter(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	recs := []HTTPLogRecord{
		{TS: base, RequestID: "ok-1", Scope: "batchgraphql", Success: true, StatusCode: 200},
		{TS: base - 1, RequestID: "fail-1", Scope: "batchgraphql", Success: false, StatusCode: 500},
		{TS: base - 2, RequestID: "ok-2", Scope: "recaptcha", Success: true, StatusCode: 200},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// 按 request_id
	pg, err := s.History(1, 20, HTTPLogFilter{RequestID: "fail-1"})
	if err != nil {
		t.Fatalf("History by req: %v", err)
	}
	if pg.Total != 1 || len(pg.Items) != 1 || pg.Items[0].RequestID != "fail-1" {
		t.Fatalf("filter by request_id failed: %+v", pg)
	}

	// 只看成功
	b := true
	pg, err = s.History(1, 20, HTTPLogFilter{Success: &b})
	if err != nil {
		t.Fatalf("History success: %v", err)
	}
	if pg.Total != 2 {
		t.Fatalf("success filter total=%d, want 2", pg.Total)
	}

	// 只看失败
	f := false
	pg, err = s.History(1, 20, HTTPLogFilter{Success: &f})
	if err != nil {
		t.Fatalf("History fail: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "fail-1" {
		t.Fatalf("fail filter total=%d, want 1", pg.Total)
	}

	// 按 scope
	pg, err = s.History(1, 20, HTTPLogFilter{Scope: "recaptcha"})
	if err != nil {
		t.Fatalf("History scope: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "ok-2" {
		t.Fatalf("scope filter total=%d, want 1", pg.Total)
	}
}

func TestHTTPLogByIDAndReset(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	rec := HTTPLogRecord{TS: base, RequestID: "single", Scope: "batchgraphql", FinalURL: "u", RequestBody: "body", Success: true}
	if err := s.InsertBatch([]HTTPLogRecord{rec}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	pg, err := s.History(1, 20, HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	id := pg.Items[0].ID

	byID, found, err := s.ByID(id)
	if err != nil || !found {
		t.Fatalf("ByID(%d) found=%v err=%v", id, found, err)
	}
	if byID.RequestBody != "body" || !byID.Success {
		t.Fatalf("ByID mismatch: %+v", byID)
	}

	// 不存在
	if _, found, err := s.ByID(99999); err != nil || found {
		t.Fatalf("ByID(99999) found=%v err=%v, want not found", found, err)
	}

	// 保留清理
	n, err := s.ResetOlderThan(base - 1000)
	if err != nil {
		t.Fatalf("ResetOlderThan: %v", err)
	}
	if n != 0 {
		t.Fatalf("ResetOlderThan should delete 0, got %d", n)
	}
	// 再清理到 future 应删除 1 条
	n, err = s.ResetOlderThan(base + 1000)
	if err != nil {
		t.Fatalf("ResetOlderThan future: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetOlderThan future should delete 1, got %d", n)
	}
}

func TestHTTPLogErrorField(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	rec := HTTPLogRecord{
		TS: base, RequestID: "err-1", Scope: "batchgraphql", Success: false,
		StatusCode: 0, Error: "dial tcp: connection refused", FinalURL: "https://x",
	}
	if err := s.InsertBatch([]HTTPLogRecord{rec}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	pg, err := s.History(1, 20, HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	if pg.Items[0].Error != "dial tcp: connection refused" {
		t.Fatalf("Error=%q, want connection refused", pg.Items[0].Error)
	}
	if pg.Items[0].Success || pg.Items[0].StatusCode != 0 {
		t.Fatalf("fail record mismatch: %+v", pg.Items[0])
	}
}

func TestHTTPLogExportAll(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	recs := []HTTPLogRecord{
		{TS: base - 10, RequestID: "old", Scope: "batchgraphql", Success: true},
		{TS: base, RequestID: "new", Scope: "recaptcha", Success: false},
		{TS: base + 1, RequestID: "newest", Scope: "batchgraphql", Success: true},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// 全量导出（from=0）
	all, err := s.ExportAll(0, HTTPLogFilter{}, 100)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("export all=%d, want 3", len(all))
	}
	// 时间升序
	if all[0].RequestID != "old" || all[2].RequestID != "newest" {
		t.Fatalf("export order mismatch: %+v", all)
	}

	// 按时间窗口 + 成功过滤
	f := true
	filtered, err := s.ExportAll(base-5, HTTPLogFilter{Success: &f}, 100)
	if err != nil {
		t.Fatalf("ExportAll filtered: %v", err)
	}
	// from=base-5 排除 old(ts=base-10)，成功过滤排除 new(fail)，仅剩 newest。
	if len(filtered) != 1 || filtered[0].RequestID != "newest" {
		t.Fatalf("filtered=%+v, want 1 (newest)", filtered)
	}
	for _, r := range filtered {
		if r.TS < base-5 || !r.Success {
			t.Fatalf("filter leak: %+v", r)
		}
	}

	// limit 生效
	limited, err := s.ExportAll(0, HTTPLogFilter{}, 2)
	if err != nil {
		t.Fatalf("ExportAll limited: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited=%d, want 2", len(limited))
	}
}

func TestHTTPLogExistingRequestIDs(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	_ = s.InsertBatch([]HTTPLogRecord{
		{TS: base, RequestID: "r1", Success: true},
		{TS: base, RequestID: "r2", Success: true},
		{TS: base, RequestID: "", Success: true},
	})
	ids, err := s.ExistingRequestIDs()
	if err != nil {
		t.Fatalf("ExistingRequestIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("existing ids=%d, want 2 (empty excluded)", len(ids))
	}
	if _, ok := ids["r1"]; !ok {
		t.Fatal("r1 missing")
	}
	if _, ok := ids["r2"]; !ok {
		t.Fatal("r2 missing")
	}
}

func TestHTTPLogResponseErrorFields(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	rec := HTTPLogRecord{
		TS: base, RequestID: "err-2", Scope: "batchgraphql",
		StatusCode: 200, Success: false,
		ResponseCode:         8,
		ResponseResult:       "DATA_FETCHING_EXCEPTION",
		ResponseErrorType:    "DATA_FETCHING_EXCEPTION",
		ResponseErrorMessage: "Resource has been exhausted (e.g. check quota).",
	}
	if err := s.InsertBatch([]HTTPLogRecord{rec}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// History 往返
	pg, err := s.History(1, 20, HTTPLogFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	it := pg.Items[0]
	if it.ResponseCode != 8 || it.ResponseErrorType != "DATA_FETCHING_EXCEPTION" ||
		it.ResponseErrorMessage != "Resource has been exhausted (e.g. check quota)." {
		t.Fatalf("History fields mismatch: %+v", it)
	}

	// ByID 往返
	byID, found, err := s.ByID(it.ID)
	if err != nil || !found {
		t.Fatalf("ByID: %v %v", found, err)
	}
	if byID.ResponseCode != 8 || byID.ResponseErrorType != "DATA_FETCHING_EXCEPTION" {
		t.Fatalf("ByID fields mismatch: %+v", byID)
	}

	// ExportAll 往返
	exp, err := s.ExportAll(0, HTTPLogFilter{}, 10)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(exp) != 1 || exp[0].ResponseErrorMessage != "Resource has been exhausted (e.g. check quota)." {
		t.Fatalf("ExportAll fields mismatch: %+v", exp)
	}
}

func TestHTTPLogFilterExtended(t *testing.T) {
	s := memHTTPLogStore(t)
	base := time.Now().Unix()
	recs := []HTTPLogRecord{
		{
			TS: base, RequestID: "f-1", Scope: "batchgraphql",
			NodeURI: "socks5://n1", NodeName: "node-a", PeerIP: "1.2.3.4",
			StatusCode: 200, Success: true, ResponseCode: 0, ResponseResult: "STOP",
		},
		{
			TS: base - 100, RequestID: "f-2", Scope: "batchgraphql",
			NodeURI: "socks5://n2", NodeName: "node-b", PeerIP: "5.6.7.8",
			StatusCode: 500, Success: false, ResponseCode: 8,
			ResponseErrorType: "DATA_FETCHING_EXCEPTION",
		},
		{
			TS: base - 200, RequestID: "f-3", Scope: "recaptcha",
			NodeURI: "socks5://n1", NodeName: "node-a", PeerIP: "9.9.9.9",
			StatusCode: 200, Success: true,
		},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	// 按节点名（node-a 命 f-1/f-3）
	pg, err := s.History(1, 20, HTTPLogFilter{NodeName: "node-a"})
	if err != nil {
		t.Fatalf("History by node: %v", err)
	}
	if pg.Total != 2 {
		t.Fatalf("node filter total=%d, want 2", pg.Total)
	}

	// 按对端 IP
	pg, err = s.History(1, 20, HTTPLogFilter{PeerIP: "5.6.7"})
	if err != nil {
		t.Fatalf("History by peer: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "f-2" {
		t.Fatalf("peer filter total=%d, want 1 f-2", pg.Total)
	}

	// 按 HTTP 状态码
	pg, err = s.History(1, 20, HTTPLogFilter{HTTPStatus: 500})
	if err != nil {
		t.Fatalf("History by http status: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "f-2" {
		t.Fatalf("http status filter total=%d, want 1 f-2", pg.Total)
	}

	// 按业务响应码
	pg, err = s.History(1, 20, HTTPLogFilter{ResponseCode: 8})
	if err != nil {
		t.Fatalf("History by resp code: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "f-2" {
		t.Fatalf("resp code filter total=%d, want 1 f-2", pg.Total)
	}

	// 按失败类型模糊
	pg, err = s.History(1, 20, HTTPLogFilter{ErrorType: "DATA_FETCH"})
	if err != nil {
		t.Fatalf("History by error type: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "f-2" {
		t.Fatalf("error type filter total=%d, want 1 f-2", pg.Total)
	}

	// 时间范围 [base-150, base-50) 命中 f-2
	pg, err = s.History(1, 20, HTTPLogFilter{FromTS: base - 150, ToTS: base - 50})
	if err != nil {
		t.Fatalf("History by time: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "f-2" {
		t.Fatalf("time filter total=%d, want 1 f-2", pg.Total)
	}

	// 组合：node-a + 成功 → f-1（recaptcha 的 f-3 也是 node-a+成功，注意 scope 不同）
	f := true
	pg, err = s.History(1, 20, HTTPLogFilter{NodeName: "node-a", Success: &f, Scope: "batchgraphql"})
	if err != nil {
		t.Fatalf("History combined: %v", err)
	}
	if pg.Total != 1 || pg.Items[0].RequestID != "f-1" {
		t.Fatalf("combined filter total=%d, want 1 f-1: %+v", pg.Total, pg.Items)
	}
}
