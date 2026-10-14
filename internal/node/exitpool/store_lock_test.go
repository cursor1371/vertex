package exitpool

import "testing"

// ---- 防删锁（locked）：锁定节点不被删除、禁用或去重移除，且状态可持久化 ----

func TestLockedNodeProtectedFromDelete(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "uri-lock", Type: "vless", Name: "lock"},
		{RawURI: "uri-free", Type: "vless", Name: "free"},
	})
	mgr.BatchUpdateNodesLocked([]string{"uri-lock"}, true)

	mgr.BatchDeleteNodes([]string{"uri-lock", "uri-free"})

	got := mgr.LoadNodes()
	if len(got) != 1 || got[0].RawURI != "uri-lock" {
		t.Fatalf("locked 节点应保留、未锁定节点应删除, got %+v", got)
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("DB 剩余行数=%d, want 1", count)
	}
}

func TestLockedNodeProtectedFromDisable(t *testing.T) {
	mgr, _ := setupDBMgr(t, nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-lock", Type: "vless", Name: "lock"}})
	mgr.BatchUpdateNodesLocked([]string{"uri-lock"}, true)

	mgr.BatchUpdateNodesDisabled([]string{"uri-lock"}, true)
	if n := mgr.LoadNodes()[0]; n.Disabled {
		t.Fatal("locked 节点不应被批量禁用")
	}

	// 解锁后可正常禁用
	mgr.BatchUpdateNodesLocked([]string{"uri-lock"}, false)
	mgr.BatchUpdateNodesDisabled([]string{"uri-lock"}, true)
	if n := mgr.LoadNodes()[0]; !n.Disabled {
		t.Fatal("解锁后应可禁用")
	}
}

func TestLockedDisabledNodeSurvivesDeleteDisabled(t *testing.T) {
	mgr, _ := setupDBMgr(t, nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "uri-lock", Type: "vless", Name: "lock"},
		{RawURI: "uri-free", Type: "vless", Name: "free"},
	})
	mgr.BatchUpdateNodesDisabled([]string{"uri-lock", "uri-free"}, true)
	mgr.BatchUpdateNodesLocked([]string{"uri-lock"}, true)

	mgr.DeleteDisabled()
	got := mgr.LoadNodes()
	if len(got) != 1 || got[0].RawURI != "uri-lock" {
		t.Fatalf("DeleteDisabled 应保留 locked 节点, got %+v", got)
	}
}

func TestLockedNodeSurvivesDedup(t *testing.T) {
	mgr, _ := setupDBMgr(t, identityByFragment{}, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "vless://a@h:443#dup", Type: "vless", Name: "first"},
		{RawURI: "vless://a@h:443#dup2", Type: "vless", Name: "second"},
	})
	// 锁定后一个（按身份键本会被去重移除的那个）
	mgr.BatchUpdateNodesLocked([]string{"vless://a@h:443#dup2"}, true)

	mgr.DedupNodes()
	for _, n := range mgr.LoadNodes() {
		if n.RawURI == "vless://a@h:443#dup2" {
			return
		}
	}
	t.Fatalf("locked 节点不应被去重移除, got %+v", mgr.LoadNodes())
}

// TestDedupRemovesDuplicateImportedNodeWhenExistingIsLocked 覆盖用户提问的场景：
// 已存在的节点带防删锁，随后导入一个与之身份重复的新节点，去重应删掉"新导入的那个"，
// 而不是删掉被锁定的旧节点，也不会因为锁而漏掉去重。
func TestDedupRemovesDuplicateImportedNodeWhenExistingIsLocked(t *testing.T) {
	mgr, database := setupDBMgr(t, identityByFragment{}, Hooks{})
	// 已存在的节点（先导入），随后上防删锁
	mgr.MergeNodes([]Node{{RawURI: "vless://a@h:443#old", Type: "vless", Name: "old"}})
	mgr.BatchUpdateNodesLocked([]string{"vless://a@h:443#old"}, true)

	// 导入身份重复的新节点（同 server 同凭证，fragment 不同 → 身份键相同）
	mgr.MergeNodes([]Node{{RawURI: "vless://a@h:443#new", Type: "vless", Name: "new"}})

	removed := mgr.DedupNodes()
	if removed != 1 {
		t.Fatalf("应移除 1 个重复节点, got %d", removed)
	}
	got := mgr.LoadNodes()
	if len(got) != 1 {
		t.Fatalf("去重后应只剩 1 个节点, got %+v", got)
	}
	if got[0].RawURI != "vless://a@h:443#old" {
		t.Errorf("应保留被锁定的旧节点, got %s", got[0].RawURI)
	}
	if !got[0].Locked {
		t.Error("保留的旧节点应保持防删锁")
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("DB 剩余行数=%d, want 1", count)
	}
}

// TestDedupLockedNewImportKeepsBoth 覆盖反向顺序：先有未锁定的旧节点，
// 再导入重复的新节点并给它上锁——此时两个都会被保留（宁可少去重也不动锁定节点）。
func TestDedupLockedNewImportKeepsBoth(t *testing.T) {
	mgr, _ := setupDBMgr(t, identityByFragment{}, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "vless://a@h:443#old", Type: "vless", Name: "old"}})
	mgr.MergeNodes([]Node{{RawURI: "vless://a@h:443#new", Type: "vless", Name: "new"}})
	// 给新导入的节点上锁
	mgr.BatchUpdateNodesLocked([]string{"vless://a@h:443#new"}, true)

	removed := mgr.DedupNodes()
	if removed != 0 {
		t.Fatalf("锁定新节点后不应移除任何节点, got removed=%d", removed)
	}
	if got := mgr.LoadNodes(); len(got) != 2 {
		t.Fatalf("两个节点都应保留, got %+v", got)
	}
}

// TestDedupDisabledOldVsNewEnabled 验证组内择优语义（P5 改动，替代原"先到先留"）：
// 旧节点已禁用、导入身份重复的新（启用）节点时，保留可用的新节点、
// 删除已禁用的旧节点——修复"导入好节点反而被丢"的数据损失。
func TestDedupDisabledOldVsNewEnabled(t *testing.T) {
	mgr, _ := setupDBMgr(t, identityByFragment{}, Hooks{})
	// 旧节点：已禁用（例如测试失败被自动禁用）
	mgr.MergeNodes([]Node{{RawURI: "vless://a@h:443#old", Type: "vless", Name: "old", Disabled: true}})
	// 导入身份重复的新节点：默认启用
	mgr.MergeNodes([]Node{{RawURI: "vless://a@h:443#new", Type: "vless", Name: "new"}})

	if removed := mgr.DedupNodes(); removed != 1 {
		t.Fatalf("应移除 1 个重复节点, got %d", removed)
	}
	got := mgr.LoadNodes()
	if len(got) != 1 {
		t.Fatalf("应只剩 1 个节点, got %+v", got)
	}
	// 择优语义：未禁用者优先，保留后导入的启用节点。
	if got[0].RawURI != "vless://a@h:443#new" {
		t.Errorf("应保留可用的 #new, got %s", got[0].RawURI)
	}
	if got[0].Disabled {
		t.Errorf("保留的节点应为启用态, got %+v", got[0])
	}
}

func TestLockedFlagPersists(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-lock", Type: "vless", Name: "lock"}})
	mgr.BatchUpdateNodesLocked([]string{"uri-lock"}, true)

	reborn := NewManager(database, nil, Hooks{}, nil)
	nodes := reborn.LoadNodes()
	if len(nodes) != 1 || !nodes[0].Locked {
		t.Fatalf("locked 标志应持久化, got %+v", nodes)
	}
}
