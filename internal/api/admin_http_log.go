package api

import (
	"net/http"
	"strconv"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// adminGetHTTPLog 返回 http_log 分页列表（时间倒序），支持按成功/失败、request_id、scope 筛选。
// query 参数：page（默认 1）、limit（默认 20，上限 200）、success（1/0）、request_id、scope。
func (adm *AdminHandler) adminGetHTTPLog(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireHTTPLogStore(w)
	if !ok {
		return
	}
	page := 1
	limit := 20
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	filter := httpLogQueryFilter(r)

	pg, err := store.History(page, limit, filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("http-log 查询失败 (history): "+err.Error()))
		return
	}
	items := make([]map[string]any, 0, len(pg.Items))
	for _, it := range pg.Items {
		items = append(items, httpLogToMap(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": pg.Total,
		"page":  pg.Page,
		"limit": pg.Limit,
	})
}

// adminResetHTTPLog 一键清空 http_log 全部记录。
func (adm *AdminHandler) adminResetHTTPLog(w http.ResponseWriter, _ *http.Request) {
	store, ok := adm.requireHTTPLogStore(w)
	if !ok {
		return
	}
	n, err := store.ResetAll()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("清空 http-log 失败 (reset failed): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// adminGetHTTPLogDetail 返回单条 http-log 完整详情（含 URL/headers/body）。
func (adm *AdminHandler) adminGetHTTPLogDetail(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireHTTPLogStore(w)
	if !ok {
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("缺少有效 id 参数 (missing id)"))
		return
	}
	rec, found, err := store.ByID(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("http-log 查询失败 (by_id): "+err.Error()))
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, adminErr("http-log 记录不存在 (not found)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": httpLogToMap(rec)})
}

// requireHTTPLogStore 断言 http-log 仓储可用；不可用时写入 500 并返回 false。
func (adm *AdminHandler) requireHTTPLogStore(w http.ResponseWriter) (*db.HTTPLogStore, bool) {
	if adm.deps.HTTPLog == nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("http-log 仓储未初始化 (http_log store unavailable)"))
		return nil, false
	}
	return adm.deps.HTTPLog, true
}

// httpLogToMap 将一条 http-log 记录映射为前端 JSON。
func httpLogToMap(rec db.HTTPLogRecord) map[string]any {
	return map[string]any{
		"id":                     rec.ID,
		"ts":                     rec.TS,
		"request_id":             rec.RequestID,
		"scope":                  rec.Scope,
		"node_uri":               rec.NodeURI,
		"node_name":              rec.NodeName,
		"peer_ip":                rec.PeerIP,
		"tcp_reused":             rec.TCPReused,
		"http_version":           rec.HTTPVersion,
		"final_url":              rec.FinalURL,
		"request_headers":        rec.RequestHeaders,
		"request_body":           rec.RequestBody,
		"response_headers":       rec.ResponseHeaders,
		"response_body":          rec.ResponseBody,
		"status_code":            rec.StatusCode,
		"success":                rec.Success,
		"error":                  rec.Error,
		"response_code":          rec.ResponseCode,
		"response_result":        rec.ResponseResult,
		"response_error_type":    rec.ResponseErrorType,
		"response_error_message": rec.ResponseErrorMessage,
		"elapsed_ms":             rec.ElapsedMs,
	}
}
