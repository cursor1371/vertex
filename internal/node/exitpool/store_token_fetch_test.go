package exitpool

import (
	"testing"
	"time"
)

// TestRecordTokenFetch_OnlyWritesRTFields rT 失败只写 rT 专属字段，绝不碰通用字段：
// 不禁用节点、不降级、不打通用冷却——"对 rT 坏"不该侵蚀普通请求的候选池。
func TestRecordTokenFetch_OnlyWritesRTFields(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-rt", Name: "rt"}}) //nolint:exhaustruct

	for _, outcome := range []TokenFetchOutcome{TokenFetchFailed, TokenFetchHang} {
		mgr.RecordTokenFetch("uri-rt", outcome, 1234, "boom")
		if n := mgr.LoadNodes()[0]; n.Disabled {
			t.Fatalf("outcome=%v 不应禁用节点（rT 问题只应影响 rT 选点）", outcome)
		}
		h, hOK := mgr.LoadHealth()["uri-rt"]
		if !hOK || h.LastSubHealthyAt != 0 || h.CooldownUntil != 0 {
			t.Fatalf("outcome=%v 不应写通用降级/冷却字段: %+v", outcome, h)
		}
		if h.RTLastMs != 1234 || h.TokenFailStreak == 0 {
			t.Fatalf("outcome=%v 应写 rT 专属字段: %+v", outcome, h)
		}
	}
}

// TestRecordTokenFetch_HangEscalatingRTCooldown 挂住型的 rT 专属冷却按连败升级
// （30→60→120→…→封顶 600），全程不禁用节点；一次成功即彻底复位。
func TestRecordTokenFetch_HangEscalatingRTCooldown(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-hang", Name: "hang"}}) //nolint:exhaustruct

	wantSecs := []int{30, 60, 120, 240, 480, 600, 600}
	for i, want := range wantSecs {
		mgr.RecordTokenFetch("uri-hang", TokenFetchHang, 15000, "挂住")
		h, ok := mgr.LoadHealth()["uri-hang"]
		if !ok || h.RTHangStreak != i+1 {
			t.Fatalf("第 %d 次挂住 RTHangStreak 异常: %+v", i+1, h)
		}
		got := int(h.RTCooldownUntil - time.Now().Unix())
		if got < want-2 || got > want+2 {
			t.Fatalf("第 %d 次挂住 rT 冷却 ≈%d 秒, want ≈%d", i+1, got, want)
		}
		if n := mgr.LoadNodes()[0]; n.Disabled {
			t.Fatalf("第 %d 次挂住不应禁用节点", i+1)
		}
	}

	mgr.RecordTokenFetch("uri-hang", TokenFetchOK, 800, "")
	h, ok := mgr.LoadHealth()["uri-hang"]
	if !ok || h.RTHangStreak != 0 || h.TokenFailStreak != 0 || h.RTCooldownUntil != 0 || h.RTLastMs != 800 {
		t.Fatalf("成功应复位 rT 连败/冷却并记录耗时: %+v", h)
	}
}

// TestSelectForTokenFetch_RanksRTBadLast rT 选点把"对 rT 差/冷却中"的节点排到同层最后，
// 好节点优先；节点不足时才回退到它们（保证不会因 rT 质量彻底抓不到 token）。
func TestSelectForTokenFetch_RanksRTBadLast(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "uri-good", Name: "good"},
		{RawURI: "uri-slow", Name: "slow"},
		{RawURI: "uri-cool", Name: "cool"},
	})

	mgr.RecordTokenFetch("uri-slow", TokenFetchOK, rtSlowMsThreshold+1, "") // 对 rT 慢
	mgr.RecordTokenFetch("uri-cool", TokenFetchHang, 15000, "挂住")           // rT 冷却中

	got := mgr.SelectForTokenFetch(3, false)
	if len(got) != 3 {
		t.Fatalf("节点不足时应回退返回全部: %v", got)
	}
	if got[0].RawURI != "uri-good" {
		t.Fatalf("rT 健康节点应排最前, got %v", got)
	}
	if got[len(got)-1].RawURI != "uri-cool" {
		t.Fatalf("rT 冷却中的节点应排最后, got %v", got)
	}
}

// TestSelectForParallel_IgnoresRTHealth 回归锚点：普通请求选点完全不受 rT 健康度影响
// ——本次改动的核心保证（rT 问题不该侵蚀普通请求的候选池）。
//
// 断言方式：把 uri-a 打成"rT 最差"后，重复单点选点仍应像改动前一样轮转命中两个节点。
// 若 rT 健康度参与了普通排序，uri-a 会被挤到后面、几乎不再出现（或节点被禁用）。
// 注意不能用"两次调用的返回顺序相同"来断言——普通选点本身就按 rrIndex 轮转。
func TestSelectForParallel_IgnoresRTHealth(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "uri-a", Name: "a"},
		{RawURI: "uri-b", Name: "b"},
	})

	for i := 0; i < 5; i++ {
		mgr.RecordTokenFetch("uri-a", TokenFetchHang, 15000, "挂住")
		mgr.RecordTokenFetch("uri-a", TokenFetchFailed, 15000, "boom")
	}
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatal("rT 失败不应禁用节点")
	}

	hits := map[string]int{}
	for i := 0; i < 10; i++ {
		got := mgr.SelectForParallel(1, false)
		if len(got) != 1 {
			t.Fatalf("应返回 1 个节点: %v", got)
		}
		hits[got[0].RawURI]++
	}
	if hits["uri-a"] == 0 || hits["uri-b"] == 0 {
		t.Fatalf("普通选点应照常轮转命中两个节点（不受 rT 健康度影响）: %v", hits)
	}
}

// TestRecordTokenFetch_IgnoresEmptyURI 空 uri（直连/前置）不上报，避免把全局路径的失败
// 误归因到某个出口节点上。
func TestRecordTokenFetch_IgnoresEmptyURI(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-rt", Name: "rt"}}) //nolint:exhaustruct

	mgr.RecordTokenFetch("", TokenFetchFailed, 100, "boom")

	if _, exists := mgr.LoadHealth()[""]; exists {
		t.Fatal("空 uri 不应产生健康记录")
	}
}

// TestLockedNodeNotProtectedFromHealthAutoDisable 防删锁只豁免删除/去重/批量手动禁用，
// 健康触发的自动禁用必须生效（这里指请求路径的 RecordTest；rT 失败已不再禁用节点）：
// 否则实际连不通的节点会被反复选中。
func TestLockedNodeNotProtectedFromHealthAutoDisable(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-lock", Name: "locked", Locked: true}}) //nolint:exhaustruct

	// 达到连败门槛：防删锁不应豁免健康触发的自动禁用
	for i := 0; i < 3; i++ {
		mgr.RecordTest("uri-lock", false, 0, "dial tcp: connection refused")
	}

	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatal("防删锁不应豁免健康触发的自动禁用")
	}
	if !n.Locked {
		t.Fatal("自动禁用不应清掉防删锁本身")
	}

	// 恢复通道保持开放：手动启用不受防删限制（批量手动禁用仍豁免）。
	if !mgr.EnableNode("uri-lock") {
		t.Fatal("EnableNode 应能恢复被自动禁用的防删节点")
	}
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatal("手动启用后应回到可用")
	}
}

// TestRecordTokenFetch_SlowCountsTowardsRTRank 慢到超时（预算到点但尝试有完成）也要计入
// rT 健康度：原来这类被完全丢弃上报，导致慢节点永远拿不到降权、照样被反复选中。
func TestRecordTokenFetch_SlowCountsTowardsRTRank(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-slow", Name: "slow"}}) //nolint:exhaustruct

	mgr.RecordTokenFetch("uri-slow", TokenFetchSlow, 15000, "预算到点")
	h, ok := mgr.LoadHealth()["uri-slow"]
	if !ok || h.TokenFailStreak != 1 || h.RTLastMs != 15000 {
		t.Fatalf("慢到超时应记连败与耗时: %+v", h)
	}
	// 慢 ≠ 挂住：不升级 rT 专属冷却（那会过度惩罚"只是慢"的节点）。
	if h.RTHangStreak != 0 || h.RTCooldownUntil != 0 {
		t.Fatalf("慢到超时不应按挂住升级冷却: %+v", h)
	}
	if got := mgr.rtRankOf("uri-slow", time.Now().Unix()); got != 1 {
		t.Fatalf("慢节点应降权到 rank 1, got %d", got)
	}
}

// TestSelectForTokenFetch_StagedByRank rtRank 必须**分级生效**：rank 0（健康）取满 k 之后
// 才轮到 rank 1、再到 rank 2。此处 rank 2 节点 3 个、rank 0 节点 2 个，k=2 时只应命中
// rank 0 的两个——若像修复前那样"每个 rank 组各取一个"，冷却中的节点会被混进来。
func TestSelectForTokenFetch_StagedByRank(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "uri-ok-1", Name: "ok1"},
		{RawURI: "uri-ok-2", Name: "ok2"},
		{RawURI: "uri-cool-1", Name: "cool1"},
		{RawURI: "uri-cool-2", Name: "cool2"},
		{RawURI: "uri-cool-3", Name: "cool3"},
	})

	for _, u := range []string{"uri-cool-1", "uri-cool-2", "uri-cool-3"} {
		mgr.RecordTokenFetch(u, TokenFetchHang, 15000, "挂住")
	}

	got := mgr.SelectForTokenFetch(2, false)
	if len(got) != 2 {
		t.Fatalf("应返回 2 个: %v", got)
	}
	for _, n := range got {
		if n.RawURI != "uri-ok-1" && n.RawURI != "uri-ok-2" {
			t.Fatalf("k=2 时只应取健康的两个节点（分级取），实际: %v", got)
		}
	}

	// 健康池不够时才回退到降权节点。
	all := mgr.SelectForTokenFetch(5, false)
	if len(all) != 5 {
		t.Fatalf("节点不足时应回退返回全部: %v", all)
	}
}

// TestRTRanks_MatchesRtRankOf 守卫：RTRanks 批量快照必须与 rtRankOf 同判据。
// 特别覆盖本次前端漏筛 bug 的场景——节点曾挂住（RTHangStreak>0）、随后一次成功
// 清零 TokenFailStreak 且 RTLastMs 很快：rtRankOf 仍应给 rank 1（挂住史未清），
// 前端若自算漏掉 rt_hang_streak 就会把它误判为正常。
func TestRTRanks_MatchesRtRankOf(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "uri-good", Name: "good"}, //nolint:exhaustruct
		{RawURI: "uri-hang", Name: "hang"}, //nolint:exhaustruct
		{RawURI: "uri-cool", Name: "cool"}, //nolint:exhaustruct
	})

	mgr.RecordTokenFetch("uri-good", TokenFetchOK, 100, "")
	mgr.RecordTokenFetch("uri-hang", TokenFetchHang, 15000, "挂住")
	mgr.RecordTokenFetch("uri-hang", TokenFetchOK, 100, "") // 成功：清 streak 但 RTHangStreak 归零？
	mgr.RecordTokenFetch("uri-cool", TokenFetchHang, 15000, "挂住")

	ranks := mgr.RTRanks()
	if ranks["uri-good"] != 0 {
		t.Errorf("健康节点 rank=%d, want 0", ranks["uri-good"])
	}
	// 挂住后成功：RecordTokenFetch 的 OK 分支清 RTHangStreak——
	// 若此处 rank 仍为 1，说明挂住史未被成功清零，前端自算会与后端不一致。
	hangRank := ranks["uri-hang"]
	if want := mgr.rtRankOf("uri-hang", time.Now().Unix()); hangRank != want {
		t.Errorf("RTRanks 与 rtRankOf 不一致: %d != %d", hangRank, want)
	}
	if ranks["uri-cool"] != 2 {
		t.Errorf("挂住节点应进入冷却层 rank=2, got %d", ranks["uri-cool"])
	}
}
