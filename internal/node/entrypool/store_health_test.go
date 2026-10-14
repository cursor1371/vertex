package entrypool

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// TestLockedEntryNodeNotProtectedFromHealthAutoDisable 前置池与出口池同构：防删锁只豁免
// 删除/去重/批量手动禁用，健康触发的自动禁用必须生效，否则连不通的前置节点会被反复选中。
func TestLockedEntryNodeNotProtectedFromHealthAutoDisable(t *testing.T) {
	mgr := NewEntryManager(nil, nil, EntryHooks{}, func() int { return 1 })
	mgr.MergeNodes([]Node{{RawURI: "entry-lock", Name: "locked", Locked: true}}) //nolint:exhaustruct

	// 达到连败门槛（注入 1，一次即禁）：防删锁不应豁免健康触发的自动禁用
	mgr.RecordTest("entry-lock", false, 0, "dial tcp: connection refused")

	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatal("防删锁不应豁免健康触发的自动禁用")
	}
	if !n.Locked {
		t.Fatal("自动禁用不应清掉防删锁本身")
	}
	if n.DisableSource != "health" {
		t.Fatalf("禁用来源应为 health（可自动恢复），got %q", n.DisableSource)
	}

	// 恢复通道保持开放：手动启用不受防删限制。
	mgr.BatchUpdateNodesDisabled([]string{"entry-lock"}, false)
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatal("手动启用后应回到可用")
	}
}

// TestEntryPolicyDrivenRemovalAndRecovery 前置池与出口池共用同一策略表：
// 默认策略为「禁用后恢复探测」的类别**首次**失败即摘出（阈值只管探测阶段），
// 成功测速可恢复；人工禁用的节点不受影响。
func TestEntryPolicyDrivenRemovalAndRecovery(t *testing.T) {
	// 注入最"苛刻"的探测阈值 3：首次移出仍应立刻发生（不受阈值门控）。
	mgr := NewEntryManager(nil, nil, EntryHooks{}, func() int { return 3 })
	mgr.MergeNodes([]Node{{RawURI: "entry-1", Name: "e1"}}) //nolint:exhaustruct

	mgr.RecordTest("entry-1", false, 0, "dial tcp timeout")
	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatalf("首次网络类失败即应摘出（探测阈值不门控首次移出）")
	}
	if n.DisableSource != "health" {
		t.Errorf("来源=%q, want health（该类策略为禁用后恢复探测）", n.DisableSource)
	}

	// 成功测速：恢复（前置池不执行自动探测，恢复靠测速成功或人工启用）
	mgr.RecordTest("entry-1", true, 100, "")
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatalf("成功测速应恢复该节点的禁用")
	}

	// 人工禁用：成功测速不得自动放开
	mgr.RecordTest("entry-1", false, 0, "dial tcp timeout")
	mgr.BatchUpdateNodesDisabled([]string{"entry-1"}, true)
	mgr.RecordTest("entry-1", true, 100, "")
	if n := mgr.LoadNodes()[0]; !n.Disabled {
		t.Fatalf("人工禁用的节点不得被成功测速自动放开")
	}
}

// TestEntry403RemovedByDefault 评审第 2 条：403 默认策略为「禁用后恢复探测」，
// 生产请求首次 403 即摘出（不再"永不自动禁用"）。
func TestEntry403RemovedByDefault(t *testing.T) {
	mgr := newMemEntryMgr()
	mgr.MergeNodes([]Node{{RawURI: "entry-403", Name: "e403"}}) //nolint:exhaustruct

	mgr.RecordTest("entry-403", false, 0, "unexpected HTTP response status: 403")
	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatal("403 应按默认策略摘出节点")
	}
	if n.DisableSource != "health" {
		t.Errorf("来源=%q, want health", n.DisableSource)
	}
	if n.DisableFailureClass != "permission" {
		t.Errorf("归因类别=%q, want permission", n.DisableFailureClass)
	}
}

// TestEntrySingleShotPolicy 把某类别配成「单次禁用」后：摘出且不进入恢复通道
// （promotion 到 permanent 的探测阶段也轮不到它）。
func TestEntrySingleShotPolicy(t *testing.T) {
	set := nodestore.NewPolicySet(nil)
	set.Replace([]nodestore.FaultPolicy{{
		Class: nodestore.FailureNodeNetwork, Action: nodestore.PolicySingleShot,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}})
	mgr := newMemEntryMgr()
	mgr.SetPolicySet(set)
	mgr.MergeNodes([]Node{{RawURI: "entry-shot", Name: "shot"}}) //nolint:exhaustruct

	mgr.RecordTest("entry-shot", false, 0, "dial tcp: connection refused")
	n := mgr.LoadNodes()[0]
	if !n.Disabled {
		t.Fatal("单次禁用策略下首次失败应摘出")
	}
	if n.DisableSource != nodestore.DisableSourceSingleShot {
		t.Errorf("来源=%q, want %q", n.DisableSource, nodestore.DisableSourceSingleShot)
	}
	// 单次禁用不进恢复通道：成功测速也不得自动放开。
	mgr.RecordTest("entry-shot", true, 100, "")
	if n := mgr.LoadNodes()[0]; !n.Disabled {
		t.Fatal("单次禁用的节点不得被自动放开（只能人工启用）")
	}
}

// TestEntryDNSFailureNeverDisables 评审第 3 条：DNS 解析失败只记录，永不触发自动禁用
// （即使连败远超阈值）。
func TestEntryDNSFailureNeverDisables(t *testing.T) {
	mgr := NewEntryManager(nil, nil, EntryHooks{}, func() int { return 1 })
	mgr.MergeNodes([]Node{{RawURI: "entry-dns", Name: "dns"}}) //nolint:exhaustruct

	for i := 0; i < 5; i++ {
		mgr.RecordTest("entry-dns", false, 0, "lookup ct.cloudflare.byoip.top: (exchange4: NXDOMAIN | exchange6: NXDOMAIN)")
	}
	n := mgr.LoadNodes()[0]
	if n.Disabled {
		t.Fatal("DNS 解析失败不得触发自动禁用")
	}
	h := mgr.LoadHealth()["entry-dns"]
	if h.LastFailureClass != string(nodestore.FailureDNS) {
		t.Fatalf("失败类别应记为 dns，got %q", h.LastFailureClass)
	}
}

// TestEntryRecordOnlyClassesNeverDisable 默认策略为「不禁用」的类别即便反复失败也不得摘出：
// 429/5xx 属上游公共故障，业务超时只降权，签名缺失是请求构造问题，other 是兜底。
// （403 已按评审第 2 条改为默认摘出，见 TestEntry403RemovedByDefault。）
func TestEntryRecordOnlyClassesNeverDisable(t *testing.T) {
	cases := []struct {
		name   string
		errStr string
	}{
		{"429限流", "unexpected HTTP response status: 429"},
		{"503上游", "status: 503 upstream busy"},
		{"业务超时", "context deadline exceeded"},
		{"签名缺失", "QUERY_SIGNATURE_NOT_FOUND"},
		{"other兜底", "eof"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewEntryManager(nil, nil, EntryHooks{}, func() int { return 1 })
			mgr.MergeNodes([]Node{{RawURI: "entry-x", Name: "x"}}) //nolint:exhaustruct
			for i := 0; i < 3; i++ {
				mgr.RecordTest("entry-x", false, 0, tc.errStr)
			}
			if n := mgr.LoadNodes()[0]; n.Disabled {
				t.Fatalf("%s 不应触发自动禁用", tc.name)
			}
		})
	}
}
