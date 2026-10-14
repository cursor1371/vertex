package db

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func memStore(t *testing.T) *UsageStore {
	t.Helper()
	s, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = s.db.Close() })
	return s
}

func TestUsageStoreInsertAndAggregate(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()

	if err := s.Insert(UsageRecord{
		TS: base, Model: "m1", APIKeyName: "alice", NodeName: "n1",
		Success: true, StatusCode: 200, PromptTokens: 10, CandidatesTokens: 20, TotalTokens: 30,
		TotalMs: 500, TTFBMs: 200, OutputMs: 300,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.Insert(UsageRecord{
		TS: base - 100, Model: "m2", APIKeyName: "bob", NodeName: "n2",
		Success: false, StatusCode: 500, PromptTokens: 5, CandidatesTokens: 0, TotalTokens: 5,
		TotalMs: 100, TTFBMs: 100, OutputMs: 0,
	}); err != nil {
		t.Fatalf("Insert 2: %v", err)
	}

	sum, err := s.Aggregate(base-3600, base+3600)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if sum.Requests != 2 || sum.Success != 1 || sum.Errors != 1 {
		t.Fatalf("unexpected summary: %+v", sum)
	}
	if sum.PromptTokens != 15 || sum.CandidatesTokens != 20 || sum.TotalTokens != 35 {
		t.Fatalf("unexpected token sums: %+v", sum)
	}

	// 时间窗口过滤：只应命中 base（不含 base-100）
	sumDay, err := s.Aggregate(base-60, base+60)
	if err != nil {
		t.Fatalf("Aggregate day: %v", err)
	}
	if sumDay.Requests != 1 || sumDay.Success != 1 {
		t.Fatalf("expected only the recent record, got %+v", sumDay)
	}
}

func TestUsageStoreGrouped(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()
	_ = s.InsertBatch([]UsageRecord{
		{TS: base, Model: "a", APIKeyName: "k1", Success: true},
		{TS: base, Model: "b", APIKeyName: "k1", Success: false},
		{TS: base, Model: "a", APIKeyName: "k2", Success: true},
	})

	byModel, err := s.ByModel(base-1, base+1)
	if err != nil {
		t.Fatalf("ByModel: %v", err)
	}
	if len(byModel) != 2 || byModel[0].Name != "a" || byModel[0].Requests != 2 {
		t.Fatalf("unexpected by_model: %+v", byModel)
	}

	byKey, err := s.ByKey(base-1, base+1)
	if err != nil {
		t.Fatalf("ByKey: %v", err)
	}
	if len(byKey) != 2 {
		t.Fatalf("unexpected by_key: %+v", byKey)
	}
}

// TestUsageStoreByNode_RawURIIsolation 验证 by_node 仍按 node_name 分组：
// UsageRow.RawURI 是 API 层增强字段，不参与 DB 聚合查询（保证 grouped() 行为不变）。
func TestUsageStoreByNode_RawURIIsolation(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()
	_ = s.InsertBatch([]UsageRecord{
		{TS: base, NodeName: "node1", Success: true},
		{TS: base, NodeName: "node1", Success: false},
		{TS: base, NodeName: "node2", Success: true},
	})

	byNode, err := s.ByNode(base-1, base+1)
	if err != nil {
		t.Fatalf("ByNode: %v", err)
	}
	if len(byNode) != 2 {
		t.Fatalf("expected 2 node groups, got %d: %+v", len(byNode), byNode)
	}
	// 仍按 name 聚合，RawURI 保持空串（由 API 层增强）。
	for _, row := range byNode {
		if row.RawURI != "" {
			t.Fatalf("RawURI should remain empty from DB query, got %q for %q", row.RawURI, row.Name)
		}
	}
	// 请求数降序：node1(2) 在 node2(1) 之前。
	if byNode[0].Name != "node1" || byNode[0].Requests != 2 {
		t.Fatalf("unexpected by_node ordering: %+v", byNode)
	}
}

func TestUsageStoreDailySeries(t *testing.T) {
	s := memStore(t)
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	_ = s.InsertBatch([]UsageRecord{
		{TS: todayStart + 100, Success: true, TotalTokens: 10},
		{TS: todayStart + 200, Success: false, TotalTokens: 5},
		{TS: todayStart - 86400 + 100, Success: true, TotalTokens: 7},
	})

	series, err := s.DailySeries(todayStart-2*86400, now.Unix()+1, 86400)
	if err != nil {
		t.Fatalf("DailySeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 daily buckets, got %d: %+v", len(series), series)
	}
	if series[1].Requests != 2 || series[1].TotalTokens != 15 {
		t.Fatalf("unexpected latest bucket: %+v", series[1])
	}
}

func TestUsageStoreReset(t *testing.T) {
	s := memStore(t)
	_ = s.Insert(UsageRecord{TS: time.Now().Unix(), Success: true})
	n, err := s.ResetAll()
	if err != nil {
		t.Fatalf("ResetAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}
	sum, _ := s.Aggregate(0, time.Now().Unix()+1)
	if sum.Requests != 0 {
		t.Fatalf("expected empty after reset, got %+v", sum)
	}
}

func TestUsageStoreResetOlderThan(t *testing.T) {
	s := memStore(t)
	now := time.Now().Unix()
	_ = s.Insert(UsageRecord{TS: now, Success: true})
	_ = s.Insert(UsageRecord{TS: now - 100000, Success: true})

	n, err := s.ResetOlderThan(now - 1000)
	if err != nil {
		t.Fatalf("ResetOlderThan: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 old record deleted, got %d", n)
	}
	sum, _ := s.Aggregate(now-200000, now+1)
	if sum.Requests != 1 {
		t.Fatalf("expected 1 record remaining, got %+v", sum)
	}
}

func TestUsageStoreHistoryPagination(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()
	recs := make([]UsageRecord, 0, 25)
	for i := 0; i < 25; i++ {
		recs = append(recs, UsageRecord{
			TS: base - int64(25-i), ReqID: "req-" + fmt.Sprint(i),
			Model: "m", Success: i%2 == 0,
		})
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page1, err := s.History(1, 10)
	if err != nil {
		t.Fatalf("History page1: %v", err)
	}
	if page1.Total != 25 || len(page1.Items) != 10 {
		t.Fatalf("page1 total=%d items=%d, want 25/10", page1.Total, len(page1.Items))
	}
	// 时间倒序：最新（最大 TS）应在第 0 条。
	if page1.Items[0].ReqID != "req-24" {
		t.Fatalf("page1[0] = %q, want req-24 (最新)", page1.Items[0].ReqID)
	}

	page3, err := s.History(3, 10)
	if err != nil {
		t.Fatalf("History page3: %v", err)
	}
	if len(page3.Items) != 5 {
		t.Fatalf("page3 items=%d, want 5", len(page3.Items))
	}
	if page3.Items[0].ReqID != "req-4" {
		t.Fatalf("page3[0] = %q, want req-4", page3.Items[0].ReqID)
	}

	// limit 上限 200 校验
	if _, err := s.History(1, 9999); err != nil {
		t.Fatalf("History oversized limit should not error: %v", err)
	}
}

func TestUsageStoreErrorReason(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()
	_ = s.Insert(UsageRecord{TS: base, Model: "m", Success: false, StatusCode: 500, ErrorReason: "upstream timeout"})

	pg, err := s.History(1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	if pg.Items[0].ErrorReason != "upstream timeout" {
		t.Fatalf("ErrorReason=%q, want upstream timeout", pg.Items[0].ErrorReason)
	}
	if pg.Items[0].StatusCode != 500 || pg.Items[0].Success {
		t.Fatalf("status/success mismatch: %+v", pg.Items[0])
	}
}

// TestUsageStoreTraceFieldsRoundTrip 锁定新增追踪字段的写入→读取往返。
func TestUsageStoreTraceFieldsRoundTrip(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()
	_ = s.Insert(UsageRecord{
		TS:             base,
		ReqID:          "req-trace",
		Model:          "gemini-3.7-flash",
		RequestedModel: "抗截断-gemini-3.7-flash",
		Streaming:      false,
		ResponseMode:   "单次",
		AntiTruncate:   true,
		Success:        true,
		TTFBMs:         900,
		UpstreamTTFBMs: 300,
	})

	pg, err := s.History(1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	it := pg.Items[0]
	if it.Model != "gemini-3.7-flash" {
		t.Errorf("Model=%q", it.Model)
	}
	if it.RequestedModel != "抗截断-gemini-3.7-flash" {
		t.Errorf("RequestedModel=%q, 请求模型应保留变体前缀", it.RequestedModel)
	}
	if it.ResponseMode != "单次" {
		t.Errorf("ResponseMode=%q", it.ResponseMode)
	}
	if !it.AntiTruncate {
		t.Error("AntiTruncate 应为 true")
	}
	// 上游首字与客户端首字必须分开存储：两者之差即代理自身开销。
	if it.TTFBMs != 900 {
		t.Errorf("TTFBMs=%d, want 900", it.TTFBMs)
	}
	if it.UpstreamTTFBMs != 300 {
		t.Errorf("UpstreamTTFBMs=%d, want 300", it.UpstreamTTFBMs)
	}
}

// TestUsageInsertSQLColumnCountMatchesArgs 防漂移护栏：usageInsertSQL 的列数、
// 占位符数与 recArgs 长度三者必须严格一致。三者无编译期保护，任一不同步都会在
// 运行期才暴露为 "N values for M columns"，故在此用断言兜底。
func TestUsageInsertSQLColumnCountMatchesArgs(t *testing.T) {
	sqlText := usageInsertSQL

	open := strings.Index(sqlText, "(")
	close := strings.LastIndex(sqlText[:strings.Index(sqlText, "VALUES")], ")")
	if open < 0 || close < 0 {
		t.Fatalf("无法定位列清单: %q", sqlText)
	}
	colsPart := sqlText[open+1 : close]
	colCount := len(strings.Split(colsPart, ","))

	valuesIdx := strings.Index(sqlText, "VALUES")
	phPart := sqlText[valuesIdx:]
	phCount := strings.Count(phPart, "?")

	argCount := len(recArgs(UsageRecord{}, 1))

	if colCount != phCount {
		t.Errorf("列数 %d 与占位符数 %d 不一致: %s", colCount, phCount, sqlText)
	}
	if colCount != argCount {
		t.Errorf("列数 %d 与 recArgs 长度 %d 不一致", colCount, argCount)
	}
}

// TestUsageStoreFinishReasonRoundTrip 终止原因原样落库并读回。
// 存原始枚举而非布尔：上游新增枚举时无需再改表，展示层按需映射为截断标注。
func TestUsageStoreFinishReasonRoundTrip(t *testing.T) {
	s := memStore(t)
	base := time.Now().Unix()
	cases := []string{"MAX_TOKENS", "MALFORMED_FUNCTION_CALL", "STOP", ""}
	for i, fr := range cases {
		if err := s.Insert(UsageRecord{
			TS: base + int64(i), ReqID: "fr-" + fr, Model: "m",
			Success: true, FinishReason: fr,
		}); err != nil {
			t.Fatalf("Insert(%q): %v", fr, err)
		}
	}

	pg, err := s.History(1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != len(cases) {
		t.Fatalf("expected %d items, got %d", len(cases), len(pg.Items))
	}
	got := map[string]bool{}
	for _, it := range pg.Items {
		got[it.FinishReason] = true
	}
	for _, fr := range cases {
		if !got[fr] {
			t.Errorf("FinishReason %q 未原样读回，实际集合: %v", fr, got)
		}
	}
}

// TestAggregate_AvgUpstreamTTFBExcludesMissing 验证"平均首字"只统计确实采到上游首字节的记录：
// 未采到（upstream_ttfb_ms = 0）的记录既不参与平均，也不应把均值拉低。
// 这正是"平均首字与平均总耗时恒等"的根因——非流式记录的客户端首帧恒等于总耗时。
func TestAggregate_AvgUpstreamTTFBExcludesMissing(t *testing.T) {
	s := memStore(t)
	defer func() { _ = s.Close() }()

	now := time.Now().Unix()
	recs := []UsageRecord{
		// 有上游首字：100ms / 200ms → 均值 150ms
		{TS: now - 10, TotalMs: 5000, TTFBMs: 5000, UpstreamTTFBMs: 100},
		{TS: now - 9, TotalMs: 6000, TTFBMs: 6000, UpstreamTTFBMs: 200},
		// 未采到上游首字（如失败或纯本地端点）：TTFB 等于总耗时，不应污染均值
		{TS: now - 8, TotalMs: 7000, TTFBMs: 7000, UpstreamTTFBMs: 0},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	sum, err := s.Aggregate(now-60, now+60)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if sum.Requests != 3 {
		t.Fatalf("Requests=%d, want 3", sum.Requests)
	}
	if sum.AvgUpstreamTTFBMs != 150 {
		t.Errorf("AvgUpstreamTTFBMs=%v, want 150（仅统计采到首字的 2 条）", sum.AvgUpstreamTTFBMs)
	}
	// 对照：客户端首字均值会被未采到的 7000ms 拖高，且与总耗时口径重复。
	if sum.AvgTTFBMs != 6000 {
		t.Errorf("AvgTTFBMs=%v, want 6000（口径无关，仅作对照）", sum.AvgTTFBMs)
	}
	if sum.AvgUpstreamTTFBMs == sum.AvgTTFBMs {
		t.Error("平均首字不应与客户端首字均值（≈平均总耗时）相同")
	}
}
