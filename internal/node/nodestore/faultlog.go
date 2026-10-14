package nodestore

import "database/sql"

// 本文件是故障记录（node_fault_log）的持久化内核：按"节点 + 类别 + 错误签名"聚合，
// 供管理面板做故障分类统计与展开明细。保留期固定 1 天，由 db.PruneFaultLog 周期清理。
//
// 表与写入方的关系：写入来自两个节点池的失败记账路径（复用其异步健康写队列，
// 不阻塞请求热路径）；原文与签名在写入前都已脱敏（MaskSecrets），本层不再做清洗。

// FaultLogDefaultLimit 是故障列表查询的默认返回上限。
const FaultLogDefaultLimit = 500

// RecordFault 聚合写入一条故障记录：同 (scope, node_uri, failure_class, error_sig) 已存在
// 则 count+1、last_seen_at 刷新，并把 node_name / http_status / error_code / raw_error /
// target / source 覆盖为最新一次的值；不存在则插入（first_seen_at=last_seen_at，count=1）。
//
// 聚合而非逐条落库：故障表只服务于即时排查，"同一节点反复出现同一故障"面板要看的是
// 发生次数与首末时间，而不是成百上千行重复记录。
func RecordFault(db *sql.DB, r FaultRow) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`INSERT INTO node_fault_log
		(scope, node_uri, node_name, failure_class, http_status, error_code, error_sig, raw_error, target, source, first_seen_at, last_seen_at, count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(scope, node_uri, failure_class, error_sig) DO UPDATE SET
			count = count + 1,
			last_seen_at = excluded.last_seen_at,
			node_name = excluded.node_name,
			http_status = excluded.http_status,
			error_code = excluded.error_code,
			raw_error = excluded.raw_error,
			target = excluded.target,
			source = excluded.source`,
		r.Scope, r.NodeURI, r.NodeName, r.FailureClass, r.HTTPStatus, r.ErrorCode, r.ErrorSig,
		r.RawError, r.Target, r.Source, r.FirstSeenAt, r.LastSeenAt)
	return err
}

// LoadFaults 返回按最近发生时间倒序的故障记录；limit <= 0 时取 FaultLogDefaultLimit。
func LoadFaults(db *sql.DB, limit int) ([]FaultRow, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = FaultLogDefaultLimit
	}
	rows, err := db.Query(`SELECT id, scope, node_uri, node_name, failure_class, http_status, error_code,
		error_sig, raw_error, target, source, first_seen_at, last_seen_at, count
		FROM node_fault_log ORDER BY last_seen_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	var out []FaultRow
	for rows.Next() {
		var r FaultRow
		if err := rows.Scan(&r.ID, &r.Scope, &r.NodeURI, &r.NodeName, &r.FailureClass, &r.HTTPStatus,
			&r.ErrorCode, &r.ErrorSig, &r.RawError, &r.Target, &r.Source,
			&r.FirstSeenAt, &r.LastSeenAt, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearFaultLog 清空故障记录表，返回删除行数（管理面板「清空」按钮）。
func ClearFaultLog(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	res, err := db.Exec("DELETE FROM node_fault_log")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FaultTotals 返回故障表的全量总量（不受列表 limit 影响）：
// 聚合行数与累计发生次数。面板据此显示"真实总量"，避免把"最近 N 条"当成全部历史。
func FaultTotals(db *sql.DB) (rows int64, occurrences int64, err error) {
	if db == nil {
		return 0, 0, nil
	}
	err = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(count), 0) FROM node_fault_log").Scan(&rows, &occurrences)
	return rows, occurrences, err
}

// FaultClassStats 是单个失败类别的汇总（面板顶部分类统计的一行）。
type FaultClassStats struct {
	Class      string `json:"class"`
	Count      int64  `json:"count"` // 该类别的累计发生次数
	Rows       int64  `json:"rows"`  // 该类别的聚合行数（不同节点/签名的组合数）
	Nodes      int64  `json:"nodes"` // 该类别的涉及节点数
	LastSeenAt int64  `json:"last_seen_at"`
}

// FaultClassCounts 按类别汇总故障记录，供面板分类统计（含"其他"一分组）。
// 排序由调用方或前端处理：这里按次数降序，让最需要关注的问题排在前面。
func FaultClassCounts(db *sql.DB) ([]FaultClassStats, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT failure_class, SUM(count), COUNT(*), COUNT(DISTINCT node_uri), MAX(last_seen_at)
		FROM node_fault_log GROUP BY failure_class ORDER BY SUM(count) DESC`)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	var out []FaultClassStats
	for rows.Next() {
		var s FaultClassStats
		if err := rows.Scan(&s.Class, &s.Count, &s.Rows, &s.Nodes, &s.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
