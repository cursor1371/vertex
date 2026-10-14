package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// adminModelsRequest 以已登录 cookie 发起 /api/admin/models 请求（method 为 GET/PUT）。
func adminModelsRequest(t *testing.T, srvURL, cookie, method string, payload any) *http.Response {
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
	req, err := http.NewRequest(method, srvURL+"/api/admin/models", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestAdminModels_FamilyMapRoundTrip 验证 /api/admin/models：
//  1. GET 回带 family_map；
//  2. PUT 的 family_map 落盘并立即驱动家族判定；
//  3. 非法家族值整体拒绝（400），且不写盘。
func TestAdminModels_FamilyMapRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	t.Cleanup(config.InvalidateModelsCache)

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("PUT 合法 family_map 落盘并生效", func(t *testing.T) {
		resp := adminModelsRequest(t, fx.server.URL, cookie, http.MethodPut, map[string]any{
			"models":     []string{"gemini-2.5-flash", "custom-img"},
			"alias_map":  map[string]string{"fast": "gemini-2.5-flash"},
			"family_map": map[string]string{"custom-img": "image"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		if got := config.ModelFamilyOf("custom-img"); got != config.FamilyNameImage {
			t.Fatalf("显式家族声明未生效，ModelFamilyOf=%q", got)
		}
		if got := config.AliasMap()["fast"]; got != "gemini-2.5-flash" {
			t.Fatalf("别名未落盘，got %q", got)
		}
	})

	t.Run("GET 回带 family_map", func(t *testing.T) {
		resp := adminModelsRequest(t, fx.server.URL, cookie, http.MethodGet, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		fm, ok := body["family_map"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 family_map 对象: %v", body)
		}
		if fm["custom-img"] != "image" {
			t.Fatalf("family_map 内容不符: %v", fm)
		}
	})

	t.Run("PUT 非法家族值被拒绝且不写盘", func(t *testing.T) {
		resp := adminModelsRequest(t, fx.server.URL, cookie, http.MethodPut, map[string]any{
			"models":     []string{"only-one"},
			"family_map": map[string]string{"only-one": "video"},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400", resp.StatusCode)
		}
		// 拒绝必须发生在写盘之前：上一轮的模型清单应原样保留。
		models := config.BaseModels()
		found := false
		for _, m := range models {
			if m == "custom-img" {
				found = true
			}
		}
		if !found {
			t.Fatalf("非法请求不应改动模型清单，got %v", models)
		}
	})
}
