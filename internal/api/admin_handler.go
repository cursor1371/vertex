package api

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/admin"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

type AdminHandler struct {
	handler
}

// adminLogSkipPath 是不打入口请求行的 admin 路径：请求监控页的 3 秒级轮询端点。
// 即使打开了 admin_api_log_enabled，它们也只会把进程日志刷成纯噪声，故无条件跳过。
var adminLogSkipPath = map[string]bool{ //nolint:gochecknoglobals // Constant-like skip table
	"/stats/active":   true,
	"/stats/rt-token": true,
}

// adminAPILogEnabled 读取 admin 控制台请求行开关；Cfg 未装配（部分测试直连 handler）时视为关。
func (adm *AdminHandler) adminAPILogEnabled() bool {
	return adm.deps.Cfg != nil && adm.deps.Cfg.AdminAPILogEnabled()
}

func (adm *AdminHandler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/admin")
	if !adminLogSkipPath[path] && adm.adminAPILogEnabled() {
		log.Printf("[Server] [AdminAPI] 收到请求: %s %s", r.Method, path)
	}

	if path == "/login" {
		adm.adminLogin(w, r)
		return
	}
	if path == "/check-auth" {
		adm.adminCheckAuth(w, r)
		return
	}

	if strings.HasPrefix(path, "/keys/") {
		if !requireAdmin(r) {
			adm.adminUnauthorized(w)
			return
		}
		adm.adminDeleteKey(w, r, strings.TrimPrefix(path, "/keys/"))
		return
	}

	if !requireAdmin(r) {
		adm.adminUnauthorized(w)
		return
	}

	switch path {
	case "/nodes":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetNodes(w, r)
		case http.MethodDelete:
			adm.adminDeleteNode(w, r)
		}
		return
	case "/nodes/faults":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetNodeFaults(w, r)
		case http.MethodDelete:
			adm.adminClearNodeFaults(w, r)
		}
		return
	case "/fault-policies":
		// 故障处理策略（策略可配置化）：GET 列出类别/生效值/默认值，PUT 逐类更新，DELETE 重置。
		switch r.Method {
		case http.MethodGet:
			adm.adminGetFaultPolicies(w, r)
		case http.MethodPut:
			adm.adminPutFaultPolicies(w, r)
		case http.MethodDelete:
			adm.adminDeleteFaultPolicies(w, r)
		}
		return
	case "/nodes/test":
		adm.adminTestNode(w, r)
		return
	case "/nodes/enable":
		adm.adminEnableNode(w, r)
		return
	case "/nodes/test-all":
		adm.adminTestAll(w, r)
		return
	case "/nodes/test-progress":
		if r.Method == http.MethodGet {
			adm.adminGetTestProgress(w, r)
		}
		return
	case "/nodes/test-pause":
		adm.adminTestPause(w, r)
		return
	case "/nodes/test-resume":
		adm.adminTestResume(w, r)
		return
	case "/nodes/test-terminate":
		adm.adminTestTerminate(w, r)
		return
	case "/nodes/deduplicate":
		adm.adminDedupNodes(w, r)
		return
	case "/nodes/disabled":
		adm.adminDeleteDisabledNodes(w, r)
		return
	case "/nodes/import":
		adm.adminImportNodes(w, r)
		return
	case "/subscriptions/fetch":
		adm.adminFetchSub(w, r)
		return
	case "/use-node":
		adm.adminUseNode(w, r)
		return
	case "/nodes/batch-disable":
		adm.adminBatchDisableNodes(w, r)
		return
	case "/nodes/batch-enable":
		adm.adminBatchEnableNodes(w, r)
		return
	case "/nodes/batch-delete":
		adm.adminBatchDeleteNodes(w, r)
		return
	case "/nodes/stats":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetNodesStatsByURI(w, r)
		return
	case "/nodes/sort":
		adm.adminSortNodesByLatency(w, r)
		return
	case "/nodes/lock":
		adm.adminLockNodes(w, r)
		return
	case "/nodes/lock-healthy":
		adm.adminLockHealthyNodes(w, r)
		return
	case "/nodes/unlock-all":
		adm.adminUnlockAllNodes(w, r)
		return
	case "/proxy-nodes/import":
		adm.adminImportProxyNode(w, r)
		return
	case "/proxy-nodes/import-batch":
		adm.adminImportProxyNodesBatch(w, r)
		return
	case "/proxy-nodes/toggle":
		adm.adminToggleProxyNodes(w, r)
		return
	case "/proxy-nodes/batch-delete":
		adm.adminBatchDeleteProxyNodes(w, r)
		return
	case "/proxy-nodes/delete-disabled":
		adm.adminDeleteDisabledProxyNodes(w, r)
		return
	case "/proxy-nodes/dedup":
		adm.adminDedupProxyNodes(w, r)
		return
	case "/proxy-nodes/test":
		adm.adminTestProxyNode(w, r)
		return
	case "/proxy-nodes/lock":
		adm.adminLockProxyNodes(w, r)
		return
	case "/proxy-nodes/unlock-all":
		adm.adminUnlockAllProxyNodes(w, r)
		return
	case "/proxy-nodes":
		if r.Method == http.MethodGet {
			adm.adminGetProxyNodes(w, r)
		} else {
			adm.adminMethodNotAllowed(w)
		}
		return
	case "/upload-bg":
		adm.adminUploadBg(w, r)
		return
	case "/delete-bg":
		adm.adminDeleteBg(w, r)
		return
	case "/list-bgs":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminListBgs(w, r)
		return
	}

	switch path {
	case "/logout":
		adm.adminLogout(w, r)
	case "/password":
		if r.Method == http.MethodPost {
			adm.adminChangePassword(w, r)
		} else {
			adm.adminMethodNotAllowed(w)
		}
	case "/settings":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetSettings(w, r)
		case http.MethodPut:
			adm.adminPutSettings(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
	case "/fingerprint":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetFingerprint(w, r)
	case "/stats":
		adm.adminGetStats(w, r)
	case "/db/stats":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetDBStats(w, r)
	case "/db/prune":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminPruneDB(w, r)
	case "/db/clear":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminClearDB(w, r)
	case "/db/vacuum":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminVacuumDB(w, r)
	case "/db/maintain":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminMaintainDB(w, r)
	case "/db/check":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminCheckDB(w, r)
	case "/stats/reset":
		adm.adminResetStats(w, r)
	case "/stats/history":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetStatsHistory(w, r)
	case "/stats/active":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetActiveRequests(w, r)
	case "/stats/rt-token":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetRTTokens(w, r)
	case "/requests/stop":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminStopRequest(w, r)
	case "/stats/records":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetHistoryRecords(w, r)
	case "/stats/nodes":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetNodeStats(w, r)
	case "/stats/racing":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetRacingStats(w, r)
	case "/stats/racing/reset":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminResetRacingStats(w, r)
	case "/http-log":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetHTTPLog(w, r)
	case "/http-log/detail":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetHTTPLogDetail(w, r)
	case "/http-log/reset":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminResetHTTPLog(w, r)
	case "/http-log/export":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminExportHTTPLog(w, r)
	case "/http-log/import":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminImportHTTPLog(w, r)
	case "/log":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetLog(w, r)
	case "/log/reset":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminClearLog(w, r)
	case "/keys":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetKeys(w, r)
		case http.MethodPost:
			adm.adminAddKey(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
	case "/models":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetModels(w, r)
		case http.MethodPut:
			adm.adminPutModels(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
	case "/models/fetch-upstream":
		// 获取上游模型目录：经节点池拉取 → 快照 + 差异，只写快照不改本地配置。
		adm.adminFetchUpstreamModels(w, r)
	default:
		writeJSON(w, http.StatusNotFound, adminErr("未知接口 (not found)"))
	}
}

func (adm *AdminHandler) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" {
		name = "admin.html"
	}
	data, err := fs.ReadFile(admin.Assets, "assets/"+name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func (adm *AdminHandler) adminUploadBg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("解析上传文件失败 (parse error)"))
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("未找到文件字段 (missing file)"))
		return
	}
	defer file.Close()

	assetsDir := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "assets")
	_ = os.MkdirAll(assetsDir, 0o755)

	filename := fmt.Sprintf("background%d.jpg", time.Now().UnixMilli())
	targetPath := filepath.Join(assetsDir, filename)

	out, err := os.Create(targetPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("无法保存文件 (create error)"))
		return
	}
	defer out.Close()

	if _, err = io.Copy(out, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("保存文件失败 (copy error)"))
		return
	}

	bgURL := "url('/assets/" + filename + "')"
	err = config.WriteSettings(map[string]any{"background_image": bgURL})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("更新配置失败 (save config error)"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": bgURL})
}

func (adm *AdminHandler) adminDeleteBg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Filename == "" || strings.Contains(body.Filename, "/") || strings.Contains(body.Filename, "\\") {
		writeJSON(w, http.StatusBadRequest, adminErr("文件名无效"))
		return
	}
	if !strings.HasPrefix(body.Filename, "background") {
		writeJSON(w, http.StatusForbidden, adminErr("无权删除该文件"))
		return
	}

	assetsDir := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "assets")
	targetPath := filepath.Join(assetsDir, body.Filename)
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, adminErr("删除文件失败"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminListBgs(w http.ResponseWriter, _ *http.Request) {
	assetsDir := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "assets")
	files, err := os.ReadDir(assetsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": []string{}})
		return
	}

	var bgs []string
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), "background") {
			bgs = append(bgs, f.Name())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": bgs})
}

// logFilePath 解析运行日志文件路径：优先 <配置目录>/../logs/logs_latest.log，
// 不存在时回退到工作目录下的相对路径（与 logger 的写入位置保持一致）。
func (adm *AdminHandler) logFilePath() string {
	logPath := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "logs", "logs_latest.log")
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		logPath = "logs/logs_latest.log"
	}
	return logPath
}

func (adm *AdminHandler) adminGetLog(w http.ResponseWriter, _ *http.Request) {
	logPath := adm.logFilePath()

	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": ""})
			return
		}
		writeJSON(w, http.StatusInternalServerError, adminErr("无法读取日志文件 (read error)"))
		return
	}

	lines := strings.Split(string(data), "\n")
	var validLines []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			validLines = append(validLines, l)
		}
	}
	if len(validLines) > 200 {
		validLines = validLines[len(validLines)-200:]
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": strings.Join(validLines, "\n")})
}

// adminClearLog 清空运行日志：优先调用注入的 logger（其内部加锁 truncate，避免与并发写
// 竞争产生半行）；未注入时（测试/旧装配）回退为直接截断日志文件。
func (adm *AdminHandler) adminClearLog(w http.ResponseWriter, _ *http.Request) {
	if adm.deps.Log != nil {
		if err := adm.deps.Log.Clear(); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("清空日志失败 (clear failed): "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := os.Truncate(adm.logFilePath(), 0); err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, adminErr("清空日志失败 (truncate failed): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
