package api

import (
	"log"
	"net/http"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

type nodeView struct {
	exitpool.Node
	Supported bool `json:"supported"`
}

func (adm *AdminHandler) adminGetNodes(w http.ResponseWriter, _ *http.Request) {
	list := adm.deps.Exit.LoadNodes()
	var enabledCount, disabledCount int
	uris := make([]string, 0, len(list))
	for _, n := range list {
		if n.Disabled {
			disabledCount++
		} else {
			enabledCount++
		}
		uris = append(uris, n.RawURI)
	}
	supportedMap := adm.deps.IR.CheckSupportedBatch(uris)
	views := make([]nodeView, 0, len(list))
	for _, n := range list {
		views = append(views, nodeView{Node: n, Supported: supportedMap[n.RawURI]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":  views,
		"health": adm.deps.Exit.LoadHealth(),
		// rt_rank 是各节点当前的 rT 降权层（0 健康/1 差/2 冷却中，与 rtRankOf 同判据）：
		// 前端筛选与排序直接消费该权威值，不再自行用 rt_last_ms 等字段重算
		//（此前自算漏了 rt_hang_streak，会把"挂住后恢复"的节点误判为正常）。
		"rt_rank": adm.deps.Exit.RTRanks(),
		// global_fault 是"疑似公共依赖/环境故障"的实时状态（类别 → 窗口内节点数）：
		// 命中期间该类别暂停自动禁用，面板据此提示"这批失败可能不是节点的问题"。
		"global_fault":   adm.deps.Exit.GlobalFaultActive(),
		"total":          len(list),
		"enabled_count":  enabledCount,
		"disabled_count": disabledCount,
	})
}

func (adm *AdminHandler) adminEnableNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	ok := adm.deps.Exit.EnableNode(body.RawURI)
	log.Printf("[Admin] [EnableNode] 启用节点 %s: %v", adm.deps.Exit.NodeName(body.RawURI), ok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}

func (adm *AdminHandler) adminDedupNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_count": adm.deps.Exit.DedupNodes()})
}

func (adm *AdminHandler) adminDeleteDisabledNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_count": adm.deps.Exit.DeleteDisabled()})
}

func (adm *AdminHandler) adminUseNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.RawURI == "" {
		_ = config.WriteSettings(map[string]any{"active_node_uri": "", "parallel_pool_enabled": true})
	} else {
		_ = config.WriteSettings(map[string]any{"active_node_uri": body.RawURI, "parallel_pool_enabled": false})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminSortNodesByLatency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Desc bool `json:"desc"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Desc {
		adm.deps.Exit.SortNodesByLatencyDesc()
	} else {
		adm.deps.Exit.SortNodesByLatency()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminDeleteNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	adm.deps.Exit.DeleteNode(body.RawURI)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminBatchDisableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDisable] 批量禁用 %d 个节点", len(body.URIs))
	adm.deps.Exit.BatchUpdateNodesDisabled(body.URIs, true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminBatchEnableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchEnable] 批量启用 %d 个节点", len(body.URIs))
	adm.deps.Exit.BatchUpdateNodesDisabled(body.URIs, false)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminBatchDeleteNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDelete] 批量删除 %d 个节点", len(body.URIs))
	adm.deps.Exit.BatchDeleteNodes(body.URIs)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminLockNodes 批量设置/解除出口节点的防删锁。
// body: {uris:[...] 或 raw_uri:"...", locked:bool}。
// 上锁后该节点不会被删除、去重移除或自动/批量禁用。
func (adm *AdminHandler) adminLockNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs   []string `json:"uris"`
		RawURI string   `json:"raw_uri"`
		Locked bool     `json:"locked"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	uris := body.URIs
	if len(uris) == 0 && body.RawURI != "" {
		uris = []string{body.RawURI}
	}
	if len(uris) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("uris 不能为空"))
		return
	}
	adm.deps.Exit.BatchUpdateNodesLocked(uris, body.Locked)
	log.Printf("[Admin] [LockNodes] 防删锁=%v, 节点数=%d", body.Locked, len(uris))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "locked": body.Locked, "count": len(uris)})
}

// adminLockHealthyNodes 一键防删：只给"确实成功过"的出口节点上锁。
// 判定口径：未禁用、未锁定、最近一次测试通过且无连续失败，并且 usage_log 中存在
// 该节点的请求级成功记录——最后一条是关键：仅凭测试通过会把从未成功过的节点也保护起来。
func (adm *AdminHandler) adminLockHealthyNodes(w http.ResponseWriter, _ *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	now := time.Now()
	from := now.Unix() - int64(stats.RetentionDays())*86400
	if from < 0 {
		from = 0
	}
	rows, err := store.ByNode(from, now.Unix()+1)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("节点统计查询失败 (by_node): "+err.Error()))
		return
	}
	successByURI := make(map[string]int, len(rows))
	for i := range rows {
		key := adm.nodeStatKey(rows[i].Name, rows[i].RawURI)
		if key != "" {
			successByURI[key] += rows[i].Success
		}
	}

	health := adm.deps.Exit.LoadHealth()
	var uris []string
	for _, n := range adm.deps.Exit.LoadNodes() {
		if n.Disabled || n.Locked {
			continue
		}
		h, ok := health[n.RawURI]
		if !ok || h.LastSuccessAt == 0 || h.ConsecutiveFailures > 0 {
			continue
		}
		if successByURI[n.RawURI] <= 0 {
			continue
		}
		uris = append(uris, n.RawURI)
	}
	if len(uris) > 0 {
		adm.deps.Exit.BatchUpdateNodesLocked(uris, true)
	}
	log.Printf("[Admin] [LockHealthyNodes] 一键防删: 锁定 %d 个成功过的节点", len(uris))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(uris)})
}

// adminUnlockAllNodes 一键解除全部出口节点的防删锁，返回解锁数量。
func (adm *AdminHandler) adminUnlockAllNodes(w http.ResponseWriter, _ *http.Request) {
	var uris []string
	for _, n := range adm.deps.Exit.LoadNodes() {
		if n.Locked {
			uris = append(uris, n.RawURI)
		}
	}
	if len(uris) > 0 {
		adm.deps.Exit.BatchUpdateNodesLocked(uris, false)
	}
	log.Printf("[Admin] [UnlockAllNodes] 一键解除防删: %d 个节点", len(uris))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(uris)})
}
