// Package nodestore 提供 exitpool / entrypool 共用的 SQLite 持久化内核。
//
// 仅承载「表名可变、列结构固定」的通用 SQL 与事务样板，全部为纯函数 + 数据视图，
// 禁止引入任何包级可变状态；两个节点池保留各自内存态、锁、回调编排与家族特有逻辑，
// 所有 SQL 操作委托本包执行。
package nodestore

import (
	"database/sql"
	"strings"
)

// 禁用来源取值（nodestore.DisableSource*）。它们回答"谁把这个节点禁掉的"，
// 并决定该禁用是否可被自动恢复：**只有 DisableSourceHealth 可以被自动探测恢复**，
// 其余来源都必须人工启用——自动策略不得擅自覆盖人工/永久性设置。
const (
	// DisableSourceManual 人工禁用（管理面板批量/单节点禁用按钮）。空串即人工，
	// 保留具名常量是为了调用点可读。
	DisableSourceManual = ""
	// DisableSourceHealth 策略「禁用后恢复探测」触发的禁用：首次匹配即移出可用池，
	// 自动探测成功即自动解除。
	DisableSourceHealth = "health"
	// DisableSourcePermanent 自动探测连续失败达阈值后的永久禁用：不再被探测、
	// 只能人工启用（避免"探测-恢复-再失败"的无限循环）。
	DisableSourcePermanent = "permanent"
	// DisableSourceSingleShot 策略「单次禁用」触发的禁用：首次匹配即移出可用池，
	// **不进入自动探测**，只能人工启用。与 permanent 同属不可自动恢复来源，
	// 二者分开只为审计可读（前者由策略直接决定，后者由探测连败升级而来）。
	DisableSourceSingleShot = "single_shot"
	// DisableSourceAdminTest admin 测试通道失败触发的直接禁用（评审确认的既有语义）。
	DisableSourceAdminTest = "admin_test"
	// DisableSourceUnsupported 协议不支持/解析失败导致的禁用。
	DisableSourceUnsupported = "unsupported"
)

// AutoRecoverable 报告该来源的禁用是否允许被自动探测恢复。
func AutoRecoverable(source string) bool {
	return source == DisableSourceHealth
}

// IsManualDisable 报告该来源是否属于"人工/永久"语义（不会被自动恢复）。
func IsManualDisable(source string) bool {
	return !AutoRecoverable(source)
}

// NodeRow 是节点表行的最小持久化视图。
// Type / Name / Locked / DisabledByHealth 及禁用审计四列由全量替换保存（SaveNodes）写入，
// 缺一不可，否则重启后字段丢失。DisabledByHealth 标记 disabled 由健康判定触发（而非人工
// 禁用），持久化它使"成功测速自动解除禁用"在重启后仍然成立；DisableSource 是它的细化
// 版本（health / permanent / admin_test / unsupported，空串表示人工禁用），
// DisableReason / DisabledAt / DisableFailureClass 回答"为什么禁、何时禁、当时什么故障"。
type NodeRow struct {
	RawURI              string
	Type                string
	Name                string
	Disabled            bool
	Locked              bool
	DisabledByHealth    bool
	DisableSource       string
	DisableReason       string
	DisabledAt          int64
	DisableFailureClass string
}

// HealthRow 是健康度表行的持久化视图（13 列，与两个健康表 schema 对齐）。
type HealthRow struct {
	RawURI              string
	SuccessCount        int
	FailCount           int
	ConsecutiveFailures int
	LastTestMs          float64
	LastTestError       string
	LastSuccessAt       int64
	LastFailAt          int64
	CooldownUntil       int64
	// LastTestAt 是最近一次检查的时间戳（LastTestMs 是本次耗时，不能当时间用）。
	LastTestAt int64
	// LastFailureClass / LastHTTPStatus 是最近一次失败的归因类别与 HTTP 状态码，
	// 面板据此展示失败类别徽章，不再只能看一段自由文本。
	LastFailureClass string
	LastHTTPStatus   int
	// AutoProbeFailStreak 是自动探测的连续失败次数：达到 health_disable_threshold 即把
	// 健康禁用升级为永久禁用（不再探测、只能人工启用）。
	AutoProbeFailStreak int
}

// 探测来源取值（故障记录的 source 列）：区分"这次失败是生产请求发现的，还是测速/自动
// 探测发现的"，便于排查"为什么这个节点在半夜被禁用了"。
const (
	ProbeSourceRequest     = "request"
	ProbeSourceAdminSingle = "admin_single"
	ProbeSourceAdminBatch  = "admin_batch"
	ProbeSourceAutoProbe   = "auto_probe"
	ProbeSourceUnsupported = "unsupported"
)

// DisableState 是一次禁用/启用要落库的完整状态（节点表侧）。
// 单独成结构而非并列参数：禁用来源、原因、时间、故障类别必须一起写，
// 拆成散参极易在某个调用点漏写其中一列，导致审计失真。
type DisableState struct {
	Disabled         bool
	DisabledByHealth bool
	Source           string
	Reason           string
	DisabledAt       int64
	FailureClass     string
}

// FaultRow 是 node_fault_log 的一行（按 scope+节点+类别+错误签名聚合后的结果）：
// 同一类故障反复发生时只累加 count 并刷新 last_seen_at，面板展开即可看到
// 完整错误码、HTTP 状态码、脱敏原文、首次/最近发生时间与次数。
type FaultRow struct {
	ID           int64
	Scope        string
	NodeURI      string
	NodeName     string
	FailureClass string
	HTTPStatus   int
	ErrorCode    string
	ErrorSig     string
	RawError     string
	Target       string
	Source       string
	FirstSeenAt  int64
	LastSeenAt   int64
	Count        int64
}

// ---- 通用 SQL 部分（db 为 nil 时静默跳过，对齐各节点池无库内存模式）----

// LoadNodesFull 回调式扫描节点表（含禁用审计四列），调用方在 scan 回调内填充各自结构体，
// 避免两个包重复 SELECT 样板。整行为参数传递而非并列形参：审计列后续还会增加，
// 并列形参每加一列都要改所有调用点。
// nodeTable 仅接受包内常量表名（"nodes" / "entry_nodes"），不接收外部输入。
func LoadNodesFull(db *sql.DB, nodeTable string, scan func(NodeRow)) error {
	if db == nil {
		return nil
	}
	rows, err := db.Query("SELECT raw_uri, type, name, disabled, locked, disabled_by_health, " +
		"disable_source, disable_reason, disabled_at, disable_failure_class FROM " + nodeTable)
	if err != nil {
		return err
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var r NodeRow
		if err := rows.Scan(&r.RawURI, &r.Type, &r.Name, &r.Disabled, &r.Locked, &r.DisabledByHealth,
			&r.DisableSource, &r.DisableReason, &r.DisabledAt, &r.DisableFailureClass); err != nil {
			return err
		}
		scan(r)
	}
	return rows.Err()
}

// LoadHealth 扫描健康度表（13 列）为行视图；db 为 nil 时返回空切片。
func LoadHealth(db *sql.DB, healthTable string) ([]HealthRow, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT raw_uri, success_count, fail_count, consecutive_failures,
		last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until,
		last_test_at, last_failure_class, last_http_status, auto_probe_fail_streak FROM ` + healthTable)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	var out []HealthRow
	for rows.Next() {
		var h HealthRow
		if err := rows.Scan(&h.RawURI, &h.SuccessCount, &h.FailCount, &h.ConsecutiveFailures,
			&h.LastTestMs, &h.LastTestError, &h.LastSuccessAt, &h.LastFailAt, &h.CooldownUntil,
			&h.LastTestAt, &h.LastFailureClass, &h.LastHTTPStatus, &h.AutoProbeFailStreak); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveNodes 全量替换保存：先 DELETE 全表再逐行插入，维持现状全量替换语义。
//
// 注意：SQLite 未启用 foreign_keys，DELETE 不级联清空健康表，陈旧健康行的内存
// 剪枝由 PruneHealthKeys 承担（调用方必须在 SaveNodes 后执行，不得依赖级联假设）。
func SaveNodes(db *sql.DB, nodeTable string, rows []NodeRow) error {
	if db == nil {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM " + nodeTable); err != nil {
		_ = tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO " + nodeTable +
		" (raw_uri, type, name, disabled, locked, disabled_by_health, disable_source, disable_reason, disabled_at, disable_failure_class)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, r := range rows {
		if _, err := stmt.Exec(r.RawURI, r.Type, r.Name, r.Disabled, r.Locked, r.DisabledByHealth,
			r.DisableSource, r.DisableReason, r.DisabledAt, r.DisableFailureClass); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// SaveHealth 追加式替换保存：逐行 INSERT OR REPLACE，不得 DELETE（与现状一致）。
func SaveHealth(db *sql.DB, healthTable string, rows []HealthRow) error {
	if db == nil {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO ` + healthTable + `
		(raw_uri, success_count, fail_count, consecutive_failures, last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until,
		 last_test_at, last_failure_class, last_http_status, auto_probe_fail_streak) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, h := range rows {
		if _, err := stmt.Exec(h.RawURI, h.SuccessCount, h.FailCount, h.ConsecutiveFailures,
			h.LastTestMs, h.LastTestError, h.LastSuccessAt, h.LastFailAt, h.CooldownUntil,
			h.LastTestAt, h.LastFailureClass, h.LastHTTPStatus, h.AutoProbeFailStreak); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// UpsertDisabled 单行更新禁用状态（UPDATE，无匹配行则零影响）。
func UpsertDisabled(db *sql.DB, nodeTable string, uri string, st DisableState) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec("UPDATE "+nodeTable+" SET disabled = ?, disabled_by_health = ?, disable_source = ?,"+
		" disable_reason = ?, disabled_at = ?, disable_failure_class = ? WHERE raw_uri = ?",
		st.Disabled, st.DisabledByHealth, st.Source, st.Reason, st.DisabledAt, st.FailureClass, uri)
	return err
}

// UpsertHealth 单行写入健康度（INSERT OR REPLACE）。
func UpsertHealth(db *sql.DB, healthTable string, h HealthRow) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO `+healthTable+`
		(raw_uri, success_count, fail_count, consecutive_failures, last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until,
		 last_test_at, last_failure_class, last_http_status, auto_probe_fail_streak) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.RawURI, h.SuccessCount, h.FailCount, h.ConsecutiveFailures,
		h.LastTestMs, h.LastTestError, h.LastSuccessAt, h.LastFailAt, h.CooldownUntil,
		h.LastTestAt, h.LastFailureClass, h.LastHTTPStatus, h.AutoProbeFailStreak)
	return err
}

// UpdateDisabledBatch 事务内批量更新禁用状态：
// Begin → Prepare → 逐条 Exec → Close → Commit，任一环节失败即 Rollback 并返回 error；
// 重复 URI 天然幂等（UPDATE 语义）。
func UpdateDisabledBatch(db *sql.DB, nodeTable string, uris []string, st DisableState) error {
	if db == nil || len(uris) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("UPDATE " + nodeTable + " SET disabled = ?, disabled_by_health = ?, disable_source = ?," +
		" disable_reason = ?, disabled_at = ?, disable_failure_class = ? WHERE raw_uri = ?")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, u := range uris {
		if _, err := stmt.Exec(st.Disabled, st.DisabledByHealth, st.Source, st.Reason, st.DisabledAt, st.FailureClass, u); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// UpdateLockedBatch 事务内批量更新 locked（防删）标志：
// Begin → Prepare → 逐条 Exec → Close → Commit，任一环节失败即 Rollback 并返回 error；
// 重复 URI 天然幂等（UPDATE 语义）。
func UpdateLockedBatch(db *sql.DB, nodeTable string, uris []string, locked bool) error {
	if db == nil || len(uris) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("UPDATE " + nodeTable + " SET locked = ? WHERE raw_uri = ?")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, u := range uris {
		if _, err := stmt.Exec(locked, u); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// PruneHealthKeys 从 healthKeys 中删除所有不在 nodeKeys 里的键（内存剪枝）。
func PruneHealthKeys(healthKeys map[string]bool, nodeKeys map[string]bool) {
	for k := range healthKeys {
		if !nodeKeys[k] {
			delete(healthKeys, k)
		}
	}
}

// deleteNodesBatchTx 在同一事务内分批执行节点与健康表定向删除，
// 避免全表清空重写（SaveNodes 语义）在删除少量节点时的写放大。
func deleteNodesBatchTx(tx *sql.Tx, nodeTable, healthTable string, uris []string) error {
	stmtNode, err := tx.Prepare("DELETE FROM " + nodeTable + " WHERE raw_uri IN (" + placeholders(len(uris)) + ")")
	if err != nil {
		return err
	}
	defer func() { _ = stmtNode.Close() }()
	stmtHealth, err := tx.Prepare("DELETE FROM " + healthTable + " WHERE raw_uri IN (" + placeholders(len(uris)) + ")")
	if err != nil {
		return err
	}
	defer func() { _ = stmtHealth.Close() }()
	args := make([]any, len(uris))
	for i, u := range uris {
		args[i] = u
	}
	if _, err := stmtNode.Exec(args...); err != nil {
		return err
	}
	if _, err := stmtHealth.Exec(args...); err != nil {
		return err
	}
	return nil
}

// deleteBatchSize 单条 IN 子句占位符上限（SQLite 变量数限制安全值）。
const deleteBatchSize = 500

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

// DeleteDisabledNodes 定向删除全部 disabled=1 的节点行及其陈旧健康行（同一事务），
// 返回被删节点 URI 列表；db 为 nil 时返回 (0, nil, nil)，由调用方走纯内存分支。
// 健康表清理采用 NOT IN 语义对齐内存 pruneHealthUnsafe（foreign_keys 未启用，须显式清理）。
func DeleteDisabledNodes(db *sql.DB, nodeTable, healthTable string) (int, []string, error) {
	if db == nil {
		return 0, nil, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query("SELECT raw_uri FROM " + nodeTable + " WHERE disabled = 1 AND locked = 0")
	if err != nil {
		_ = tx.Rollback()
		return 0, nil, err
	}
	var uris []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return 0, nil, err
		}
		uris = append(uris, u)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return 0, nil, err
	}
	_ = rows.Close()
	if _, err := tx.Exec("DELETE FROM " + nodeTable + " WHERE disabled = 1 AND locked = 0"); err != nil {
		_ = tx.Rollback()
		return 0, nil, err
	}
	if _, err := tx.Exec("DELETE FROM " + healthTable + " WHERE raw_uri NOT IN (SELECT raw_uri FROM " + nodeTable + ")"); err != nil {
		_ = tx.Rollback()
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return len(uris), uris, nil
}

// DeleteNodesBatch 定向删除指定 URI 的节点行及其健康行（同一事务内分批执行）；
// 空 URI 列表与 db 为 nil 时静默跳过。仅删除不重写，替换全表清空语义仅适用于删除场景。
func DeleteNodesBatch(db *sql.DB, nodeTable, healthTable string, uris []string) error {
	if db == nil || len(uris) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for start := 0; start < len(uris); start += deleteBatchSize {
		end := start + deleteBatchSize
		if end > len(uris) {
			end = len(uris)
		}
		if err := deleteNodesBatchTx(tx, nodeTable, healthTable, uris[start:end]); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
