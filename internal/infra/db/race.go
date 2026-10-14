package db

import (
	"fmt"
)

// RaceRecord 是一次"节点竞速候选"的结局记录，对应 race_log 表的一行。
// Outcome 取值：win（胜出）、collect（成功但未胜出）、timeout（候选级超时/取消）、
// ratelimit（429 正常竞争限流，不计入失败）、error（其他失败）。
type RaceRecord struct {
	TS        int64
	ReqID     string
	NodeName  string
	NodeURI   string
	Outcome   string
	ElapsedMs int64
}

// RaceNodeStat 是按节点聚合的竞速统计行。
// 口径：一次批量竞速只有胜出者算成功，Wins 之外的候选（Lost/Collects/Timeouts/Errors）一律视为不成功。
type RaceNodeStat struct {
	Name     string
	RawURI   string
	Attempts int
	Wins     int
	Lost     int
	Collects int
	Timeouts int
	Errors   int
	// RateLimits 是 429 正常竞争限流的独立计数：不算失败，前端成功率分母会扣除。
	RateLimits int
	AvgMs      float64
}

// RacePoint 是按时间桶聚合的竞速统计点。
type RacePoint struct {
	Ts         int64
	Attempts   int
	Wins       int
	Lost       int
	Collects   int
	Timeouts   int
	Errors     int
	RateLimits int
}

// 竞速聚合共用表达式（避免多处口径漂移）。
const (
	exprRaceWins     = "COALESCE(SUM(CASE WHEN outcome='win' THEN 1 ELSE 0 END), 0)"
	exprRaceLost     = "COALESCE(SUM(CASE WHEN outcome='lost' THEN 1 ELSE 0 END), 0)"
	exprRaceCollects = "COALESCE(SUM(CASE WHEN outcome='collect' THEN 1 ELSE 0 END), 0)"
	exprRaceTimeouts = "COALESCE(SUM(CASE WHEN outcome='timeout' THEN 1 ELSE 0 END), 0)"
	exprRaceErrors   = "COALESCE(SUM(CASE WHEN outcome='error' THEN 1 ELSE 0 END), 0)"
	// exprRaceRateLimits 单列 429：历史数据中 429 已存为 error 无法重分类，仅新数据分流。
	exprRaceRateLimits = "COALESCE(SUM(CASE WHEN outcome='ratelimit' THEN 1 ELSE 0 END), 0)"
)

const raceInsertSQL = `
	INSERT INTO race_log (ts, req_id, node_name, node_uri, outcome, elapsed_ms)
	VALUES (?, ?, ?, ?, ?, ?)`

// InsertRaceBatch 在单事务内批量写入竞速记录。
func (s *UsageStore) InsertRaceBatch(recs []RaceRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	stmt, err := tx.Prepare(raceInsertSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("error: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, rec := range recs {
		if _, err = stmt.Exec(rec.TS, rec.ReqID, rec.NodeName, rec.NodeURI, rec.Outcome, rec.ElapsedMs); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("error: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

// RaceByNode 返回 [from, to) 范围内按节点聚合的竞速统计，按候选次数降序。
// 分组键与 usage_log 保持一致：优先 node_uri，老数据回退 node_name。
func (s *UsageStore) RaceByNode(from, to int64) ([]RaceNodeStat, error) {
	if to <= from {
		return nil, nil
	}
	q := fmt.Sprintf(`
	SELECT MAX(node_name), COALESCE(NULLIF(node_uri, ''), node_name) AS k, MAX(node_uri),
		COUNT(*), %s, %s, %s, %s, %s, %s, COALESCE(AVG(elapsed_ms), 0)
	FROM race_log WHERE ts >= ? AND ts < ?
	GROUP BY k ORDER BY COUNT(*) DESC`,
		exprRaceWins, exprRaceLost, exprRaceCollects, exprRaceTimeouts, exprRaceErrors, exprRaceRateLimits)
	rows, err := s.db.Query(q, from, to)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RaceNodeStat
	for rows.Next() {
		var st RaceNodeStat
		var groupKey string
		if err := rows.Scan(&st.Name, &groupKey, &st.RawURI, &st.Attempts, &st.Wins, &st.Lost,
			&st.Collects, &st.Timeouts, &st.Errors, &st.RateLimits, &st.AvgMs); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// RaceSeries 返回 [from, to) 范围内按固定桶长（秒）聚合的竞速序列，升序。
func (s *UsageStore) RaceSeries(from, to int64, bucketSec int64) ([]RacePoint, error) {
	if to <= from {
		return nil, nil
	}
	if bucketSec <= 0 {
		bucketSec = 3600
	}
	q := fmt.Sprintf(`
	SELECT (ts / ?) * ? AS bucket, COUNT(*), %s, %s, %s, %s, %s, %s
	FROM race_log WHERE ts >= ? AND ts < ?
	GROUP BY bucket ORDER BY bucket ASC`,
		exprRaceWins, exprRaceLost, exprRaceCollects, exprRaceTimeouts, exprRaceErrors, exprRaceRateLimits)
	rows, err := s.db.Query(q, bucketSec, bucketSec, from, to)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RacePoint
	for rows.Next() {
		var p RacePoint
		if err := rows.Scan(&p.Ts, &p.Attempts, &p.Wins, &p.Lost, &p.Collects, &p.Timeouts, &p.Errors, &p.RateLimits); err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	return out, nil
}

// ResetRaceOlderThan 删除 ts < cutoff 的竞速记录，返回删除行数。
func (s *UsageStore) ResetRaceOlderThan(cutoff int64) (int64, error) {
	return s.reset("DELETE FROM race_log WHERE ts < ?", cutoff)
}

// ResetRaceAll 清空 race_log 全部记录。
func (s *UsageStore) ResetRaceAll() (int64, error) {
	return s.reset("DELETE FROM race_log")
}
