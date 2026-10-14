package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// HTTPLogRecord 是一次上游 HTTP 请求的完整日志，对应 http_log 表的一行。
// 各文本字段（URL/headers/body）为完整原样存储，不截断。
// JSON tag 采用 snake_case，与导出文件/前端字段一致，便于导入回读。
type HTTPLogRecord struct {
	ID              int64  `json:"id"`
	TS              int64  `json:"ts"`
	RequestID       string `json:"request_id"`
	Scope           string `json:"scope"`
	NodeURI         string `json:"node_uri"`
	NodeName        string `json:"node_name"`
	PeerIP          string `json:"peer_ip"`
	TCPReused       bool   `json:"tcp_reused"`
	HTTPVersion     string `json:"http_version"`
	FinalURL        string `json:"final_url"`
	RequestHeaders  string `json:"request_headers"`
	RequestBody     string `json:"request_body"`
	ResponseHeaders string `json:"response_headers"`
	ResponseBody    string `json:"response_body"`
	StatusCode      int    `json:"status_code"` // 传输层 HTTP 状态码
	Success         bool   `json:"success"`     // 传输层 HTTP 结果（2xx/3xx 且完整读尽）
	Error           string `json:"error"`
	// 业务响应状态：与传输层 HTTP 状态区分（如 Gemini error.code / finishReason）。
	ResponseCode   int    `json:"response_code"`
	ResponseResult string `json:"response_result"`
	// 失败详情：batchGraphql errors[].errorType 与 message（流式/聚合失败时填充）。
	ResponseErrorType    string `json:"response_error_type"`
	ResponseErrorMessage string `json:"response_error_message"`
	ElapsedMs            int64  `json:"elapsed_ms"`
}

// HTTPLogFilter 是 http_log 分页查询的可选过滤条件（零值字段不过滤）。
type HTTPLogFilter struct {
	RequestID string
	// Success 用 *bool 表达"不筛选 / 只看成功 / 只看失败"。
	Success *bool
	Scope   string
	// NodeName 按节点名/URI 模糊匹配（node_name 或 node_uri LIKE）。
	NodeName string
	// PeerIP 按对端 IP 模糊匹配。
	PeerIP string
	// HTTPStatus 按传输层 HTTP 状态码精确过滤（>0 生效）。
	HTTPStatus int
	// ResponseCode 按业务响应码精确过滤（>0 生效）。
	ResponseCode int
	// ErrorType 按失败类型模糊匹配（response_error_type LIKE）。
	ErrorType string
	// FromTS / ToTS 时间范围（Unix 秒，半开区间 [FromTS, ToTS)）。
	FromTS int64
	ToTS   int64
}

// HTTPLogHistoryPage 是分页响应的数据载体。
type HTTPLogHistoryPage struct {
	Items []HTTPLogRecord
	Total int64
	Page  int
	Limit int
}

// HTTPLogStore 是 http_log 表的读写仓储。由 db.Open 建立的单连接 SQLite 实例驱动，
// 不持有包级状态，线程安全由底层 *sql.DB 保证。
type HTTPLogStore struct {
	db *sql.DB
}

// NewHTTPLogStore 构造 HTTPLogStore。
func NewHTTPLogStore(database *sql.DB) *HTTPLogStore {
	return &HTTPLogStore{db: database}
}

// NewMemoryHTTPLogStore 构造内存态 HTTPLogStore（sqlite :memory:），供测试使用。
func NewMemoryHTTPLogStore() (*HTTPLogStore, error) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	if err := createTables(database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &HTTPLogStore{db: database}, nil
}

// httpLogInsertSQL 是单条写入语句（InsertBatch 使用），列清单唯一来源。
const httpLogInsertSQL = `
	INSERT INTO http_log (ts, request_id, scope, node_uri, node_name, peer_ip, tcp_reused,
		http_version, final_url, request_headers, request_body, response_headers, response_body,
		status_code, success, error, response_code, response_result,
		response_error_type, response_error_message, elapsed_ms)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// httpLogArgs 将记录展开为与 httpLogInsertSQL 占位符顺序一致的绑定参数。
func httpLogArgs(rec HTTPLogRecord) []any {
	reused := 0
	if rec.TCPReused {
		reused = 1
	}
	success := 0
	if rec.Success {
		success = 1
	}
	return []any{
		rec.TS, rec.RequestID, rec.Scope, rec.NodeURI, rec.NodeName, rec.PeerIP, reused,
		rec.HTTPVersion, rec.FinalURL, rec.RequestHeaders, rec.RequestBody,
		rec.ResponseHeaders, rec.ResponseBody,
		rec.StatusCode, success, rec.Error, rec.ResponseCode, rec.ResponseResult,
		rec.ResponseErrorType, rec.ResponseErrorMessage, rec.ElapsedMs,
	}
}

// Close 关闭底层数据库连接（仅对 NewMemoryHTTPLogStore 创建的实例有意义）。
func (s *HTTPLogStore) Close() error {
	if s != nil && s.db != nil {
		return s.db.Close()
	}
	return nil
}

// InsertBatch 在单事务内批量写入 HTTP 日志。
func (s *HTTPLogStore) InsertBatch(recs []HTTPLogRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	stmt, err := tx.Prepare(httpLogInsertSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("error: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, rec := range recs {
		if _, err = stmt.Exec(httpLogArgs(rec)...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("error: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

// httpLogColumns 是 http_log 全列清单（SELECT/scan 共用，避免列漂移）。
const httpLogColumns = `id, ts, request_id, scope, node_uri, node_name, peer_ip, tcp_reused,
	http_version, final_url, request_headers, request_body, response_headers, response_body,
	status_code, success, error, response_code, response_result,
	response_error_type, response_error_message, elapsed_ms`

// httpLogScan 将当前行扫描进 rec（列顺序须与 httpLogColumns 一致）。
func httpLogScan(scan func(dest ...any) error, rec *HTTPLogRecord) error {
	var reused, success int
	if err := scan(&rec.ID, &rec.TS, &rec.RequestID, &rec.Scope, &rec.NodeURI,
		&rec.NodeName, &rec.PeerIP, &reused, &rec.HTTPVersion, &rec.FinalURL,
		&rec.RequestHeaders, &rec.RequestBody, &rec.ResponseHeaders, &rec.ResponseBody,
		&rec.StatusCode, &success, &rec.Error, &rec.ResponseCode, &rec.ResponseResult,
		&rec.ResponseErrorType, &rec.ResponseErrorMessage, &rec.ElapsedMs); err != nil {
		return err
	}
	rec.TCPReused = reused != 0
	rec.Success = success != 0
	return nil
}

// History 返回按时间倒序分页的 HTTP 日志；page 从 1 开始，limit 每页条数（默认 20，上限 200）。
func (s *HTTPLogStore) History(page, limit int, filter HTTPLogFilter) (HTTPLogHistoryPage, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	where, args := httpLogWhere(filter)
	var total int64
	countQ := "SELECT COUNT(*) FROM http_log" + where
	if err := s.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return HTTPLogHistoryPage{}, fmt.Errorf("error: %w", err)
	}

	offset := (page - 1) * limit
	q := `SELECT ` + httpLogColumns + ` FROM http_log` + where + ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query(q, queryArgs...)
	if err != nil {
		return HTTPLogHistoryPage{}, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HTTPLogRecord
	for rows.Next() {
		var rec HTTPLogRecord
		if err := httpLogScan(rows.Scan, &rec); err != nil {
			return HTTPLogHistoryPage{}, fmt.Errorf("error: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return HTTPLogHistoryPage{}, fmt.Errorf("error: %w", err)
	}
	return HTTPLogHistoryPage{Items: out, Total: total, Page: page, Limit: limit}, nil
}

// httpLogWhere 根据过滤条件构造 WHERE 子句（无过滤时返回空串）。
func httpLogWhere(filter HTTPLogFilter) (string, []any) {
	conds := []string{}
	args := []any{}
	if filter.RequestID != "" {
		// 模糊匹配：前端可能按截断的前缀/片段筛选，精确等值会匹配不上完整 ID。
		conds = append(conds, "request_id LIKE ?")
		args = append(args, "%"+filter.RequestID+"%")
	}
	if filter.Success != nil {
		conds = append(conds, "success = ?")
		if *filter.Success {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if filter.Scope != "" {
		conds = append(conds, "scope = ?")
		args = append(args, filter.Scope)
	}
	if filter.NodeName != "" {
		conds = append(conds, "(node_name LIKE ? OR node_uri LIKE ?)")
		pat := "%" + filter.NodeName + "%"
		args = append(args, pat, pat)
	}
	if filter.PeerIP != "" {
		conds = append(conds, "peer_ip LIKE ?")
		args = append(args, "%"+filter.PeerIP+"%")
	}
	if filter.HTTPStatus > 0 {
		conds = append(conds, "status_code = ?")
		args = append(args, filter.HTTPStatus)
	}
	if filter.ResponseCode > 0 {
		conds = append(conds, "response_code = ?")
		args = append(args, filter.ResponseCode)
	}
	if filter.ErrorType != "" {
		conds = append(conds, "response_error_type LIKE ?")
		args = append(args, "%"+filter.ErrorType+"%")
	}
	if filter.FromTS > 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, filter.FromTS)
	}
	if filter.ToTS > 0 {
		conds = append(conds, "ts < ?")
		args = append(args, filter.ToTS)
	}
	if len(conds) == 0 {
		return "", nil
	}
	where := " WHERE " + conds[0]
	for i := 1; i < len(conds); i++ {
		where += " AND " + conds[i]
	}
	return where, args
}

// ByID 按 id 返回单条 HTTP 日志；不存在时返回 (zero, false)。
func (s *HTTPLogStore) ByID(id int64) (HTTPLogRecord, bool, error) {
	var rec HTTPLogRecord
	q := `SELECT ` + httpLogColumns + ` FROM http_log WHERE id = ?`
	err := httpLogScan(s.db.QueryRow(q, id).Scan, &rec)
	if err == sql.ErrNoRows {
		return HTTPLogRecord{}, false, nil
	}
	if err != nil {
		return HTTPLogRecord{}, false, fmt.Errorf("error: %w", err)
	}
	return rec, true, nil
}

// ResetOlderThan 删除 ts < cutoff 的 HTTP 日志，返回删除行数。
func (s *HTTPLogStore) ResetOlderThan(cutoff int64) (int64, error) {
	res, err := s.db.Exec("DELETE FROM http_log WHERE ts < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return n, nil
}

// ExportAll 返回 ts >= from 且满足 filter 的全部 HTTP 日志（时间升序，供导出分析）。
// from 为 0 时导出全量（受保留窗口约束）。limit 上限 100000。
func (s *HTTPLogStore) ExportAll(from int64, filter HTTPLogFilter, limit int) ([]HTTPLogRecord, error) {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	// httpLogWhere 返回带 " WHERE " 前缀或空串；剥离前缀后与 ts 条件用 AND 连接。
	where, args := httpLogWhere(filter)
	where = strings.TrimPrefix(where, " WHERE ")
	if where != "" {
		where = "ts >= ? AND " + where
	} else {
		where = "ts >= ?"
	}
	q := `SELECT ` + httpLogColumns + ` FROM http_log WHERE ` + where + ` ORDER BY ts ASC, id ASC LIMIT ?`
	queryArgs := []any{from}
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, limit)
	return s.exportQuery(q, queryArgs)
}

func (s *HTTPLogStore) exportQuery(q string, queryArgs []any) ([]HTTPLogRecord, error) {
	rows, err := s.db.Query(q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HTTPLogRecord
	for rows.Next() {
		var rec HTTPLogRecord
		if err := httpLogScan(rows.Scan, &rec); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// ExistingRequestIDs 返回 http_log 中已有的全部 request_id 集合（供导入幂等去重）。
func (s *HTTPLogStore) ExistingRequestIDs() (map[string]struct{}, error) {
	rows, err := s.db.Query("SELECT request_id FROM http_log WHERE request_id != ''")
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// ResetAll 清空 http_log 全部记录，返回删除行数。
func (s *HTTPLogStore) ResetAll() (int64, error) {
	res, err := s.db.Exec("DELETE FROM http_log")
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return n, nil
}
