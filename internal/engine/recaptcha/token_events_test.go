package recaptcha

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestTokenEventRing_OrderAndCap 记录流按 newest first 返回、容量有界、自动补时间戳。
func TestTokenEventRing_OrderAndCap(t *testing.T) {
	var ring tokenEventRing
	for i := 0; i < tokenEventRingSize+20; i++ {
		ring.record(TokenEvent{Source: TokenSourceLive, OK: true})
	}

	got := ring.recent(0)
	if len(got) != tokenEventRingSize {
		t.Fatalf("容量应封顶在 %d，实际 %d", tokenEventRingSize, len(got))
	}
	if got[0].Seq != int64(tokenEventRingSize+20) {
		t.Fatalf("最新一条 seq 应为 %d，实际 %d", tokenEventRingSize+20, got[0].Seq)
	}
	if got[0].Seq <= got[len(got)-1].Seq {
		t.Fatalf("应 newest first: 首条 seq=%d 末条 seq=%d", got[0].Seq, got[len(got)-1].Seq)
	}
	if got[0].At.IsZero() {
		t.Fatal("未显式给 At 时应自动补当前时刻")
	}
	if l := ring.recent(3); len(l) != 3 {
		t.Fatalf("limit=3 应返回 3 条，实际 %d", len(l))
	}
}

// TestTokenPool_WarmHitRecordsEvent 池命中进记录流：来源"池命中"、无节点、成功。
func TestTokenPool_WarmHitRecordsEvent(t *testing.T) {
	fetch := &countingFetch{}
	warm := newWarmPool(fakeWarmCfg{enabled: true, depth: 1, ttlSec: 90}, fetch.fn)
	defer warm.Close()
	warm.put("tok-warm")

	p := &TokenPool{warm: warm} //nolint:exhaustruct
	tok, err := p.GetTokenShared(context.Background())
	if err != nil || tok != "tok-warm" {
		t.Fatalf("应命中池内 token: tok=%q err=%v", tok, err)
	}

	events := p.RecentTokenEvents(1)
	if len(events) != 1 {
		t.Fatalf("应记录 1 条事件，实际 %d", len(events))
	}
	ev := events[0]
	if ev.Source != TokenSourceWarm || !ev.OK || ev.NodeName != "" || ev.Err != "" {
		t.Fatalf("池命中事件异常: %+v", ev)
	}
}

// TestTokenPool_RecordTokenEventMapping 事件字段映射：节点展示名、入口/直连兜底、失败原因与耗时。
func TestTokenPool_RecordTokenEventMapping(t *testing.T) {
	p := &TokenPool{source: &fakeNodeSource{name: "hk-01"}} //nolint:exhaustruct

	p.recordTokenEvent(TokenSourceLive, tokenFetchResult{tok: "t1", node: "vless://a@h:443#n1", rtRank: 0}, 320*time.Millisecond)
	p.recordTokenEvent(TokenSourceLive, tokenFetchResult{err: errors.New("dial timeout")}, 0)
	p.recordTokenEvent(TokenSourcePrefetch, tokenFetchResult{node: "vless://a@h:443#n1", rtRank: 2, err: errors.New("boom")}, time.Second)
	// noPath：禁止直连且无可用候选，压根没尝试任何路径 → 节点留空。
	p.recordTokenEvent(TokenSourceLive, tokenFetchResult{noPath: true, err: errors.New("禁止直连抓取 rT")}, 0)

	got := p.RecentTokenEvents(0)
	if len(got) != 4 {
		t.Fatalf("应记录 4 条事件，实际 %d", len(got))
	}
	// newest first：noPath → 预取失败 → 入口/直连失败 → 实时成功。
	if got[0].NodeName != "" || got[0].OK {
		t.Fatalf("noPath 事件不应标节点: %+v", got[0])
	}
	if got[1].Source != TokenSourcePrefetch || got[1].OK || got[1].LatencyMs != 1000 {
		t.Fatalf("预取事件异常: %+v", got[1])
	}
	if got[1].NodeName != "hk-01" {
		t.Fatalf("节点展示名应为 hk-01，实际 %q", got[1].NodeName)
	}
	if got[1].Err != "boom" {
		t.Fatalf("失败原因应保留，实际 %q", got[1].Err)
	}
	if got[1].RTRank != 2 {
		t.Fatalf("选点时刻的 rT 降权层应带出（供展示解释为何用它）, 实际 %d", got[1].RTRank)
	}
	if got[2].NodeName != tokenNodeEntry || got[2].Err != "dial timeout" {
		t.Fatalf("入口/直连事件异常: %+v", got[2])
	}
	if !got[3].OK || got[3].LatencyMs != 320 || got[3].NodeName != "hk-01" {
		t.Fatalf("实时抓取事件异常: %+v", got[3])
	}
}

// TestTokenPool_RecentTokenEventsNilSafe 未装配（测试用 NewTokenPoolCustom）时不得 panic。
func TestTokenPool_RecentTokenEventsNilSafe(t *testing.T) {
	var p *TokenPool
	if got := p.RecentTokenEvents(5); got != nil {
		t.Fatalf("nil pool 应返回 nil，实际 %v", got)
	}
	p2 := &TokenPool{} //nolint:exhaustruct
	if got := p2.RecentTokenEvents(5); len(got) != 0 {
		t.Fatalf("无事件时应返回空，实际 %v", got)
	}
}

// TestTokenPool_RecordTokenEventCanceled 被 ctx 取消的抓取标记 Canceled（中性展示、不算失败）；
// 真·预算到点（DeadlineExceeded）不标——两者混在一起正是之前"1s 的 ✗"被误读成节点故障的原因。
func TestTokenPool_RecordTokenEventCanceled(t *testing.T) {
	p := &TokenPool{} //nolint:exhaustruct
	p.recordTokenEvent(TokenSourceLive, tokenFetchResult{err: fmt.Errorf(
		"节点 X 抓取被取消（竞速其它候选胜出或请求中断）: %w", context.Canceled)}, time.Second)
	p.recordTokenEvent(TokenSourceLive, tokenFetchResult{err: context.DeadlineExceeded}, 15*time.Second)

	got := p.RecentTokenEvents(0)
	if len(got) != 2 {
		t.Fatalf("应记录 2 条, 实际 %d", len(got))
	}
	// newest first：超时在前，取消在后。
	if got[1].Canceled != true || got[1].OK {
		t.Fatalf("被取消的抓取应标记 Canceled 且不算成功: %+v", got[1])
	}
	if got[0].Canceled {
		t.Fatalf("真超时不应标记 Canceled: %+v", got[0])
	}
}
