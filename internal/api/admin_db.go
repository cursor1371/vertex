package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// requireDBMaintenance 断言数据库维护器可用；不可用时写入 500 并返回 false。
func (adm *AdminHandler) requireDBMaintenance(w http.ResponseWriter) (*db.DBMaintenance, bool) {
	if adm.deps.DB == nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("数据库维护器未初始化 (db maintenance unavailable)"))
		return nil, false
	}
	return adm.deps.DB, true
}

// adminGetDBStats 返回数据库体检快照：文件/WAL 体积、空闲页与可回收比例、
// auto_vacuum 模式、磁盘余量、各表行数与时间跨度。
func (adm *AdminHandler) adminGetDBStats(w http.ResponseWriter, _ *http.Request) {
	m, ok := adm.requireDBMaintenance(w)
	if !ok {
		return
	}
	st, err := m.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("数据库体检失败 (stats): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// adminPruneDB 按天数清理三张日志表（删除 ts 早于 now-days 的记录）。
func (adm *AdminHandler) adminPruneDB(w http.ResponseWriter, r *http.Request) {
	m, ok := adm.requireDBMaintenance(w)
	if !ok {
		return
	}
	var body struct {
		Days int `json:"days"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	// 与 adminSettingSpecs 的 history_retention_days 口径一致；下界 1 防止
	// days=0 时 cutoff 落到"现在"而一次删光全部历史。
	if body.Days < 1 || body.Days > 3650 {
		writeJSON(w, http.StatusBadRequest, adminErr("保留天数需在 1~3650 之间 (days out of range)"))
		return
	}
	cutoff := time.Now().Unix() - int64(body.Days)*86400
	deleted, err := m.PruneOlderThan(cutoff)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("清理失败 (prune): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "cutoff": cutoff})
}

// adminClearDB 清空指定日志表；table 取 usage_log / race_log / http_log / all。
func (adm *AdminHandler) adminClearDB(w http.ResponseWriter, r *http.Request) {
	m, ok := adm.requireDBMaintenance(w)
	if !ok {
		return
	}
	var body struct {
		Table string `json:"table"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	n, err := m.Clear(body.Table)
	if err != nil {
		if errors.Is(err, db.ErrUnknownTable) {
			writeJSON(w, http.StatusBadRequest,
				adminErr("未知表名，只允许 usage_log / race_log / http_log / all (unknown table): "+body.Table))
			return
		}
		writeJSON(w, http.StatusInternalServerError, adminErr("清空失败 (clear): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "table": body.Table, "deleted": n})
}

// adminVacuumDB 回收数据库空间。
//
// mode=incremental：在线归还空闲页，不需要额外磁盘，但仅在库的
// auto_vacuum=INCREMENTAL 时真正生效。模式不支持或没有空闲页时**不谎报**回收量，
// 而是通过 reclaim_outcome 说明原因。
// mode=init_incremental：一次性把旧库迁移到 auto_vacuum=INCREMENTAL（含 VACUUM 重建，
// 需要约 1.1× 库大小的临时空间），此后日常清理即可增量回收。
// mode=full：完整 VACUUM，需与库等量的临时空间，余量不足时返回 400 并提示走离线压缩。
func (adm *AdminHandler) adminVacuumDB(w http.ResponseWriter, r *http.Request) {
	m, ok := adm.requireDBMaintenance(w)
	if !ok {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	switch body.Mode {
	case "incremental":
		adm.vacuumIncremental(w, m)
	case "init_incremental":
		adm.vacuumInitIncremental(w, m)
	case "full":
		cp, err := m.FullVacuum()
		if err != nil {
			if errors.Is(err, db.ErrInsufficientSpace) {
				writeJSON(w, http.StatusBadRequest, adminErr("无法完整 VACUUM："+err.Error()))
				return
			}
			writeJSON(w, http.StatusInternalServerError, adminErr("完整 VACUUM 失败 (vacuum): "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "full", "checkpoint": cp})
	default:
		writeJSON(w, http.StatusBadRequest, adminErr("mode 需为 incremental / init_incremental / full (invalid mode)"))
	}
}

// vacuumIncremental 执行在线增量回收，并按实际情况区分回收结论。
func (adm *AdminHandler) vacuumIncremental(w http.ResponseWriter, m *db.DBMaintenance) {
	before, err := m.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("读取回收前统计失败： "+err.Error()))
		return
	}
	// 模式不支持时绝不回报"成功释放 0 字节"：那会让用户以为空间已回收。
	if before.AutoVacuum != db.AutoVacuumIncremental {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                    true,
			"mode":                  "incremental",
			"reclaim_outcome":       db.ReclaimModeNotSupported,
			"auto_vacuum":           before.AutoVacuum,
			"freed_bytes":           int64(0),
			"freelist_bytes_before": before.FreelistBytes,
			"message":               "当前 auto_vacuum 不是 INCREMENTAL，增量回收无法归还空间；请执行一次性初始化（mode=init_incremental）或停服运行离线压缩",
		})
		return
	}
	if _, err := m.IncrementalVacuum(0); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("增量回收失败 (incremental_vacuum): "+err.Error()))
		return
	}
	after, err := m.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("读取回收后统计失败： "+err.Error()))
		return
	}
	outcome := db.ReclaimDone
	freed := before.FileBytes - after.FileBytes
	if freed < 0 {
		freed = 0
	}
	switch {
	case freed > 0:
		outcome = db.ReclaimDone
	case before.FreelistBytes == 0:
		outcome = db.ReclaimNoFreePages
	default:
		outcome = db.ReclaimNoEffect
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"mode":                  "incremental",
		"reclaim_outcome":       outcome,
		"auto_vacuum":           after.AutoVacuum,
		"freed_bytes":           freed,
		"freelist_bytes_before": before.FreelistBytes,
		"freelist_bytes_after":  after.FreelistBytes,
		"file_bytes_before":     before.FileBytes,
		"file_bytes_after":      after.FileBytes,
	})
}

// vacuumInitIncremental 执行旧库一次性迁移（PRAGMA + VACUUM 重建），并回报前后体积。
func (adm *AdminHandler) vacuumInitIncremental(w http.ResponseWriter, m *db.DBMaintenance) {
	before, err := m.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("读取迁移前统计失败： "+err.Error()))
		return
	}
	changed, err := m.EnableIncrementalAutoVacuum()
	if err != nil {
		if errors.Is(err, db.ErrInsufficientSpace) {
			writeJSON(w, http.StatusBadRequest, adminErr("无法完成一次性初始化："+err.Error()))
			return
		}
		writeJSON(w, http.StatusInternalServerError, adminErr("一次性初始化失败 (init_incremental): "+err.Error()))
		return
	}
	after, err := m.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("读取迁移后统计失败： "+err.Error()))
		return
	}
	freed := before.FileBytes - after.FileBytes
	if freed < 0 {
		freed = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"mode":               "init_incremental",
		"changed":            changed,
		"reclaim_outcome":    db.ReclaimMigrated,
		"auto_vacuum_before": before.AutoVacuum,
		"auto_vacuum_after":  after.AutoVacuum,
		"freed_bytes":        freed,
		"file_bytes_before":  before.FileBytes,
		"file_bytes_after":   after.FileBytes,
		"wal_bytes_before":   before.WALBytes,
		"wal_bytes_after":    after.WALBytes,
	})
}

// adminMaintainDB 一键「清理并回收空间」：按保留天数清理 → 回收 → checkpoint → 校验，
// 返回分阶段结果与前后快照。
//
// 状态码语义：请求被正常处理并给出分阶段结果时返回 200（ok 字段表示业务成败）；
// 阶段失败返回 500，响应体同时带 error.message 与完整 result（含已完成阶段），
// 让接口调用方从状态码即可看出失败，也让前端能展示"做到哪一步"——不谎报全流程成功。
// 删除历史仅在 days>0 时发生；days=0 等价纯回收空间。
func (adm *AdminHandler) adminMaintainDB(w http.ResponseWriter, r *http.Request) {
	m, ok := adm.requireDBMaintenance(w)
	if !ok {
		return
	}
	var body struct {
		Days            int  `json:"days"`
		InitIncremental bool `json:"init_incremental"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	// 下界 0 表示"不删历史"；上界与 adminSettingSpecs 的保留天数口径一致。
	if body.Days < 0 || body.Days > 3650 {
		writeJSON(w, http.StatusBadRequest,
			adminErr("保留天数需在 0~3650 之间（0 表示不清理历史）(days out of range)"))
		return
	}
	res := m.Maintain(db.MaintainOptions{Days: body.Days, InitIncremental: body.InitIncremental})
	if !res.OK {
		payload := adminErr(res.Message)
		payload["result"] = res
		writeJSON(w, http.StatusInternalServerError, payload)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// adminCheckDB 执行 PRAGMA quick_check 做完整性体检。
func (adm *AdminHandler) adminCheckDB(w http.ResponseWriter, _ *http.Request) {
	m, ok := adm.requireDBMaintenance(w)
	if !ok {
		return
	}
	res, err := m.QuickCheck()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("完整性检查失败 (quick_check): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res == "ok", "result": res})
}
