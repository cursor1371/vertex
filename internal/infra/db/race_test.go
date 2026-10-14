package db

import "testing"

// TestByNodeGroupsByURI 验证按节点分组以 node_uri 为键：
// 同名但不同 URI 的节点不再被错误合并成一行。
func TestByNodeGroupsByURI(t *testing.T) {
	s, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	recs := []UsageRecord{
		{TS: 100, NodeName: "same", NodeURI: "vless://a", Success: true, TotalTokens: 10},
		{TS: 101, NodeName: "same", NodeURI: "vless://b", Success: true, TotalTokens: 20},
		{TS: 102, NodeName: "same", NodeURI: "vless://a", Success: false, TotalTokens: 5},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	rows, err := s.ByNode(0, 1000)
	if err != nil {
		t.Fatalf("ByNode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("同名不同 URI 应分为 2 行, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.RawURI == "" {
			t.Errorf("RawURI 不应为空: %+v", r)
		}
		if r.Name != "same" {
			t.Errorf("Name 应为 same: %+v", r)
		}
	}
}

// TestRaceAggregates 验证竞速记录按节点/时间桶聚合的计数正确。
func TestRaceAggregates(t *testing.T) {
	s, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	recs := []RaceRecord{
		{TS: 100, NodeName: "n1", NodeURI: "u1", Outcome: "win", ElapsedMs: 100},
		{TS: 101, NodeName: "n1", NodeURI: "u1", Outcome: "error", ElapsedMs: 200},
		{TS: 102, NodeName: "n2", NodeURI: "u2", Outcome: "timeout", ElapsedMs: 300},
		{TS: 103, NodeName: "n2", NodeURI: "u2", Outcome: "collect", ElapsedMs: 400},
		{TS: 104, NodeName: "n2", NodeURI: "u2", Outcome: "lost", ElapsedMs: 50},
	}
	if err := s.InsertRaceBatch(recs); err != nil {
		t.Fatalf("InsertRaceBatch: %v", err)
	}

	byNode, err := s.RaceByNode(0, 1000)
	if err != nil {
		t.Fatalf("RaceByNode: %v", err)
	}
	if len(byNode) != 2 {
		t.Fatalf("want 2 nodes, got %d: %+v", len(byNode), byNode)
	}
	var n1, n2 RaceNodeStat
	for _, st := range byNode {
		switch st.RawURI {
		case "u1":
			n1 = st
		case "u2":
			n2 = st
		}
	}
	if n1.Attempts != 2 || n1.Wins != 1 || n1.Errors != 1 || n1.Timeouts != 0 || n1.Lost != 0 {
		t.Errorf("n1 统计错误: %+v", n1)
	}
	if n2.Attempts != 3 || n2.Collects != 1 || n2.Timeouts != 1 || n2.Lost != 1 || n2.Wins != 0 {
		t.Errorf("n2 统计错误: %+v", n2)
	}

	series, err := s.RaceSeries(0, 1000, 3600)
	if err != nil {
		t.Fatalf("RaceSeries: %v", err)
	}
	if len(series) != 1 || series[0].Attempts != 5 || series[0].Wins != 1 || series[0].Lost != 1 {
		t.Errorf("时间桶统计错误: %+v", series)
	}

	if _, err := s.ResetRaceAll(); err != nil {
		t.Fatalf("ResetRaceAll: %v", err)
	}
	empty, err := s.RaceByNode(0, 1000)
	if err != nil {
		t.Fatalf("RaceByNode after reset: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("重置后应无记录, got %+v", empty)
	}
}

// TestRaceAggregates_RateLimitOutcome 守卫：outcome="ratelimit"（429 正常竞争限流）
// 单列计数、不进 Errors——此前 429 记为 error 会拉低节点 Racing 成功率。
func TestRaceAggregates_RateLimitOutcome(t *testing.T) {
	s, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	recs := []RaceRecord{
		{TS: 100, NodeName: "n1", NodeURI: "u1", Outcome: "win", ElapsedMs: 100},
		{TS: 101, NodeName: "n1", NodeURI: "u1", Outcome: "ratelimit", ElapsedMs: 50},
		{TS: 102, NodeName: "n1", NodeURI: "u1", Outcome: "ratelimit", ElapsedMs: 60},
		{TS: 103, NodeName: "n1", NodeURI: "u1", Outcome: "error", ElapsedMs: 200},
	}
	if err := s.InsertRaceBatch(recs); err != nil {
		t.Fatalf("InsertRaceBatch: %v", err)
	}
	rows, err := s.RaceByNode(0, 1000)
	if err != nil {
		t.Fatalf("RaceByNode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应聚合为 1 行, got %d", len(rows))
	}
	st := rows[0]
	if st.Attempts != 4 || st.Wins != 1 || st.Errors != 1 {
		t.Fatalf("attempts/wins/errors = %d/%d/%d, want 4/1/1", st.Attempts, st.Wins, st.Errors)
	}
	if st.RateLimits != 2 {
		t.Errorf("RateLimits=%d, want 2（429 独立计数）", st.RateLimits)
	}

	// 时间桶序列同步携带 RateLimits 且 Scan 顺序一致
	pts, err := s.RaceSeries(0, 1000, 3600)
	if err != nil {
		t.Fatalf("RaceSeries: %v", err)
	}
	if len(pts) != 1 || pts[0].RateLimits != 2 {
		t.Fatalf("RaceSeries RateLimits 应为 2, got %+v", pts)
	}
}
