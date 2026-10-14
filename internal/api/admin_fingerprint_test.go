package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

// adminGetFingerprintRaw 使用已登录 cookie 发起 GET /api/admin/fingerprint。
func adminGetFingerprintRaw(t *testing.T, srvURL, cookie string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+"/api/admin/fingerprint", nil)
	if err != nil {
		t.Fatalf("adminGetFingerprintRaw new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("adminGetFingerprintRaw do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adminGetFingerprintRaw status=%d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("adminGetFingerprintRaw decode: %v", err)
	}
	return body
}

// newFingerprintFixture 启动一个带 admin 密码的测试服务器并登录，返回 (srvURL, cookie)。
func newFingerprintFixture(t *testing.T) (string, string) {
	t.Helper()
	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})
	return fx.server.URL, adminLogin(t, fx.server.URL)
}

func TestAdminFingerprint_SettingsRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srvURL, cookie := newFingerprintFixture(t)

	t.Run("GET settings 返回默认值", func(t *testing.T) {
		body := adminGetSettingsRaw(t, srvURL, cookie)
		settings := body["settings"].(map[string]any)
		if got, _ := settings["browser_fingerprint_enabled"].(bool); got {
			t.Error("browser_fingerprint_enabled 默认应为 false")
		}
		if got := settings["browser_fingerprint_mode"]; got != fingerprint.ModePreset {
			t.Errorf("mode=%v, want %v", got, fingerprint.ModePreset)
		}
		if got := settings["browser_fingerprint_preset"]; got != fingerprint.DefaultPreset {
			t.Errorf("preset=%v, want %v", got, fingerprint.DefaultPreset)
		}
		if got, _ := settings["browser_fingerprint_custom"].(string); got != "" {
			t.Errorf("custom 默认应为空串, got %q", got)
		}
		if got := settings["browser_fingerprint_tls_mode"]; got != fingerprint.TLSModeFollow {
			t.Errorf("tls_mode=%v, want %v", got, fingerprint.TLSModeFollow)
		}
		if got := settings["browser_fingerprint_locale"]; got != fingerprint.DefaultLocale {
			t.Errorf("locale=%v, want %v", got, fingerprint.DefaultLocale)
		}
	})

	t.Run("PUT 开关与预设持久化", func(t *testing.T) {
		resp := adminRequest(t, srvURL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"browser_fingerprint_enabled": true,
				"browser_fingerprint_mode":    "preset",
				"browser_fingerprint_preset":  "chrome_146",
			},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		config.InvalidateCache()
		cfg := config.Load()
		if !cfg.BrowserFingerprintEnabled || cfg.BrowserFingerprintPreset != "chrome_146" {
			t.Errorf("持久化结果 enabled=%v preset=%q", cfg.BrowserFingerprintEnabled, cfg.BrowserFingerprintPreset)
		}
	})

	t.Run("PUT TLS 策略与地区持久化", func(t *testing.T) {
		resp := adminRequest(t, srvURL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"browser_fingerprint_tls_mode": "range",
				"browser_fingerprint_tls_min":  144,
				"browser_fingerprint_tls_max":  152,
				"browser_fingerprint_locale":   "en-US",
			},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		config.InvalidateCache()
		cfg := config.Load()
		if cfg.BrowserFingerprintTLSMode != fingerprint.TLSModeRange || cfg.BrowserFingerprintTLSMin != 144 || cfg.BrowserFingerprintTLSMax != 152 {
			t.Errorf("TLS 区间持久化异常: mode=%q [%d, %d]", cfg.BrowserFingerprintTLSMode, cfg.BrowserFingerprintTLSMin, cfg.BrowserFingerprintTLSMax)
		}
		if cfg.BrowserFingerprintLocale != "en-US" {
			t.Errorf("locale 持久化异常: %q", cfg.BrowserFingerprintLocale)
		}
	})

	t.Run("PUT 手动包合法则保存", func(t *testing.T) {
		const valid = `{"user_agent":"UA-X"}`
		resp := adminRequest(t, srvURL, "/settings", cookie, map[string]any{
			"settings": map[string]any{"browser_fingerprint_custom": valid},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		config.InvalidateCache()
		if got := config.Load().BrowserFingerprintCustom; got != valid {
			t.Errorf("custom=%q, want %q", got, valid)
		}
	})

	t.Run("PUT 手动包非法返回 400 且不落盘", func(t *testing.T) {
		for name, raw := range map[string]string{
			"未知字段":            `{"nope":"x"}`,
			"tls_profile 已迁移": `{"tls_profile":"chrome_150"}`,
			"非字符串字段":          `{"user_agent":150}`,
			"坏JSON":           `{"user_agent":`,
		} {
			resp := adminRequest(t, srvURL, "/settings", cookie, map[string]any{
				"settings": map[string]any{"browser_fingerprint_custom": raw},
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: status=%d, want 400", name, resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
		config.InvalidateCache()
		if got := config.Load().BrowserFingerprintCustom; got != `{"user_agent":"UA-X"}` {
			t.Errorf("非法 PUT 不应改动已存手动包, got %q", got)
		}
	})

	t.Run("预设内容不可写：白名单外键被静默忽略", func(t *testing.T) {
		resp := adminRequest(t, srvURL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"chrome_150": map[string]any{"user_agent": "tampered"},
				"presets":    []string{"x"},
			},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200（白名单外键忽略而非报错）", resp.StatusCode)
		}
		// 预设注册表不受影响（只读保证）。
		if got := fingerprint.PresetByName("chrome_150").UA; strings.Contains(got, "tampered") {
			t.Error("预设内容被写穿，预设只读保证失效")
		}
	})
}

func TestAdminFingerprint_GetEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srvURL, cookie := newFingerprintFixture(t)

	body := adminGetFingerprintRaw(t, srvURL, cookie)
	presets, ok := body["presets"].([]any)
	if !ok || len(presets) != 3 {
		t.Fatalf("presets 应为 3 项, got %v", body["presets"])
	}
	first := presets[0].(map[string]any)
	if first["name"] != fingerprint.DefaultPreset {
		t.Errorf("首个预设=%v, want %v", first["name"], fingerprint.DefaultPreset)
	}
	if _, ok := first["verified"].(bool); !ok {
		t.Error("预设缺少 verified 标注")
	}
	if _, ok := first["headers"].(map[string]any); !ok {
		t.Error("预设缺少 headers 详情（只读展示用）")
	}
	if _, ok := first["tls_pool"].([]any); !ok {
		t.Error("预设缺少 tls_pool")
	}
	// TLS 策略解析：默认 follow → 池 = 首个（默认）预设的 TLS。
	tls, ok := body["tls"].(map[string]any)
	if !ok {
		t.Fatal("响应缺少 tls 对象")
	}
	if got := tls["mode"]; got != fingerprint.TLSModeFollow {
		t.Errorf("tls.mode=%v, want follow", got)
	}
	resolved, ok := tls["resolved_pool"].([]any)
	if !ok || len(resolved) != 1 || resolved[0] != fingerprint.DefaultPreset {
		t.Errorf("resolved_pool=%v, want [%v]", tls["resolved_pool"], fingerprint.DefaultPreset)
	}
	if _, ok := body["locales"].([]any); !ok {
		t.Error("响应缺少 locales 清单")
	}
	effective, ok := body["effective"].(map[string]any)
	if !ok {
		t.Fatal("响应缺少 effective 对象")
	}
	if active, _ := effective["active"].(bool); active {
		t.Error("默认关闭时 effective.active 应为 false")
	}
	if src, _ := effective["source"].(string); src != "legacy" {
		t.Errorf("默认 source=legacy, got %v", src)
	}
	if _, ok := body["tls_profiles"].([]any); !ok {
		t.Error("响应缺少 tls_profiles 清单")
	}
}
