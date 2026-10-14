package entrypool

import (
	"path/filepath"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// TestEntryRecoveryChannelOnlyForHealthSource 前置池的恢复通道只有"测速成功"这一条，
// 且**只对 health 来源生效**：人工 / 单次 / 永久 / admin 测试 / 协议不支持一律不得被放开。
//
// 这条正面回答"前置池不执行周期性自动探测"之后的语义边界问题：
// 它仍能通过既有测速通道恢复"健康禁用"的节点，但绝不会绕过人工与永久设置。
func TestEntryRecoveryChannelOnlyForHealthSource(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		wantRecover bool
	}{
		{"健康禁用_测速成功应恢复", nodestore.DisableSourceHealth, true},
		{"人工禁用_不得恢复", nodestore.DisableSourceManual, false},
		{"单次禁用_不得恢复", nodestore.DisableSourceSingleShot, false},
		{"永久禁用_不得恢复", nodestore.DisableSourcePermanent, false},
		{"admin测试禁用_不得恢复", nodestore.DisableSourceAdminTest, false},
		{"协议不支持_不得恢复", nodestore.DisableSourceUnsupported, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newMemEntryMgr()
			mgr.MergeNodes([]Node{{RawURI: "entry-x", Name: "x"}}) //nolint:exhaustruct
			mgr.BatchDisableWithSource([]string{"entry-x"}, tc.source, "测试原因", "node_network")

			if n := mgr.LoadNodes()[0]; !n.Disabled {
				t.Fatalf("前置条件：%q 来源应已禁用", tc.source)
			}
			// 走既有测速通道
			mgr.RecordTest("entry-x", true, 80, "")

			got := mgr.LoadNodes()[0].Disabled
			if tc.wantRecover {
				if got {
					t.Errorf("来源 %q 应被测速成功恢复", tc.source)
				}
				return
			}
			if !got {
				t.Errorf("来源 %q 不得被测速成功放开（自动路径不得覆盖人工/永久设置）", tc.source)
			}
		})
	}
}

// TestEntryFaultRecordScopeAndSource 故障记录的「所属」与「来源」必须反映事件真实出处：
// 前置池写入的记录 scope=entry、source=request，且同因多次发生按聚合计数。
// 面板的「所属」列与「发现来源」列直接消费这两个字段。
func TestEntryFaultRecordScopeAndSource(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "entry-fault.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mgr := NewEntryManager(database, nil, EntryHooks{}, func() int { return 2 })
	mgr.MergeNodes([]Node{{RawURI: "entry-1", Name: "e1"}}) //nolint:exhaustruct

	// 同因失败两次（请求/测速路径）
	mgr.RecordTest("entry-1", false, 0, "dial tcp: connection refused")
	mgr.RecordTest("entry-1", false, 0, "dial tcp: connection refused")

	rows, err := nodestore.LoadFaults(database, 0)
	if err != nil {
		t.Fatalf("LoadFaults: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同因失败应聚合为 1 条, got %d", len(rows))
	}
	got := rows[0]
	if got.Scope != "entry" {
		t.Errorf("所属 scope=%q, want entry（前置池写入的记录不能标成出口池）", got.Scope)
	}
	if got.Source != nodestore.ProbeSourceRequest {
		t.Errorf("来源 source=%q, want %q（测速/请求路径的真实出处）", got.Source, nodestore.ProbeSourceRequest)
	}
	if got.FailureClass != string(nodestore.FailureNodeNetwork) {
		t.Errorf("类别=%q, want node_network", got.FailureClass)
	}
	if got.NodeName != "e1" || got.NodeURI != "entry-1" {
		t.Errorf("节点定位字段不完整: %+v", got)
	}
	if got.Count != 2 {
		t.Errorf("同因两次应聚合计数为 2, got %d", got.Count)
	}

	// 放行一次成功：不应产生新的故障记录（成功不是故障）。
	before, _, _ := nodestore.FaultTotals(database)
	mgr.RecordTest("entry-1", true, 60, "")
	after, _, _ := nodestore.FaultTotals(database)
	if after != before {
		t.Errorf("成功不该写入故障记录: rows %d→%d", before, after)
	}
}

// TestEntryPolicyRecordOnlyWritesNoDisable 前置池把类别配成"不禁用"后：
// 失败只记账 + 写故障记录，节点保持可用（与出口池同一张策略表，口径一致）。
func TestEntryPolicyRecordOnlyWritesNoDisable(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "entry-policy.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	set := nodestore.NewPolicySet(nil)
	set.Replace([]nodestore.FaultPolicy{{
		Class: nodestore.FailureNodeNetwork, Action: nodestore.PolicyRecordOnly,
		ProbeFailThreshold: nodestore.ProbeThresholdFollowGlobal,
	}})
	mgr := NewEntryManager(database, nil, EntryHooks{}, func() int { return 2 })
	mgr.SetPolicySet(set)
	mgr.MergeNodes([]Node{{RawURI: "entry-ro", Name: "ro"}}) //nolint:exhaustruct

	for i := 0; i < 3; i++ {
		mgr.RecordTest("entry-ro", false, 0, "dial tcp: connection refused")
	}
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatal("策略为不禁用时不得摘出前置节点")
	}
	rows, _ := nodestore.LoadFaults(database, 0)
	if len(rows) != 1 || rows[0].Count != 3 {
		t.Fatalf("失败仍应照常进故障记录并聚合计数, got %+v", rows)
	}
}
