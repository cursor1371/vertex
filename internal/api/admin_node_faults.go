package api

import (
	"log"
	"net/http"
	"strconv"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// 故障记录接口（管理面板「故障统计」）：评审要求故障分类统计里"其他"不能只显示一个数量——
// 展开后必须能拿到完整错误码、HTTP 状态码、脱敏原文、首次/最近发生时间、节点与发生次数，
// 并支持一键复制，便于把完整信息反馈出去。原文本就在入库前脱敏（nodestore.MaskSecrets），
// 因此这里返回的 raw_error 可以安全展示。
//
// 保留期固定 1 天（db.FaultLogRetentionDays），由 stats 的周期清理与一键维护负责删除。

// nodeFaultsResponse 是 GET /api/admin/node-faults 的响应。
type nodeFaultsResponse struct {
	Classes []faultClassView `json:"classes"`
	Rows    []faultRowView   `json:"rows"`
	// Total 是本次**返回**的行数（= len(rows)）；真实总量见 TotalRows / TotalCount。
	Total int `json:"total"`
	// TotalRows / TotalCount 是全表真实总量（SQL 聚合，不受列表 limit 影响）：
	// 面板必须用它们显示"共多少"，否则数据超过 limit 时用户会把"最近 N 条"当成全部历史。
	TotalRows  int64 `json:"total_rows"`
	TotalCount int64 `json:"total_count"`
	// Truncated 表示列表被 limit 截断（TotalRows > Total）。
	Truncated bool `json:"truncated"`
	// Limit 是本次查询生效的行数上限。
	Limit int `json:"limit"`
	// RetentionDays 让面板明确告知"这些记录只保留 1 天"，避免用户以为历史被吞了。
	RetentionDays int `json:"retention_days"`
}

type faultClassView struct {
	Class      string `json:"class"`
	Count      int64  `json:"count"`
	Rows       int64  `json:"rows"`
	Nodes      int64  `json:"nodes"`
	LastSeenAt int64  `json:"last_seen_at"`
}

type faultRowView struct {
	ID           int64  `json:"id"`
	Scope        string `json:"scope"`
	NodeURI      string `json:"node_uri"`
	NodeName     string `json:"node_name"`
	FailureClass string `json:"failure_class"`
	HTTPStatus   int    `json:"http_status"`
	ErrorCode    string `json:"error_code"`
	RawError     string `json:"raw_error"`
	Target       string `json:"target"`
	Source       string `json:"source"`
	FirstSeenAt  int64  `json:"first_seen_at"`
	LastSeenAt   int64  `json:"last_seen_at"`
	Count        int64  `json:"count"`
}

func (adm *AdminHandler) adminGetNodeFaults(w http.ResponseWriter, r *http.Request) {
	if adm.deps.Exit == nil {
		writeJSON(w, http.StatusOK, nodeFaultsResponse{RetentionDays: db.FaultLogRetentionDays})
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	stats, err := adm.deps.Exit.FaultClassStats()
	if err != nil {
		log.Printf("[Admin] [NodeFaults] 分类统计失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, adminErr("读取故障统计失败: "+err.Error()))
		return
	}
	rows, err := adm.deps.Exit.NodeFaults(limit)
	if err != nil {
		log.Printf("[Admin] [NodeFaults] 明细读取失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, adminErr("读取故障明细失败: "+err.Error()))
		return
	}

	effLimit := limit
	if effLimit <= 0 {
		effLimit = nodestore.FaultLogDefaultLimit
	}
	totalRows, totalCount, err := adm.deps.Exit.FaultTotals()
	if err != nil {
		// 总量查不出来不该让整个页面失败：退回"以返回集为准"，但明确不声称是全部。
		log.Printf("[Admin] [NodeFaults] 全量总量统计失败（按返回集显示）: %v", err)
		totalRows, totalCount = int64(len(rows)), 0
	}
	resp := nodeFaultsResponse{
		Classes:       make([]faultClassView, 0, len(stats)),
		Rows:          make([]faultRowView, 0, len(rows)),
		Total:         len(rows),
		TotalRows:     totalRows,
		TotalCount:    totalCount,
		Truncated:     totalRows > int64(len(rows)),
		Limit:         effLimit,
		RetentionDays: db.FaultLogRetentionDays,
	}
	for _, s := range stats {
		resp.Classes = append(resp.Classes, faultClassView{
			Class: s.Class, Count: s.Count, Rows: s.Rows, Nodes: s.Nodes, LastSeenAt: s.LastSeenAt,
		})
	}
	for _, f := range rows {
		resp.Rows = append(resp.Rows, faultRowView{
			ID: f.ID, Scope: f.Scope, NodeURI: f.NodeURI, NodeName: f.NodeName,
			FailureClass: f.FailureClass, HTTPStatus: f.HTTPStatus, ErrorCode: f.ErrorCode,
			RawError: f.RawError, Target: f.Target, Source: f.Source,
			FirstSeenAt: f.FirstSeenAt, LastSeenAt: f.LastSeenAt, Count: f.Count,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (adm *AdminHandler) adminClearNodeFaults(w http.ResponseWriter, _ *http.Request) {
	if adm.deps.Exit == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": 0})
		return
	}
	n, err := adm.deps.Exit.ClearFaults()
	if err != nil {
		log.Printf("[Admin] [NodeFaults] 清空失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, adminErr("清空故障记录失败: "+err.Error()))
		return
	}
	log.Printf("[Admin] [NodeFaults] 已清空故障记录 %d 条", n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}
