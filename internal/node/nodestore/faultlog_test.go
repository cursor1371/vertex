package nodestore

import (
	"path/filepath"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// TestRecordFaultAggregates 锁定故障记录的聚合语义：同 (scope, 节点, 类别, 签名) 反复发生时
// 只累加次数并刷新最近时间，首次时间保持不变——面板要的是"发生 137 次、从 X 到 Y"，
// 而不是 137 行重复记录。
func TestRecordFaultAggregates(t *testing.T) {
	setupDB(t)

	info := ClassifyFailure("dial tcp 1.2.3.4:443: connect: connection refused")
	base := FaultRow{
		Scope: "exit", NodeURI: "vless://a@h:443", NodeName: "n1",
		FailureClass: string(info.Class), HTTPStatus: info.Status, ErrorCode: info.Code,
		ErrorSig: info.Sig, RawError: info.Detail, Source: ProbeSourceRequest,
	}

	first := base
	first.FirstSeenAt, first.LastSeenAt, first.Count = 1000, 1000, 1
	if err := RecordFault(testDB, first); err != nil {
		t.Fatalf("RecordFault 首次: %v", err)
	}
	for i := 0; i < 4; i++ {
		again := base
		ts := int64(2000 + i)
		again.FirstSeenAt, again.LastSeenAt, again.Count = ts, ts, 1
		if err := RecordFault(testDB, again); err != nil {
			t.Fatalf("RecordFault 第 %d 次: %v", i+2, err)
		}
	}

	rows, err := LoadFaults(testDB, 0)
	if err != nil {
		t.Fatalf("LoadFaults: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同因故障应聚合为 1 行，got %d", len(rows))
	}
	got := rows[0]
	if got.Count != 5 {
		t.Errorf("Count=%d, want 5", got.Count)
	}
	if got.FirstSeenAt != 1000 {
		t.Errorf("FirstSeenAt=%d, want 1000（首次发生时间不应被后续事件覆盖）", got.FirstSeenAt)
	}
	if got.LastSeenAt != 2003 {
		t.Errorf("LastSeenAt=%d, want 2003（最近发生时间应刷新）", got.LastSeenAt)
	}
}

// TestRecordFaultSeparatesByClassAndNode 不同节点、不同类别、不同签名必须分开成行，
// 否则面板的分类统计与节点定位都会失真。
func TestRecordFaultSeparatesByClassAndNode(t *testing.T) {
	setupDB(t)

	refused := ClassifyFailure("dial tcp: connect: connection refused")
	dns := ClassifyFailure("lookup a.b: no such host")

	rows := []FaultRow{
		{Scope: "exit", NodeURI: "u1", FailureClass: string(refused.Class), ErrorSig: refused.Sig, FirstSeenAt: 1, LastSeenAt: 1},
		{Scope: "exit", NodeURI: "u2", FailureClass: string(refused.Class), ErrorSig: refused.Sig, FirstSeenAt: 1, LastSeenAt: 1},
		{Scope: "entry", NodeURI: "u1", FailureClass: string(refused.Class), ErrorSig: refused.Sig, FirstSeenAt: 1, LastSeenAt: 1},
		{Scope: "exit", NodeURI: "u1", FailureClass: string(dns.Class), ErrorSig: dns.Sig, FirstSeenAt: 1, LastSeenAt: 1},
	}
	for _, r := range rows {
		if err := RecordFault(testDB, r); err != nil {
			t.Fatalf("RecordFault: %v", err)
		}
	}

	got, err := LoadFaults(testDB, 0)
	if err != nil {
		t.Fatalf("LoadFaults: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("不同节点/类别/池应各成一行，want 4 rows, got %d", len(got))
	}

	stats, err := FaultClassCounts(testDB)
	if err != nil {
		t.Fatalf("FaultClassCounts: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("应按类别汇总为 2 组，got %d", len(stats))
	}
	byClass := map[string]FaultClassStats{}
	for _, s := range stats {
		byClass[s.Class] = s
	}
	if n := byClass[string(refused.Class)]; n.Count != 3 || n.Nodes != 2 || n.Rows != 3 {
		t.Errorf("网络类统计=%+v, want count=3 nodes=2 rows=3", n)
	}
	if n := byClass[string(dns.Class)]; n.Count != 1 || n.Nodes != 1 {
		t.Errorf("DNS 类统计=%+v, want count=1 nodes=1", n)
	}
}

// TestClearFaultLog 清空必须只动故障表，不影响节点/健康数据。
func TestClearFaultLog(t *testing.T) {
	setupDB(t)

	if err := SaveNodes(testDB, "nodes", []NodeRow{{RawURI: "uri1", Type: "ss", Name: "n1"}}); err != nil {
		t.Fatalf("SaveNodes: %v", err)
	}
	if err := RecordFault(testDB, FaultRow{Scope: "exit", NodeURI: "uri1", FailureClass: "other", ErrorSig: "x", FirstSeenAt: 1, LastSeenAt: 1}); err != nil {
		t.Fatalf("RecordFault: %v", err)
	}
	n, err := ClearFaultLog(testDB)
	if err != nil {
		t.Fatalf("ClearFaultLog: %v", err)
	}
	if n != 1 {
		t.Errorf("ClearFaultLog 删除 %d 行, want 1", n)
	}
	if rows, _ := LoadFaults(testDB, 0); len(rows) != 0 {
		t.Errorf("清空后应无故障记录, got %d", len(rows))
	}
	if err := LoadNodesFull(testDB, "nodes", func(NodeRow) {}); err != nil {
		t.Errorf("清空故障记录不应影响节点表: %v", err)
	}
}

// TestPruneFaultLogFixedRetention 锁定"故障记录只保留 1 天"：按 last_seen_at 判龄，
// 只删超期行，且与日志表的可配置保留期无关。
func TestPruneFaultLogFixedRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault-retention.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := int64(2_000_000_000)
	if err := RecordFault(database, FaultRow{Scope: "exit", NodeURI: "fresh", FailureClass: "other", ErrorSig: "s1", FirstSeenAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("RecordFault fresh: %v", err)
	}
	oldTs := now - 3*86400
	if err := RecordFault(database, FaultRow{Scope: "exit", NodeURI: "stale", FailureClass: "other", ErrorSig: "s2", FirstSeenAt: oldTs, LastSeenAt: oldTs}); err != nil {
		t.Fatalf("RecordFault stale: %v", err)
	}

	if db.FaultLogRetentionDays != 1 {
		t.Fatalf("故障记录保留天数应为固定 1 天, got %d", db.FaultLogRetentionDays)
	}
	deleted, err := db.PruneFaultLog(database, db.FaultLogCutoff(now))
	if err != nil {
		t.Fatalf("PruneFaultLog: %v", err)
	}
	if deleted != 1 {
		t.Errorf("应只删 1 行超期记录, got %d", deleted)
	}
	rows, err := LoadFaults(database, 0)
	if err != nil {
		t.Fatalf("LoadFaults: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeURI != "fresh" {
		t.Fatalf("应保留未超期的记录, got %+v", rows)
	}
	// 边界：正好 1 天前的记录（cutoff 之上）不应被误删。
	edge := now - 86400 + 1
	if err := RecordFault(database, FaultRow{Scope: "exit", NodeURI: "edge", FailureClass: "other", ErrorSig: "s3", FirstSeenAt: edge, LastSeenAt: edge}); err != nil {
		t.Fatalf("RecordFault edge: %v", err)
	}
	deleted, err = db.PruneFaultLog(database, db.FaultLogCutoff(now))
	if err != nil {
		t.Fatalf("PruneFaultLog 第二次: %v", err)
	}
	if deleted != 0 {
		t.Errorf("未超期记录不应被删, deleted=%d", deleted)
	}
}
