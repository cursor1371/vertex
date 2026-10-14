package exitpool

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// ---- 测试基础设施（实例注入风格）----

// hookRecorder 记录 InvalidateParsed 收到的全部 URI（保持到达顺序）。
type hookRecorder struct{ uris []string }

func (h *hookRecorder) hooks() Hooks {
	return Hooks{InvalidateParsed: func(uris []string) { h.uris = append(h.uris, uris...) }}
}

// identityByFragment 以片段剥离结果为身份键：同 server 同凭证不同 RawURI 视为同节点。
type identityByFragment struct{}

func (identityByFragment) Identity(rawURI string) (string, bool) {
	if idx := strings.Index(rawURI, "#"); idx != -1 {
		return rawURI[:idx], true
	}
	return rawURI, true
}

func (identityByFragment) Supported(string) bool { return true }

// identityNeverOK 恒返回 ok=false：模拟 IR 解析失败回退 rawURI 键。
type identityNeverOK struct{}

func (identityNeverOK) Identity(rawURI string) (string, bool) { return rawURI, false }

func (identityNeverOK) Supported(string) bool { return true }

// newMemMgr 构造无库内存实例（纯内存模式）。
func newMemMgr(identity IdentityResolver, hooks Hooks) *Manager {
	return NewManager(nil, identity, hooks, nil)
}

// setupDBMgr 用临时文件初始化真实 SQLite（禁止 mock SQL）并构造绑定该库的实例。
func setupDBMgr(t *testing.T, identity IdentityResolver, hooks Hooks) (*Manager, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes-test.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return NewManager(database, identity, hooks, nil), database
}

// seedUnsafe 直接预置内存态（模拟已装载状态，绕过 DB 装载）。
func (m *Manager) seedUnsafe(nodes []Node, health map[string]*NodeHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodeList = nodes
	if health != nil {
		m.healthMap = health
	}
	m.loaded = true
}

func TestNodesLifecycle(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	n2 := Node{RawURI: "uri2", Name: "node2"} //nolint:exhaustruct

	mgr.MergeNodes([]Node{n1, n2})

	nodes := mgr.LoadNodes()
	if len(nodes) != 2 {
		t.Fatalf("Expected 2 nodes, got %d", len(nodes))
	}

	// Test Dedup
	mgr.MergeNodes([]Node{n1}) // Add duplicate
	if len(mgr.LoadNodes()) != 2 {
		t.Fatalf("Expected 2 nodes after merging duplicate, got %d", len(mgr.LoadNodes()))
	}

	removed := mgr.DedupNodes()
	if removed != 0 {
		t.Errorf("Expected 0 removed during dedup, got %d", removed)
	}

	// Test RecordTest
	mgr.RecordTest("uri1", true, 10.5, "")
	health := mgr.LoadHealth()
	hUri1, ok := health["uri1"]
	if !ok || hUri1.SuccessCount != 1 {
		t.Errorf("Expected success count 1, got %v", hUri1)
	}

	mgr.RecordTest("uri1", false, 0, "timeout")
	hUri1, ok = mgr.LoadHealth()["uri1"]
	if !ok || hUri1.FailCount != 1 {
		t.Errorf("Expected fail count 1, got %v", hUri1)
	}

	// Test BatchUpdateNodesDisabled
	mgr.BatchUpdateNodesDisabled([]string{"uri1"}, true)
	for _, n := range mgr.LoadNodes() {
		if n.RawURI == "uri1" && !n.Disabled {
			t.Errorf("Expected uri1 to be disabled")
		}
	}

	// Test SelectForParallel (uri1 is disabled, should only return uri2 if available)
	selected := mgr.SelectForParallel(2, false)
	if len(selected) != 1 || selected[0].RawURI != "uri2" {
		t.Errorf("Expected only uri2 to be selected, got %v", selected)
	}

	// Test DeleteDisabled
	removed = mgr.DeleteDisabled()
	if removed != 1 {
		t.Errorf("Expected 1 node removed, got %d", removed)
	}
	if len(mgr.LoadNodes()) != 1 {
		t.Errorf("Expected 1 node remaining, got %d", len(mgr.LoadNodes()))
	}

	// Test DeleteNode
	mgr.DeleteNode("uri2")
	if len(mgr.LoadNodes()) != 0 {
		t.Errorf("Expected 0 nodes, got %d", len(mgr.LoadNodes()))
	}
}

func TestDedupIdentityFallback(t *testing.T) {
	n1 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name1", Name: "node1"} //nolint:exhaustruct
	n2 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name2", Name: "node2"} //nolint:exhaustruct

	// nil IdentityResolver：回退 rawURI 键，不同片段（不同 RawURI）不去重
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{n1, n2})
	if removed := mgr.DedupNodes(); removed != 0 {
		t.Errorf("nil resolver: expected 0 removed (rawURI fallback), got %d", removed)
	}

	// 注册解析器：同 server 同凭证不同 RawURI（仅片段不同）→ 同键去重
	mgr = newMemMgr(identityByFragment{}, Hooks{})
	mgr.MergeNodes([]Node{n1, n2})
	if removed := mgr.DedupNodes(); removed != 1 {
		t.Errorf("registered resolver: expected 1 removed, got %d", removed)
	}
	if result := mgr.LoadNodes(); len(result) != 1 {
		t.Errorf("expected 1 node after dedup, got %d", len(result))
	}

	// 解析失败（ok=false）→ 回退 rawURI 键，不去重
	mgr = newMemMgr(identityNeverOK{}, Hooks{})
	mgr.MergeNodes([]Node{n1, n2})
	if removed := mgr.DedupNodes(); removed != 0 {
		t.Errorf("ok=false identity: expected 0 removed (fallback), got %d", removed)
	}
}

func TestMergeNodesPrunesHealthMap(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	n1 := Node{RawURI: "uri1", Name: "node1"} //nolint:exhaustruct
	n2 := Node{RawURI: "uri2", Name: "node2"} //nolint:exhaustruct

	mgr.MergeNodes([]Node{n1, n2})

	mgr.RecordTest("uri1", true, 10, "")
	mgr.RecordTest("uri2", false, 0, "timeout")
	health := mgr.LoadHealth()
	if len(health) != 2 {
		t.Fatalf("Expected 2 health entries, got %d", len(health))
	}

	mgr.DeleteNode("uri2")

	mgr.mu.Lock()
	mgr.healthMap["orphan-uri"] = &NodeHealth{SuccessCount: 99} //nolint:exhaustruct
	mgr.mu.Unlock()

	mgr.MergeNodes([]Node{n1})
	health = mgr.LoadHealth()
	if len(health) != 1 {
		t.Fatalf("Expected 1 health entry after MergeNodes prunes orphan, got %d", len(health))
	}
	if _, exists := health["orphan-uri"]; exists {
		t.Errorf("Expected orphan-uri health entry to be pruned")
	}
	if _, exists := health["uri1"]; !exists {
		t.Errorf("Expected uri1 health entry to survive")
	}

	mgr.RecordTest("uri1", false, 0, "timeout")
	health = mgr.LoadHealth()
	if h, ok := health["uri1"]; !ok || h.FailCount != 1 {
		t.Errorf("Expected RecordTest to still work after pruning, got %v", health["uri1"])
	}
}

func TestDedupNodesSemantic(t *testing.T) {
	// 身份解析器模拟"同身份不同 RawURI"（生产由 transport.NewIdentityResolver 实现）
	mgr := newMemMgr(identityByFragment{}, Hooks{})

	// Two nodes with same identity but different raw URIs (different names/fragments)
	n1 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name1", Name: "node1"}
	n2 := Node{RawURI: "vless://uuid@example.com:443?security=tls#name2", Name: "node2"}
	mgr.MergeNodes([]Node{n1, n2})

	removed := mgr.DedupNodes()
	if removed != 1 {
		t.Errorf("Expected 1 removed during semantic dedup, got %d", removed)
	}
	result := mgr.LoadNodes()
	if len(result) != 1 {
		t.Errorf("Expected 1 node after dedup, got %d", len(result))
	}
}

// ---- 真实 SQLite 持久化回归（补充方案第 7 条）----

func TestNodesPersistenceRoundTrip(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})

	seeds := []Node{
		{RawURI: "vless://a@x.com:443#n1", Type: "vless", Name: "n1", Disabled: false},
		{RawURI: "vmess://b@y.com:8443#n2", Type: "vmess", Name: "n2", Disabled: true},
		{RawURI: "ss://c@z.com:8388#n3", Type: "ss", Name: "n3", Disabled: false},
	}
	mgr.MergeNodes(seeds)

	// 模拟重启：以同一数据库构造全新实例重新加载
	reborn := NewManager(database, nil, Hooks{}, nil)
	got := reborn.LoadNodes()
	if len(got) != len(seeds) {
		t.Fatalf("roundtrip 数量=%d, want %d", len(got), len(seeds))
	}
	for i, want := range seeds {
		n := got[i]
		if n.RawURI != want.RawURI || n.Type != want.Type || n.Name != want.Name || n.Disabled != want.Disabled {
			t.Errorf("roundtrip[%d] 字段不一致: got %+v, want %+v", i, n, want)
		}
	}
}

func TestBatchUpdateNodesDisabledPersistence(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})

	mgr.MergeNodes([]Node{
		{RawURI: "uri1", Name: "node1"},
		{RawURI: "uri2", Name: "node2"},
		{RawURI: "uri3", Name: "node3"},
	})

	// 重复 URI 也应幂等（targets 去重）
	mgr.BatchUpdateNodesDisabled([]string{"uri1", "uri1", "uri3"}, true)

	// 内存一致性
	for _, n := range mgr.LoadNodes() {
		switch n.RawURI {
		case "uri1", "uri3":
			if !n.Disabled {
				t.Errorf("%s 应被禁用", n.RawURI)
			}
		case "uri2":
			if n.Disabled {
				t.Error("uri2 不应被禁用")
			}
		}
	}
	// 数据库一致性：直接查询
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes WHERE disabled = 1").Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 2 {
		t.Errorf("DB disabled 数量=%d, want 2", count)
	}

	// 反向启用
	mgr.BatchUpdateNodesDisabled([]string{"uri1"}, false)
	var d1 int
	if err := database.QueryRow("SELECT disabled FROM nodes WHERE raw_uri = 'uri1'").Scan(&d1); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if d1 != 0 {
		t.Errorf("DB uri1 disabled=%d, want 0", d1)
	}
	for _, n := range mgr.LoadNodes() {
		if n.RawURI == "uri1" && n.Disabled {
			t.Error("内存 uri1 应已启用")
		}
	}
}

func TestBatchUpdateNodesDisabledDBFailure(t *testing.T) {
	// 关闭连接的库句柄 → Begin 失败，内存不得被修改
	d := openBrokenDB(t)
	mgr := NewManager(d, nil, Hooks{}, nil)

	mgr.seedUnsafe([]Node{{RawURI: "uri1", Name: "node1"}}, nil)

	mgr.BatchUpdateNodesDisabled([]string{"uri1"}, true)
	if got := mgr.LoadNodes(); len(got) != 1 || got[0].Disabled {
		t.Errorf("DB 失败时内存不得被修改: %+v", got)
	}
}

func TestLoadNodesReturnsIsolatedCopy(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.MergeNodes([]Node{{RawURI: "uri1", Name: "node1"}, {RawURI: "uri2", Name: "node2"}}) //nolint:exhaustruct
	got := mgr.LoadNodes()
	got[0].Disabled = true
	got[0].RawURI = "mutated"
	// 外部修改拷贝不得影响内部状态
	internal := mgr.LoadNodes()
	if len(internal) != 2 || internal[0].Disabled || internal[0].RawURI != "uri1" {
		t.Errorf("LoadNodes 返回拷贝被外部修改污染内部状态: %+v", internal)
	}
}

func TestGetAllRawURIs(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})

	mgr.MergeNodes([]Node{{RawURI: "uri1", Name: "node1"}, {RawURI: "uri2", Name: "node2"}}) //nolint:exhaustruct
	uris := mgr.AllRawURIs()
	if len(uris) != 2 || uris[0] != "uri1" || uris[1] != "uri2" {
		t.Errorf("AllRawURIs = %v, want [uri1 uri2]", uris)
	}
}

func TestDeleteDisabledDBPersistence(t *testing.T) {
	rec := &hookRecorder{}
	mgr, database := setupDBMgr(t, nil, rec.hooks())

	mgr.MergeNodes([]Node{
		{RawURI: "uri1", Name: "node1"},
		{RawURI: "uri2", Name: "node2", Disabled: true},
		{RawURI: "uri3", Name: "node3", Disabled: true},
	})
	mgr.RecordTest("uri2", true, 10, "")
	mgr.RecordTest("uri3", true, 20, "")
	// 健康异步 worker 未启动，直接向 DB 注入健康行模拟已持久化状态
	for _, u := range []string{"uri2", "uri3"} {
		if err := nodestore.UpsertHealth(database, "node_health", nodestore.HealthRow{RawURI: u, SuccessCount: 1}); err != nil {
			t.Fatalf("UpsertHealth: %v", err)
		}
	}

	removed := mgr.DeleteDisabled()
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	// 内存一致性
	got := mgr.LoadNodes()
	if len(got) != 1 || got[0].RawURI != "uri1" {
		t.Errorf("内存剩余节点=%v, want [uri1]", got)
	}
	if len(mgr.LoadHealth()) != 0 {
		t.Errorf("健康度应全部清理, got %v", mgr.LoadHealth())
	}
	// DB 一致性
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 1 {
		t.Errorf("DB 节点剩余=%d, want 1", count)
	}
	var healthCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM node_health").Scan(&healthCount); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if healthCount != 0 {
		t.Errorf("DB 健康行剩余=%d, want 0", healthCount)
	}
	// 批量回调一次性收到全部 URI
	if len(rec.uris) != 2 || rec.uris[0] != "uri2" || rec.uris[1] != "uri3" {
		t.Errorf("批量回调 = %v, want [uri2 uri3]", rec.uris)
	}
}

func TestBatchDeleteNodesDBPersistence(t *testing.T) {
	rec := &hookRecorder{}
	mgr, database := setupDBMgr(t, nil, rec.hooks())

	mgr.MergeNodes([]Node{
		{RawURI: "uri1", Name: "node1"},
		{RawURI: "uri2", Name: "node2"},
		{RawURI: "uri3", Name: "node3"},
	})
	mgr.RecordTest("uri2", true, 10, "")
	// 健康异步 worker 未启动，直接向 DB 注入健康行模拟已持久化状态
	if err := nodestore.UpsertHealth(database, "node_health", nodestore.HealthRow{RawURI: "uri2", SuccessCount: 1}); err != nil {
		t.Fatalf("UpsertHealth: %v", err)
	}

	mgr.BatchDeleteNodes([]string{"uri1", "uri3"})
	got := mgr.LoadNodes()
	if len(got) != 1 || got[0].RawURI != "uri2" {
		t.Errorf("内存剩余节点=%v, want [uri2]", got)
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 1 {
		t.Errorf("DB 节点剩余=%d, want 1", count)
	}
	var healthCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM node_health").Scan(&healthCount); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if healthCount != 1 {
		t.Errorf("DB 健康行剩余=%d, want 1（uri2 保留）", healthCount)
	}
	if len(rec.uris) != 2 {
		t.Errorf("批量回调收到 %d 个 URI, want 2: %v", len(rec.uris), rec.uris)
	}
	// 空列表幂等
	mgr.BatchDeleteNodes(nil)
}

func TestDedupNodesDBPersistence(t *testing.T) {
	rec := &hookRecorder{}
	mgr, database := setupDBMgr(t, identityByFragment{}, rec.hooks())

	mgr.MergeNodes([]Node{
		{RawURI: "vless://u@x.com:443#name1", Name: "node1"},
		{RawURI: "vless://u@x.com:443#name2", Name: "node2"},
	})
	mgr.RecordTest("vless://u@x.com:443#name1", true, 10, "")

	removed := mgr.DedupNodes()
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}
	got := mgr.LoadNodes()
	if len(got) != 1 || got[0].RawURI != "vless://u@x.com:443#name1" {
		t.Errorf("内存剩余节点=%v", got)
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 1 {
		t.Errorf("DB 节点剩余=%d, want 1", count)
	}
	if len(rec.uris) != 1 || rec.uris[0] != "vless://u@x.com:443#name2" {
		t.Errorf("批量回调 = %v, want [name2 URI]", rec.uris)
	}
}

func TestDeleteDisabledNoDBFallback(t *testing.T) {
	rec := &hookRecorder{}
	// 无库模式：纯内存过滤
	mgr := newMemMgr(nil, rec.hooks())

	mgr.MergeNodes([]Node{
		{RawURI: "uri1", Name: "node1"},
		{RawURI: "uri2", Name: "node2", Disabled: true},
	})
	removed := mgr.DeleteDisabled()
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}
	got := mgr.LoadNodes()
	if len(got) != 1 || got[0].RawURI != "uri1" {
		t.Errorf("无库模式删除后剩余节点=%v, want [uri1]", got)
	}
	if len(rec.uris) != 1 || rec.uris[0] != "uri2" {
		t.Errorf("无库模式批量回调 = %v, want [uri2]", rec.uris)
	}
}

func TestDeleteDisabledDBFailure(t *testing.T) {
	mgr := NewManager(openBrokenDB(t), nil, Hooks{}, nil)

	mgr.seedUnsafe([]Node{{RawURI: "uri1", Name: "node1", Disabled: true}}, nil)

	if removed := mgr.DeleteDisabled(); removed != 0 {
		t.Errorf("DB 失败时 DeleteDisabled 应返回 0, got %d", removed)
	}
	if got := mgr.LoadNodes(); len(got) != 1 || !got[0].Disabled {
		t.Errorf("DB 失败时内存不得被修改: %+v", got)
	}
}

func TestDedupNodesDBFailureKeepsHealth(t *testing.T) {
	mgr := NewManager(openBrokenDB(t), identityByFragment{}, Hooks{}, nil)

	mgr.seedUnsafe(
		[]Node{
			{RawURI: "vless://u@x.com:443#name1", Name: "node1"},
			{RawURI: "vless://u@x.com:443#name2", Name: "node2"},
		},
		map[string]*NodeHealth{
			"vless://u@x.com:443#name2": {SuccessCount: 7}, //nolint:exhaustruct
		},
	)

	if removed := mgr.DedupNodes(); removed != 0 {
		t.Errorf("DB 失败时 DedupNodes 应返回 0, got %d", removed)
	}
	// 节点与健康度必须零变更（去重未发生）
	got := mgr.LoadNodes()
	if len(got) != 2 {
		t.Errorf("DB 失败时节点列表不得被修改, got %d 个", len(got))
	}
	health := mgr.LoadHealth()
	if h, ok := health["vless://u@x.com:443#name2"]; !ok || h.SuccessCount != 7 {
		t.Errorf("DB 失败时健康度不得被清理: %v", health)
	}
}

// openBrokenDB 返回一个已关闭连接的库句柄（触发 Begin 失败路径）。
func openBrokenDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return d
}

// ---- 加载失败 fail-closed 与延迟恢复（P0-6）----

// TestLoadFailure_FailClosedBlocksFullReplace 首装加载失败必须置脏并拒绝全量覆盖保存：
// 否则"空池 + 新导入节点"的 DELETE+INSERT 会把库里的存量节点整表抹掉（原缺陷的丢数据路径）。
func TestLoadFailure_FailClosedBlocksFullReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes-failclosed.db")
	h1, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = h1.Close() })

	seed := NewManager(h1, nil, Hooks{}, nil)
	seed.MergeNodes([]Node{
		{RawURI: "vless://a@x.com:443#na", Type: "vless"}, //nolint:exhaustruct
		{RawURI: "vless://b@x.com:443#nb", Type: "vless"}, //nolint:exhaustruct
	})

	// 同一库文件的第二个句柄构造第二个管理器，关闭句柄模拟加载期 DB 故障
	h2, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open(2): %v", err)
	}
	victim := NewManager(h2, nil, Hooks{}, nil)
	if err := h2.Close(); err != nil {
		t.Fatalf("close h2: %v", err)
	}

	// 加载失败（脏态）后合并新节点：新节点仅存于内存，全量保存必须被拒绝
	victim.MergeNodes([]Node{{RawURI: "vless://c@x.com:443#nc", Type: "vless"}}) //nolint:exhaustruct
	if got := victim.LoadNodes(); len(got) != 1 {
		t.Fatalf("脏态下合并的节点应仅存于内存, got %d", len(got))
	}

	// 库中存量节点必须原封不动（fail-closed 生效，未被"空池 + c"整表覆盖）
	var got []string
	if err := nodestore.LoadNodesFull(h1, "nodes", func(r nodestore.NodeRow) {
		got = append(got, r.RawURI)
	}); err != nil {
		t.Fatalf("LoadNodesFull: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("脏态期间的全量保存必须被拒绝，库中应仍为 2 个存量节点, got %v", got)
	}
}

// TestLoadFailure_DelayedRetryRecoversAndMerges 脏态延迟重试恢复：合并式装载必须保留
// dirty 期间加入内存、库中尚不存在的节点，恢复后补齐落库（不得把内存变更挤掉）。
func TestLoadFailure_DelayedRetryRecoversAndMerges(t *testing.T) {
	mgr, h1 := setupDBMgr(t, nil, Hooks{})
	mgr.MergeNodes([]Node{
		{RawURI: "vless://a@x.com:443#na", Type: "vless"}, //nolint:exhaustruct
		{RawURI: "vless://b@x.com:443#nb", Type: "vless"}, //nolint:exhaustruct
	})

	// 白盒置脏并推迟重试窗口，模拟"首装失败后继续运行"的状态
	mgr.mu.Lock()
	mgr.loadDirty = true
	mgr.loadRetryAfter = time.Now().Add(time.Minute)
	mgr.mu.Unlock()

	// 脏态期间导入 c：保存被拒，仅存内存
	mgr.MergeNodes([]Node{{RawURI: "vless://c@x.com:443#nc", Type: "vless"}}) //nolint:exhaustruct
	var got []string
	if err := nodestore.LoadNodesFull(h1, "nodes", func(r nodestore.NodeRow) {
		got = append(got, r.RawURI)
	}); err != nil {
		t.Fatalf("LoadNodesFull: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("脏态期间的全量保存必须被拒绝, got %v", got)
	}

	// 重试窗口到期后触发延迟重试：合并库中 a,b 与内存 c，解除脏标记并补齐落库
	mgr.mu.Lock()
	mgr.loadRetryAfter = time.Now().Add(-time.Second)
	mgr.mu.Unlock()
	mgr.EnsureLoaded()

	uris := map[string]bool{}
	for _, n := range mgr.LoadNodes() {
		uris[n.RawURI] = true
	}
	for _, want := range []string{"vless://a@x.com:443#na", "vless://b@x.com:443#nb", "vless://c@x.com:443#nc"} {
		if !uris[want] {
			t.Fatalf("恢复后内存应合并库中存量与 dirty 期间节点，缺 %s: %v", want, uris)
		}
	}
	got = got[:0]
	if err := nodestore.LoadNodesFull(h1, "nodes", func(r nodestore.NodeRow) {
		got = append(got, r.RawURI)
	}); err != nil {
		t.Fatalf("LoadNodesFull: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("恢复后应把 dirty 期间节点补齐落库, got %v", got)
	}
}

// TestLoadBackfillsDisabledByHealthWithoutHealthRow 守卫测试：健康禁用标记的回填
// 不得依赖 node_health 行存在。auth 失败禁用（node_error.go）的健康事件走异步队列，
// 队列满即丢弃——重启后若标记丢失，节点既不在 HealthAutoDisabledURIs 也不在批量
// 测速自禁用名单，成为无任何自动恢复通道的事实永久禁用。
func TestLoadBackfillsDisabledByHealthWithoutHealthRow(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-auth", Name: "auth-dead"}}) //nolint:exhaustruct

	// 直接改库：nodes 行 disabled=1 + disabled_by_health=1，但清空 node_health——
	// 模拟"禁用已落库、健康事件在异步队列中丢失"的重启前状态。
	if _, err := database.Exec("UPDATE nodes SET disabled = 1, disabled_by_health = 1 WHERE raw_uri = 'uri-auth'"); err != nil {
		t.Fatalf("更新 nodes 行失败: %v", err)
	}
	if _, err := database.Exec("DELETE FROM node_health"); err != nil {
		t.Fatalf("清空 node_health 失败: %v", err)
	}

	// 新 Manager 重载（模拟重启）
	mgr2 := NewManager(database, nil, Hooks{}, nil)
	mgr2.EnsureLoaded()

	health := mgr2.LoadHealth()
	h, ok := health["uri-auth"]
	if !ok {
		t.Fatal("重启后应有无健康行的最小健康条目（携带健康禁用标记）")
	}
	if !h.DisabledByHealth {
		t.Error("DisabledByHealth 标记应被回填，否则该节点失去自动恢复通道")
	}
	if !mgr2.HealthAutoDisabledURIs()["uri-auth"] {
		t.Error("uri-auth 应出现在 HealthAutoDisabledURIs，供自愈/批量测速恢复")
	}
}

// TestUpsertHealthRow_SkipsOrphanURI 守卫：事件入队后节点被删除，worker 消费时
// 不得把已删节点的健康行复活成孤儿（UpsertHealth 是 INSERT OR REPLACE，SQLite
// 未启用外键、DELETE 不级联，此前无存活校验会复活孤儿行）。
func TestUpsertHealthRow_SkipsOrphanURI(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})
	mgr.StartHealthWorker()
	mgr.MergeNodes([]Node{{RawURI: "uri-del", Name: "del"}}) //nolint:exhaustruct
	mgr.RecordTest("uri-del", false, 10, "timeout")          // 异步入队一条健康事件
	mgr.DeleteNode("uri-del")                                // 消费前删除节点

	mgr.StopHealthWorker() // 排空队列（含那条已孤儿化的事件）

	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM node_health WHERE raw_uri = 'uri-del'").Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 0 {
		t.Errorf("孤儿健康行不应复活, got %d 行", count)
	}
}

// TestStopHealthWorkerDrainsPending 验证停止语义：Stop 前入队的最后一批事件被
// 落库后排空退出（优雅关闭不丢最后一批健康记账），且 Stop 幂等。
func TestStopHealthWorkerDrainsPending(t *testing.T) {
	mgr, database := setupDBMgr(t, nil, Hooks{})
	mgr.StartHealthWorker()
	mgr.MergeNodes([]Node{{RawURI: "uri-1", Name: "n1"}}) //nolint:exhaustruct
	mgr.RecordTest("uri-1", true, 12.5, "")               // 异步入队
	mgr.StopHealthWorker()                                // 排空后退出

	var ms float64
	if err := database.QueryRow("SELECT last_test_ms FROM node_health WHERE raw_uri = 'uri-1'").Scan(&ms); err != nil {
		t.Fatalf("排空后健康行应已落库: %v", err)
	}
	if ms != 12.5 {
		t.Errorf("last_test_ms=%v, want 12.5", ms)
	}
	mgr.StopHealthWorker() // 幂等：重复 Stop 安全
}

// TestDecInFlight_ClampedAtZero 守卫：Inc 与 Dec 之间条目被删除重建时，旧候选的
// Dec 命中新条目不得把 InFlight 减成负值（负值会让该节点在 inFlight 升序选点中
// 永远排最前，形成热点）。
func TestDecInFlight_ClampedAtZero(t *testing.T) {
	mgr := newMemMgr(nil, Hooks{})
	mgr.MergeNodes([]Node{{RawURI: "uri-x", Name: "x"}}) //nolint:exhaustruct
	mgr.IncInFlight("uri-x")
	// 模拟删除重建：healthMap 条目换新（Inc 建立的旧条目被丢弃）
	mgr.mu.Lock()
	mgr.healthMap["uri-x"] = &NodeHealth{} //nolint:exhaustruct
	mgr.mu.Unlock()

	mgr.DecInFlight("uri-x") // 旧候选的 Dec：新条目 InFlight=0，不得减成 -1
	mgr.DecInFlight("uri-x")

	health := mgr.LoadHealth()
	h, ok := health["uri-x"]
	if !ok {
		t.Fatal("应有健康条目")
	}
	if h.InFlight != 0 {
		t.Errorf("InFlight 应钳制在 0, got %d", h.InFlight)
	}
}
