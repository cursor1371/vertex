package nodestore

import "database/sql"

// 本文件是故障处理策略（fault_policies）的持久化内核。策略语义与解析见 failure_policy.go。
//
// 存储设计：**稀疏覆盖**——表里只存"与内置默认不同"的类别。
//   - 空表 = 全默认，因此"重置"就是删行，语义干净；
//   - 未知 / 未来新增的失败类别天然落内置默认，不会因为缺配置而异常；
//   - 表只增行不改结构，迁移成本为零（新老库都由 CREATE TABLE IF NOT EXISTS 覆盖）。

// PolicyRow 是一条策略持久化行。
type PolicyRow struct {
	Class FailureClass
	// Action 存字符串（而非枚举序号）：可读、可审计、跨版本稳定。
	Action PolicyAction
	// ProbeFailThreshold：-1 = 跟随全局配置；0 = 无限探测；>0 = 连续失败 N 次转永久。
	ProbeFailThreshold int
	UpdatedAt          int64
}

// LoadPolicies 读取全部自定义策略行（空表返回空切片；db 为 nil 时返回 nil）。
func LoadPolicies(db *sql.DB) ([]PolicyRow, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query("SELECT failure_class, action, probe_fail_threshold, updated_at FROM fault_policies")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	var out []PolicyRow
	for rows.Next() {
		var r PolicyRow
		var class, action string
		if err := rows.Scan(&class, &action, &r.ProbeFailThreshold, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Class = FailureClass(class)
		r.Action = PolicyAction(action)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertPolicy 写入/更新一条自定义策略行（按 failure_class 主键 UPSERT）。
func UpsertPolicy(db *sql.DB, r PolicyRow) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`INSERT INTO fault_policies (failure_class, action, probe_fail_threshold, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(failure_class) DO UPDATE SET
			action = excluded.action,
			probe_fail_threshold = excluded.probe_fail_threshold,
			updated_at = excluded.updated_at`,
		string(r.Class), string(r.Action), r.ProbeFailThreshold, r.UpdatedAt)
	return err
}

// DeletePolicy 删除某类别的自定义行（回落内置默认）。
func DeletePolicy(db *sql.DB, class FailureClass) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec("DELETE FROM fault_policies WHERE failure_class = ?", string(class))
	return err
}

// ResetPolicies 清空全部自定义行，返回删除行数（"重置为默认策略"）。
func ResetPolicies(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	res, err := db.Exec("DELETE FROM fault_policies")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PolicyRowsToSet 把 DB 行装配为解析器可用的自定义集合（已过滤非法行，见 PolicySet.Replace）。
func PolicyRowsToSet(rows []PolicyRow) []FaultPolicy {
	out := make([]FaultPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, FaultPolicy{
			Class:              r.Class,
			Action:             r.Action,
			ProbeFailThreshold: r.ProbeFailThreshold,
		})
	}
	return out
}
