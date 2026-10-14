package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// adminDBRequest 以已登录 cookie 发起 /api/admin/db/* 请求。
func adminDBRequest(t *testing.T, srvURL, cookie, path, method string, payload any) *http.Response {
	t.Helper()
	var body *bytes.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srvURL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestAdminDBEndpoints 覆盖 /api/admin/db/* 的鉴权、体检响应与参数校验。
func TestAdminDBEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	t.Run("未登录一律 401", func(t *testing.T) {
		for _, tc := range []struct{ path, method string }{
			{"/api/admin/db/stats", http.MethodGet},
			{"/api/admin/db/check", http.MethodPost},
		} {
			resp := adminDBRequest(t, fx.server.URL, "", tc.path, tc.method, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s status=%d, want 401", tc.method, tc.path, resp.StatusCode)
			}
		}
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET /db/stats 返回体检字段", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/stats", http.MethodGet, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, k := range []string{"path", "page_size", "page_count", "freelist_bytes", "reclaim_ratio", "auto_vacuum", "tables"} {
			if _, ok := body[k]; !ok {
				t.Errorf("响应缺少字段 %q: %v", k, body)
			}
		}
	})

	t.Run("POST /db/check 返回完整性结论", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/check", http.MethodPost, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["ok"] != true || body["result"] != "ok" {
			t.Errorf("quick_check 结论异常: %v", body)
		}
	})

	t.Run("参数校验拒绝非法输入", func(t *testing.T) {
		cases := []struct {
			name    string
			path    string
			payload map[string]any
		}{
			{"未知表名", "/api/admin/db/clear", map[string]any{"table": "nodes"}},
			{"SQL 注入式表名", "/api/admin/db/clear", map[string]any{"table": "race_log; DROP TABLE nodes"}},
			{"清理天数为 0", "/api/admin/db/prune", map[string]any{"days": 0}},
			{"清理天数越界", "/api/admin/db/prune", map[string]any{"days": 99999}},
			{"未知回收模式", "/api/admin/db/vacuum", map[string]any{"mode": "shrink"}},
		}
		for _, tc := range cases {
			resp := adminDBRequest(t, fx.server.URL, cookie, tc.path, http.MethodPost, tc.payload)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: status=%d, want 400", tc.name, resp.StatusCode)
			}
		}
	})

	t.Run("合法清理与增量回收可执行", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/prune", http.MethodPost, map[string]any{"days": 30})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("prune status=%d, want 200", resp.StatusCode)
		}

		resp = adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/vacuum", http.MethodPost, map[string]any{"mode": "incremental"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("incremental vacuum status=%d, want 200", resp.StatusCode)
		}
	})

	t.Run("方法不匹配返回 405", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/stats", http.MethodPost, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status=%d, want 405", resp.StatusCode)
		}
	})
}

// TestAdminDBMaintain 覆盖一键「清理并回收空间」端点：鉴权、分阶段结果、
// 参数校验与失败时的响应形态（HTTP 500 + 完整分阶段结果，不谎报成功）。
func TestAdminDBMaintain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	t.Run("未登录 401", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, "", "/api/admin/db/maintain", http.MethodPost,
			map[string]any{"days": 7})
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status=%d, want 401", resp.StatusCode)
		}
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 方法不允许", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/maintain", http.MethodGet, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status=%d, want 405", resp.StatusCode)
		}
	})

	t.Run("参数越界 400", func(t *testing.T) {
		for _, days := range []int{-1, 99999} {
			resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/maintain", http.MethodPost,
				map[string]any{"days": days})
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("days=%d status=%d, want 400", days, resp.StatusCode)
			}
		}
	})

	t.Run("成功返回分阶段结果与前后快照", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/maintain", http.MethodPost,
			map[string]any{"days": 7, "init_incremental": false})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["ok"] != true {
			t.Fatalf("ok=%v, want true (body=%v)", body["ok"], body)
		}
		stages, _ := body["stages"].([]any)
		if len(stages) == 0 {
			t.Fatalf("应返回分阶段结果, got %v", body["stages"])
		}
		for _, k := range []string{"stats_before", "stats_after", "reclaim_outcome", "reclaimed_bytes",
			"freelist_freed_bytes", "checkpoint", "auto_vacuum_before", "auto_vacuum_after", "quick_check", "message"} {
			if _, ok := body[k]; !ok {
				t.Errorf("响应缺少字段 %q", k)
			}
		}
		if body["quick_check"] != "ok" {
			t.Errorf("quick_check=%v, want ok", body["quick_check"])
		}
		// 阶段名顺序固定，前端据此渲染进度。
		want := []string{"stats_before", "prune", "reclaim", "checkpoint", "verify", "stats_after"}
		for i, name := range want {
			if i >= len(stages) {
				t.Fatalf("阶段数不足: %v", stages)
			}
			m, _ := stages[i].(map[string]any)
			if m["name"] != name {
				t.Errorf("阶段[%d]=%v, want %q", i, m["name"], name)
			}
		}
	})

	t.Run("vacuum 支持 init_incremental 模式", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/vacuum", http.MethodPost,
			map[string]any{"mode": "init_incremental"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// 测试库由 Open 新建，已是 INCREMENTAL，故 changed=false。
		if body["changed"] != false {
			t.Errorf("新建库已是增量模式，changed 应为 false, got %v", body["changed"])
		}
		if body["auto_vacuum_after"] != float64(2) {
			t.Errorf("auto_vacuum_after=%v, want 2", body["auto_vacuum_after"])
		}
	})

	t.Run("增量回收区分没有空闲页", func(t *testing.T) {
		resp := adminDBRequest(t, fx.server.URL, cookie, "/api/admin/db/vacuum", http.MethodPost,
			map[string]any{"mode": "incremental"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// 空库没有空闲页：必须报 no_free_pages，而不是"成功释放 0 字节"。
		if body["reclaim_outcome"] != "no_free_pages" {
			t.Errorf("reclaim_outcome=%v, want no_free_pages (body=%v)", body["reclaim_outcome"], body)
		}
		if body["freed_bytes"] != float64(0) {
			t.Errorf("freed_bytes=%v, want 0", body["freed_bytes"])
		}
	})
}
