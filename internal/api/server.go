// Package api 暴露 Gemini REST HTTP 端点。
package api

import (
	"net/http"
	"path/filepath"
	"time"
)

type Server struct {
	gemini *GeminiHandler
	openai *OpenAIHandler
	admin  *AdminHandler
	mw     *middleware
}

func NewServer(deps ServerDeps) *Server {
	h := handler{vc: deps.VC, keys: deps.Keys, cfg: deps.Cfg, deps: deps}
	return &Server{
		gemini: &GeminiHandler{h},
		openai: &OpenAIHandler{h},
		admin:  &AdminHandler{h},
		mw:     &middleware{cfg: deps.Cfg, keys: deps.Keys},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1beta/models", s.handleModelsGemini)
	mux.HandleFunc("/v1beta1/models", s.handleModelsGemini)
	mux.HandleFunc("/v1alpha/models", s.handleModelsGemini)
	// /v1/models 是双协议混响：Gemini 客户端读 models 字段，OpenAI 客户端读
	// object/data 字段。其余前缀保持纯 Gemini，守护既有客户端的不变式。
	mux.HandleFunc("/v1/models", s.handleModelsHybrid)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/admin", s.admin.handleAdminPage)
	mux.HandleFunc("/admin/", s.admin.handleAdminPage)
	mux.HandleFunc("/api/admin/", s.admin.handleAdminAPI)
	mux.HandleFunc("/assets/", s.handleAssets)
	mux.HandleFunc("/v1beta/models/", s.gemini.handleModelsSubtree)
	mux.HandleFunc("/v1beta1/models/", s.gemini.handleModelsSubtree)
	mux.HandleFunc("/v1alpha/models/", s.gemini.handleModelsSubtree)
	mux.HandleFunc("/v1/models/", s.gemini.handleModelsSubtree)

	// OpenAI 兼容层：/v1/chat/completions 是 OpenAI 专属路径（与 /v1/models 及
	// /v1/models/ 子树零重叠），直接并入 /v1 使文档承诺的 base URL 真正可用。
	// 鉴权/限流错误格式由 middleware.isOpenAIPath 按该精确路径分流。
	mux.HandleFunc("/v1/chat/completions", s.openai.handleChatCompletions)

	if s.mw.cfg.DebugPprof() {
		mux.HandleFunc("/debug/pprof/", pprofIndex)
		mux.HandleFunc("/debug/pprof/cmdline", pprintCmdline)
		mux.HandleFunc("/debug/pprof/profile", pprofProfile)
		mux.HandleFunc("/debug/pprof/symbol", pprofSymbol)
		mux.HandleFunc("/debug/pprof/trace", pprofTrace)
		mux.HandleFunc("/debug/pprof/goroutine", pprofGoroutine)
		mux.HandleFunc("/debug/pprof/heap", pprofHeap)
		mux.HandleFunc("/debug/pprof/threadcreate", pprofThreadcreate)
		mux.HandleFunc("/debug/pprof/block", pprofBlock)
		mux.HandleFunc("/debug/pprof/mutex", pprofMutex)
	}

	return s.mw.withRecover(s.mw.withCORS(s.mw.withConcurrencyLimit(s.mw.withMetrics(s.mw.withAPIKey(s.mw.withBodyLimit(mux))))))
}

func (s *Server) handleFavicon(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	assetsDir := filepath.Join(filepath.Dir(s.mw.cfg.ConfigDir()), "assets")
	fs := http.StripPrefix("/assets/", http.FileServer(http.Dir(assetsDir)))
	fs.ServeHTTP(w, r)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// 统一为 Gemini 规范错误形状：此前是 {"error":"not found"} 字符串，
		// 属于既非 Gemini 也非 OpenAI 的第三套形状，网关无法解析。
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "message": "not found", "status": "NOT_FOUND",
		}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Vertex AI Proxy", "version": "2.0-go"})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// 仅检查 HTTP 进程存活；不访问上游、不逐次记录日志，避免探针刷屏。
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "healthy",
		"timestamp":       time.Now().Unix(),
		"api_keys_loaded": s.mw.keys.Count(),
	})
}

func (s *Server) handleModelsGemini(w http.ResponseWriter, _ *http.Request) {
	models := s.mw.cfg.ModelsWithVariants()
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, geminiModelInfo(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": data})
}

// handleModelsHybrid 服务 GET /v1/models 的双协议混响。
//
// /v1 是文档承诺的 OpenAI base URL，但 Gemini 客户端也在用同一前缀（README 引导填
// /v1，SillyTavern 等据 supportedGenerationMethods 过滤）。两类客户端各自只读自己
// 认识的字段——Gemini 读 models、OpenAI 读 object/data——故同响应同时携带两套形状，
// 互不干扰。/v1beta 等其余前缀保持纯 Gemini（handleModelsGemini），不变式由
// gemini_routing_test.go 守护。
func (s *Server) handleModelsHybrid(w http.ResponseWriter, _ *http.Request) {
	models := s.mw.cfg.ModelsWithVariants()
	gemini := make([]any, 0, len(models))
	openai := make([]any, 0, len(models))
	for _, m := range models {
		gemini = append(gemini, geminiModelInfo(m))
		openai = append(openai, openaiModelEntry(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": gemini,
		"object": "list",
		"data":   openai,
	})
}
