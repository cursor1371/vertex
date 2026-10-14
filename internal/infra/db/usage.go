package db

import (
	"database/sql"
	"fmt"
)

// UsageRecord 是一次请求的完整统计记录，对应 usage_log 表的一行。
// TS 使用 Unix 秒；各耗时字段单位均为毫秒。
type UsageRecord struct {
	TS    int64
	ReqID string
	Model string
	// RequestedModel 是客户端请求的原始模型名（含 抗截断- / 假非流- 等变体前缀），
	// 与 Model（实际发往上游的名字）可能不同。
	RequestedModel string
	Family         string
	APIKeyName     string
	UseID          string
	NodeName       string
	NodeURI        string
	Streaming      bool
	// ResponseMode 是客户端实际接收形态：增量 / 聚合 / 单次。
	// 上游侧恒为流式，故只有客户端侧才存在该维度。
	ResponseMode string
	// AntiTruncate 标记是否走 StreamChatCollect 竞速链路（首包锁定胜出）。
	AntiTruncate bool
	// AntiTruncateTransport 是合成工具传输的实际启用形态：空串=未启用；
	// tool-transport=合成工具已装配（正文经工具参数传输并增量还原）。
	// 与 AntiTruncate 布尔的差异：布尔还包含"仅改道竞速链路、未注入合成工具"
	// 的形态（结构性闸门拦下注入时），本字段只反映注入是否真实发生。
	AntiTruncateTransport string
	// Integrity 是响应完整性根因分类：complete / length / content_filter /
	// tool_calls / empty / incomplete。空串=未跟踪（请求未产出响应）。
	// 只做记录与标注，不参与 Success 判定。
	Integrity        string
	Success          bool
	StatusCode       int
	ErrorReason      string
	PromptTokens     int
	CandidatesTokens int
	TotalTokens      int
	TotalMs          int64
	TTFBMs           int64
	// UpstreamTTFBMs 是上游首字耗时（请求开始 → 上游首个网络字节）。
	// 与 TTFBMs（客户端首帧）区分：上游侧恒为流式，故该值对三个端点都有效；
	// 两者之差即代理自身开销（竞速等待/解包/序列化/写出）。
	UpstreamTTFBMs int64
	OutputMs       int64
	// FinishReason 是上游给出的终止原因原样值（STOP / MAX_TOKENS /
	// MALFORMED_FUNCTION_CALL / SAFETY 等）。存原始枚举而非布尔：
	// 上游新增枚举时无需再改表，展示层按需映射为"输出上限截断"等标注。
	// 注意它不参与 Success 判定——撞输出上限仍是 HTTP 成功。
	FinishReason string
}

// UsageHistoryItem 是历史请求列表中的单条记录（用于前端分页展示）。
type UsageHistoryItem struct {
	ID                    int64
	TS                    int64
	ReqID                 string
	Model                 string
	RequestedModel        string
	Family                string
	APIKeyName            string
	UseID                 string
	NodeName              string
	NodeURI               string
	Streaming             bool
	ResponseMode          string
	AntiTruncate          bool
	AntiTruncateTransport string
	Integrity             string
	Success               bool
	StatusCode            int
	ErrorReason           string
	PromptTokens          int
	CandidatesTokens      int
	TotalTokens           int
	TotalMs               int64
	TTFBMs                int64
	UpstreamTTFBMs        int64
	OutputMs              int64
	FinishReason          string
}

// UsageHistoryPage 是分页响应的数据载体。
type UsageHistoryPage struct {
	Items []UsageHistoryItem
	Total int64
	Page  int
	Limit int
}

// History 返回按时间倒序分页的历史请求记录；page 从 1 开始，limit 每页条数（默认 20，上限 200）。
func (s *UsageStore) History(page, limit int) (UsageHistoryPage, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	var total int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM usage_log").Scan(&total); err != nil {
		return UsageHistoryPage{}, fmt.Errorf("error: %w", err)
	}
	offset := (page - 1) * limit
	rows, err := s.db.Query(`
		SELECT id, ts, req_id, model, requested_model, family, api_key_name, useid, node_name, node_uri,
			streaming, response_mode, anti_truncate, anti_truncate_transport, integrity, success,
			status_code, error_reason, prompt_tokens, candidates_tokens, total_tokens,
			total_ms, ttfb_ms, upstream_ttfb_ms, output_ms, finish_reason
		FROM usage_log ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return UsageHistoryPage{}, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageHistoryItem
	for rows.Next() {
		var it UsageHistoryItem
		var streaming, success, antiTruncate int
		if err := rows.Scan(&it.ID, &it.TS, &it.ReqID, &it.Model, &it.RequestedModel, &it.Family,
			&it.APIKeyName, &it.UseID, &it.NodeName, &it.NodeURI, &streaming, &it.ResponseMode,
			&antiTruncate, &it.AntiTruncateTransport, &it.Integrity, &success, &it.StatusCode, &it.ErrorReason,
			&it.PromptTokens, &it.CandidatesTokens, &it.TotalTokens,
			&it.TotalMs, &it.TTFBMs, &it.UpstreamTTFBMs, &it.OutputMs, &it.FinishReason); err != nil {
			return UsageHistoryPage{}, fmt.Errorf("error: %w", err)
		}
		it.Streaming = streaming != 0
		it.Success = success != 0
		it.AntiTruncate = antiTruncate != 0
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return UsageHistoryPage{}, fmt.Errorf("error: %w", err)
	}
	return UsageHistoryPage{Items: out, Total: total, Page: page, Limit: limit}, nil
}

// ResetAll 清空 usage_log 全部记录，返回删除行数。
func (s *UsageStore) ResetAll() (int64, error) {
	return s.reset("DELETE FROM usage_log")
}

// UsageSummary 是某时间范围内的汇总指标。
type UsageSummary struct {
	Requests         int
	Success          int
	Errors           int
	PromptTokens     int
	CandidatesTokens int
	TotalTokens      int
	AvgTotalMs       float64
	AvgTTFBMs        float64
	AvgOutputMs      float64
	// AvgUpstreamTTFBMs 是"仅统计确实采到上游首字节的请求"的平均首字耗时。
	// 与 AvgTTFBMs（客户端首帧，非流式/聚合流下恒等于总耗时）不同：它剔除了未产出
	// 首字的记录（失败、纯本地端点如 countTokens），是唯一有意义的"平均首字"。
	AvgUpstreamTTFBMs float64
}

// UsageRow 是按某个维度（模型/API Key/节点）分组的聚合行。
type UsageRow struct {
	Name             string
	Requests         int
	Success          int
	Errors           int
	PromptTokens     int
	CandidatesTokens int
	TotalTokens      int
	// RawURI 仅在按节点分组时填充：记录里带 node_uri 时即为该 URI；
	// 老数据无 URI 时留空，由 API 层按名称反查兜底。按模型/Key 分组时保持空串。
	RawURI string
	// Locked 仅在按节点分组时由 API 层增强填充：该节点是否处于防删锁状态。
	Locked bool
	// Disabled 仅在按节点分组时由 API 层增强填充：该节点当前是否被禁用。
	Disabled bool
}

// UsagePoint 是趋势序列的一个数据点：bucket 为桶起始的 Unix 秒。
type UsagePoint struct {
	Ts          int64
	Requests    int
	Success     int
	Errors      int
	TotalTokens int
}

// UsageStore 是 usage_log 表的读写仓储。由 db.Open 建立的单连接 SQLite 实例驱动，
// 不持有包级状态，线程安全由底层 *sql.DB 保证。
type UsageStore struct {
	db *sql.DB
}

// usageInsertSQL 是单条写入语句，Insert 与 InsertBatch 共用，避免列清单漂移。
const usageInsertSQL = `
	INSERT INTO usage_log (ts, req_id, model, requested_model, family, api_key_name, useid, node_name, node_uri,
		streaming, response_mode, anti_truncate, anti_truncate_transport, integrity, success,
		status_code, error_reason, prompt_tokens, candidates_tokens, total_tokens,
		total_ms, ttfb_ms, upstream_ttfb_ms, output_ms, finish_reason)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// recArgs 将记录展开为与 usageInsertSQL 占位符顺序一致的绑定参数。
func recArgs(rec UsageRecord, success int) []any {
	streaming := 0
	if rec.Streaming {
		streaming = 1
	}
	antiTruncate := 0
	if rec.AntiTruncate {
		antiTruncate = 1
	}
	return []any{
		rec.TS, rec.ReqID, rec.Model, rec.RequestedModel, rec.Family, rec.APIKeyName, rec.UseID,
		rec.NodeName, rec.NodeURI, streaming, rec.ResponseMode, antiTruncate,
		rec.AntiTruncateTransport, rec.Integrity, success,
		rec.StatusCode, rec.ErrorReason, rec.PromptTokens, rec.CandidatesTokens, rec.TotalTokens,
		rec.TotalMs, rec.TTFBMs, rec.UpstreamTTFBMs, rec.OutputMs, rec.FinishReason,
	}
}

// 聚合查询共用的 SELECT 表达式片段（Aggregate/grouped/DailySeries 复用，避免口径漂移）。
const (
	exprSuccess = "COALESCE(SUM(CASE WHEN success=1 THEN 1 ELSE 0 END), 0)"
	exprErrors  = "COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), 0)"
	exprPrompt  = "COALESCE(SUM(prompt_tokens), 0)"
	exprCand    = "COALESCE(SUM(candidates_tokens), 0)"
	exprTotal   = "COALESCE(SUM(total_tokens), 0)"
)

// NewUsageStore 构造 UsageStore。database 必须已 Open 并完成表结构初始化。
func NewUsageStore(database *sql.DB) *UsageStore {
	return &UsageStore{db: database}
}

// Close 关闭底层数据库连接（仅对 NewMemoryStore 创建的实例有意义；
// 由 db.Open 创建的实例关闭权归调用方持有的 *sql.DB）。
func (s *UsageStore) Close() error {
	if s != nil && s.db != nil {
		return s.db.Close()
	}
	return nil
}

// NewMemoryStore 构造一个内存态 UsageStore（sqlite :memory:），供测试与未注入场景使用。
func NewMemoryStore() (*UsageStore, error) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	if err := createTables(database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &UsageStore{db: database}, nil
}

// Insert 写入单条统计记录。
func (s *UsageStore) Insert(rec UsageRecord) error {
	success := 0
	if rec.Success {
		success = 1
	}
	_, err := s.db.Exec(usageInsertSQL, recArgs(rec, success)...)
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

// InsertBatch 在单事务内批量写入统计记录，显著降低高并发下的写放大。
func (s *UsageStore) InsertBatch(recs []UsageRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	stmt, err := tx.Prepare(usageInsertSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("error: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, rec := range recs {
		success := 0
		if rec.Success {
			success = 1
		}
		if _, err = stmt.Exec(recArgs(rec, success)...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("error: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

// Aggregate 返回 [from, to) 时间范围内的汇总指标。from/to 为 Unix 秒；to<=from 时返回零值。
func (s *UsageStore) Aggregate(from, to int64) (UsageSummary, error) {
	var sum UsageSummary
	if to <= from {
		return sum, nil
	}
	q := fmt.Sprintf(`
	SELECT COUNT(*), %s, %s, %s, %s, %s,
		COALESCE(AVG(total_ms), 0),
		COALESCE(AVG(ttfb_ms), 0),
		COALESCE(AVG(output_ms), 0),
		-- 仅对"确实采到上游首字节"的记录求平均：AVG 自动忽略 CASE 产生的 NULL。
		-- 若直接 AVG(upstream_ttfb_ms)，未采到的 0 值会把均值显著拉低。
		COALESCE(AVG(CASE WHEN upstream_ttfb_ms > 0 THEN upstream_ttfb_ms END), 0)
	FROM usage_log WHERE ts >= ? AND ts < ?`,
		exprSuccess, exprErrors, exprPrompt, exprCand, exprTotal)
	err := s.db.QueryRow(q, from, to).Scan(
		&sum.Requests, &sum.Success, &sum.Errors,
		&sum.PromptTokens, &sum.CandidatesTokens, &sum.TotalTokens,
		&sum.AvgTotalMs, &sum.AvgTTFBMs, &sum.AvgOutputMs, &sum.AvgUpstreamTTFBMs)
	if err != nil {
		return sum, fmt.Errorf("error: %w", err)
	}
	return sum, nil
}

// ByModel 返回 [from, to) 范围内按模型分组的聚合行，按请求数降序。
func (s *UsageStore) ByModel(from, to int64) ([]UsageRow, error) {
	return s.grouped(from, to, "model")
}

// ByKey 返回 [from, to) 范围内按 API Key 名称分组的聚合行，按请求数降序。
func (s *UsageStore) ByKey(from, to int64) ([]UsageRow, error) {
	return s.grouped(from, to, "api_key_name")
}

// ByNode 返回 [from, to) 范围内按代理节点分组的聚合行，按请求数降序。
// 分组键优先取 node_uri（节点唯一标识），老数据无 URI 时回退 node_name；
// 这样同名不同 URI 的节点不会再被合并成一行，"单节点"身份与名称可一起识别。
func (s *UsageStore) ByNode(from, to int64) ([]UsageRow, error) {
	if to <= from {
		return nil, nil
	}
	q := fmt.Sprintf(`
	SELECT COALESCE(NULLIF(node_uri, ''), node_name) AS k, MAX(node_name), MAX(node_uri),
		COUNT(*), %s, %s, %s, %s, %s
	FROM usage_log WHERE ts >= ? AND ts < ?
	GROUP BY k ORDER BY COUNT(*) DESC`,
		exprSuccess, exprErrors, exprPrompt, exprCand, exprTotal)
	rows, err := s.db.Query(q, from, to)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var groupKey, uri string
		if err := rows.Scan(&groupKey, &r.Name, &uri, &r.Requests, &r.Success, &r.Errors,
			&r.PromptTokens, &r.CandidatesTokens, &r.TotalTokens); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		// 记录里确实带 URI 时作为唯一标识返回；否则留空，由上层按名称反查兜底。
		if uri != "" {
			r.RawURI = uri
		} else {
			r.RawURI = ""
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// grouped 是 ByModel/ByKey 的公共实现。
func (s *UsageStore) grouped(from, to int64, col string) ([]UsageRow, error) {
	if to <= from {
		return nil, nil
	}
	q := fmt.Sprintf(`
	SELECT %s, COUNT(*), %s, %s, %s, %s, %s
	FROM usage_log WHERE ts >= ? AND ts < ?
	GROUP BY %s ORDER BY COUNT(*) DESC`,
		col, exprSuccess, exprErrors, exprPrompt, exprCand, exprTotal, col)
	rows, err := s.db.Query(q, from, to)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Name, &r.Requests, &r.Success, &r.Errors,
			&r.PromptTokens, &r.CandidatesTokens, &r.TotalTokens); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// DailySeries 返回 [from, to) 范围内按固定桶长（秒）聚合的趋势序列，升序。
// bucketSec 例如 86400（按天）或 3600（按小时）；<=0 时回退为 86400。
func (s *UsageStore) DailySeries(from, to int64, bucketSec int64) ([]UsagePoint, error) {
	if to <= from {
		return nil, nil
	}
	if bucketSec <= 0 {
		bucketSec = 86400
	}
	q := fmt.Sprintf(`
	SELECT (ts / ?) * ? AS bucket, COUNT(*), %s, %s, %s
	FROM usage_log WHERE ts >= ? AND ts < ?
	GROUP BY bucket ORDER BY bucket ASC`,
		exprSuccess, exprErrors, exprTotal)
	rows, err := s.db.Query(q, bucketSec, bucketSec, from, to)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsagePoint
	for rows.Next() {
		var p UsagePoint
		if err := rows.Scan(&p.Ts, &p.Requests, &p.Success, &p.Errors, &p.TotalTokens); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// ResetOlderThan 删除 ts < cutoff 的统计记录，返回删除行数。
func (s *UsageStore) ResetOlderThan(cutoff int64) (int64, error) {
	return s.reset("DELETE FROM usage_log WHERE ts < ?", cutoff)
}

func (s *UsageStore) reset(q string, args ...any) (int64, error) {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return n, nil
}
