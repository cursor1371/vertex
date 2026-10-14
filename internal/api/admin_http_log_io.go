package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/jsonx"
)

// adminExportHTTPLog 导出 http_log 记录（JSON/CSV 下载）。
// query 参数：format（json 默认 / csv）、days（默认按保留窗口）、success、request_id、scope、limit。
func (adm *AdminHandler) adminExportHTTPLog(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireHTTPLogStore(w)
	if !ok {
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}
	days := int64(0)
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			days = n
		}
	}
	limit := 100000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100000 {
			limit = n
		}
	}
	from := int64(0)
	if days > 0 {
		from = time.Now().Unix() - days*86400
	}
	filter := httpLogQueryFilter(r)
	recs, err := store.ExportAll(from, filter, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("http-log 导出失败 (export): "+err.Error()))
		return
	}

	switch format {
	case "csv":
		adminExportHTTPLogCSV(w, recs)
	default:
		adminExportHTTPLogJSON(w, recs)
	}
}

func adminExportHTTPLogJSON(w http.ResponseWriter, recs []db.HTTPLogRecord) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="http-log-`+time.Now().Format("20060102-150405")+`.json"`)
	items := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		items = append(items, httpLogToMap(rec))
	}
	data, err := jsonx.Marshal(map[string]any{"exported_at": time.Now().Unix(), "count": len(items), "items": items})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func adminExportHTTPLogCSV(w http.ResponseWriter, recs []db.HTTPLogRecord) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="http-log-`+time.Now().Format("20060102-150405")+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "ts", "time", "request_id", "scope", "node_uri", "node_name", "peer_ip",
		"tcp_reused", "http_version", "http_status_code", "http_result", "error",
		"response_code", "response_result", "response_error_type", "response_error_message",
		"elapsed_ms", "final_url"})
	for _, rec := range recs {
		httpResult := "0"
		if rec.Success {
			httpResult = "1"
		}
		_ = cw.Write([]string{
			strconv.FormatInt(rec.ID, 10),
			strconv.FormatInt(rec.TS, 10),
			time.Unix(rec.TS, 0).Format("2006-01-02 15:04:05"),
			rec.RequestID, rec.Scope, rec.NodeURI, rec.NodeName, rec.PeerIP,
			boolStr(rec.TCPReused), rec.HTTPVersion,
			strconv.Itoa(rec.StatusCode), httpResult, rec.Error,
			strconv.Itoa(rec.ResponseCode), rec.ResponseResult, rec.ResponseErrorType, rec.ResponseErrorMessage,
			strconv.FormatInt(rec.ElapsedMs, 10), rec.FinalURL,
		})
	}
	cw.Flush()
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// adminImportHTTPLog 导入 http_log JSON（用于合并分析）。按 request_id 幂等跳过已存在记录。
// 请求体：{"items":[ {record 字段...}, ... ]} 或直接 [ {record...} ]。
func (adm *AdminHandler) adminImportHTTPLog(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireHTTPLogStore(w)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("读取导入内容失败 (read body): "+err.Error()))
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("导入内容为空 (empty body)"))
		return
	}

	// 兼容 {items:[...]} 与裸数组两种包装。
	items, err := httpLogImportItems(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("导入 JSON 解析失败 (invalid JSON): "+err.Error()))
		return
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("导入内容为空 (no items)"))
		return
	}
	if len(items) > 5000 {
		writeJSON(w, http.StatusBadRequest, adminErr("单次导入上限 5000 条 (too many items)"))
		return
	}

	// 幂等：跳过已存在的 request_id（收集现有集合一次）。
	existing, err := store.ExistingRequestIDs()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("http-log 导入失败 (existing check): "+err.Error()))
		return
	}
	inserted := 0
	skipped := 0
	batch := make([]db.HTTPLogRecord, 0, len(items))
	for _, rec := range items {
		if rec.RequestID == "" {
			rec.RequestID = fmt.Sprintf("import-%d", time.Now().UnixNano())
		}
		if _, dup := existing[rec.RequestID]; dup {
			skipped++
			continue
		}
		if rec.TS == 0 {
			rec.TS = time.Now().Unix()
		}
		batch = append(batch, rec)
		inserted++
	}
	if len(batch) > 0 {
		if err := store.InsertBatch(batch); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("http-log 导入失败 (insert): "+err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inserted": inserted, "skipped": skipped})
}

// httpLogImportItems 解析导入 JSON（兼容 {items:[...]} 与裸数组）。
func httpLogImportItems(body []byte) ([]db.HTTPLogRecord, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var arr []db.HTTPLogRecord
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var wrapper struct {
		Items []db.HTTPLogRecord `json:"items"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Items, nil
}

// httpLogQueryFilter 从请求 query 构造 HTTP 日志过滤条件（导出/列表共用）。
func httpLogQueryFilter(r *http.Request) db.HTTPLogFilter {
	filter := db.HTTPLogFilter{
		RequestID: r.URL.Query().Get("request_id"),
		Scope:     r.URL.Query().Get("scope"),
		NodeName:  r.URL.Query().Get("node"),
		PeerIP:    r.URL.Query().Get("peer_ip"),
		ErrorType: r.URL.Query().Get("error_type"),
	}
	if v := r.URL.Query().Get("success"); v == "1" {
		b := true
		filter.Success = &b
	} else if v == "0" {
		b := false
		filter.Success = &b
	}
	if v := r.URL.Query().Get("http_status"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.HTTPStatus = n
		}
	}
	if v := r.URL.Query().Get("response_code"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.ResponseCode = n
		}
	}
	if v := r.URL.Query().Get("from"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.FromTS = n
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.ToTS = n
		}
	}
	return filter
}
