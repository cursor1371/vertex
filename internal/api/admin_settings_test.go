package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// adminGetSettingsRaw 使用已登录 cookie 发起 GET /api/admin/settings，返回解析后的响应体。
func adminGetSettingsRaw(t *testing.T, srvURL, cookie string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+"/api/admin/settings", nil)
	if err != nil {
		t.Fatalf("adminGetSettings new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("adminGetSettings do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adminGetSettings status=%d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("adminGetSettings decode: %v", err)
	}
	return body
}

// TestAdminSettings_TrailingKeysRemoved 尾部回合兼容已迁移为模型级开关（model_settings.json）：
// 设置页接口不再返回两个旧键；携带旧键的 PUT 被白名单静默忽略，不写盘。
func TestAdminSettings_TrailingKeysRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 不再返回旧键", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		for _, key := range []string{"trailing_fix_models", "trailing_model_fix_enabled"} {
			if _, exists := settings[key]; exists {
				t.Errorf("设置接口不应再返回旧键 %s: %v", key, settings[key])
			}
		}
	})

	t.Run("PUT 旧键被白名单忽略且不写盘", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"trailing_fix_models":        []string{"gemini-3.6-flash"},
				"trailing_model_fix_enabled": true,
			},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		// 旧键不在白名单：不落盘、不改变归一化值（保持默认关闭与默认清单）。
		config.InvalidateCache()
		got := config.Load()
		if got.TrailingModelFixEnabled {
			t.Error("PUT 旧键不应生效（trailing_model_fix_enabled 应保持关闭）")
		}
		want := []string{"gemini-3.5-flash-lite", "gemini-3.6-flash", "gemini-3.7-flash"}
		if !reflect.DeepEqual(got.TrailingFixModels, want) {
			t.Errorf("PUT 旧键不应改写清单, got %v want %v", got.TrailingFixModels, want)
		}
	})
}

func TestAdminSettings_IPv6PolicyRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("PUT 持久化三个 IPv6 字段", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"force_node_ipv6":     true,
				"host_ipv6_interface": "eth0",
				"host_ipv6_address":   "2001:db8::1",
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		config.InvalidateCache()
		got := config.Load()
		if !got.ForceNodeIPv6 {
			t.Error("PUT 后 ForceNodeIPv6=false, want true")
		}
		if got.HostIPv6Interface != "eth0" {
			t.Errorf("PUT 后 HostIPv6Interface=%q, want eth0", got.HostIPv6Interface)
		}
		if got.HostIPv6Address != "2001:db8::1" {
			t.Errorf("PUT 后 HostIPv6Address=%q, want 2001:db8::1", got.HostIPv6Address)
		}
	})

	t.Run("GET 返回三个 IPv6 字段", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		if _, ok := settings["force_node_ipv6"]; !ok {
			t.Error("settings 缺少 force_node_ipv6")
		}
		if _, ok := settings["host_ipv6_interface"]; !ok {
			t.Error("settings 缺少 host_ipv6_interface")
		}
		if _, ok := settings["host_ipv6_address"]; !ok {
			t.Error("settings 缺少 host_ipv6_address")
		}
	})
}

// TestAdminSettings_RTCompensationRoundTrip 锁定 rt_compensation_ms 的 GET/PUT 全链路。
// 该键必须在后端白名单（PUT 不被静默丢弃）与 GET 响应（前端输入框有值）中同时接线，
// 只加前端字段表会造成"输入框空白且保存无效"。
func TestAdminSettings_RTCompensationRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 返回默认 500", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		got, ok := settings["rt_compensation_ms"].(float64)
		if !ok {
			t.Fatalf("settings 缺少 rt_compensation_ms, got %v", settings["rt_compensation_ms"])
		}
		if got != 500 {
			t.Errorf("rt_compensation_ms=%v, want 500", got)
		}
	})

	t.Run("PUT 持久化新值", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"rt_compensation_ms": 120,
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		config.InvalidateCache()
		if got := config.Load().RTCompensationMs; got != 120 {
			t.Errorf("PUT 后 RTCompensationMs=%d, want 120", got)
		}
	})

	t.Run("PUT 允许 0 表示立即重发", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"rt_compensation_ms": 0,
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		config.InvalidateCache()
		if got := config.Load().RTCompensationMs; got != 0 {
			t.Errorf("PUT 后 RTCompensationMs=%d, want 0", got)
		}
	})
}

// TestAdminSettings_PrefetchRoundTrip 锁定 rT 预取池三个配置键的 GET/PUT 全链路。
// 三者必须在后端白名单（PUT 不被静默丢弃）与 GET 响应（前端输入框有值）中同时接线，
// 只加前端字段表会造成"输入框空白且保存无效"（包C 的 rt_compensation_ms 即此问题）。
func TestAdminSettings_PrefetchRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 返回默认关/深度 5/TTL 90", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		if got, ok := settings["recaptcha_token_prefetch"].(bool); !ok || got {
			t.Errorf("recaptcha_token_prefetch=%v, want false", settings["recaptcha_token_prefetch"])
		}
		if got, ok := settings["recaptcha_prefetch_depth"].(float64); !ok || got != 5 {
			t.Errorf("recaptcha_prefetch_depth=%v, want 5", settings["recaptcha_prefetch_depth"])
		}
		if got, ok := settings["recaptcha_prefetch_ttl_sec"].(float64); !ok || got != 90 {
			t.Errorf("recaptcha_prefetch_ttl_sec=%v, want 90", settings["recaptcha_prefetch_ttl_sec"])
		}
		if got, ok := settings["recaptcha_no_direct_fetch"].(bool); !ok || got {
			t.Errorf("recaptcha_no_direct_fetch=%v, want false", settings["recaptcha_no_direct_fetch"])
		}
		if got, ok := settings["recaptcha_prefetch_budget_sec"].(float64); !ok || got != 15 {
			t.Errorf("recaptcha_prefetch_budget_sec=%v, want 15", settings["recaptcha_prefetch_budget_sec"])
		}
		if got, ok := settings["recaptcha_live_budget_sec"].(float64); !ok || got != 15 {
			t.Errorf("recaptcha_live_budget_sec=%v, want 15", settings["recaptcha_live_budget_sec"])
		}
	})

	t.Run("PUT 持久化开启与自定义深度/TTL", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"recaptcha_token_prefetch":      true,
				"recaptcha_prefetch_depth":      3,
				"recaptcha_prefetch_ttl_sec":    60,
				"recaptcha_no_direct_fetch":     true,
				"recaptcha_prefetch_budget_sec": 45,
				"recaptcha_live_budget_sec":     45,
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		config.InvalidateCache()
		got := config.Load()
		if !got.RecaptchaTokenPrefetch {
			t.Error("PUT 后 RecaptchaTokenPrefetch=false, want true")
		}
		if got.RecaptchaPrefetchDepth != 3 {
			t.Errorf("PUT 后 RecaptchaPrefetchDepth=%d, want 3", got.RecaptchaPrefetchDepth)
		}
		if got.RecaptchaPrefetchTTLSec != 60 {
			t.Errorf("PUT 后 RecaptchaPrefetchTTLSec=%d, want 60", got.RecaptchaPrefetchTTLSec)
		}
		if !got.RecaptchaNoDirectFetch {
			t.Error("PUT 后 RecaptchaNoDirectFetch=false, want true（必须同时接线白名单与 GET）")
		}
		if got.RecaptchaPrefetchBudgetSec != 45 {
			t.Errorf("PUT 后 RecaptchaPrefetchBudgetSec=%d, want 45", got.RecaptchaPrefetchBudgetSec)
		}
		if got.RecaptchaLiveBudgetSec != 45 {
			t.Errorf("PUT 后 RecaptchaLiveBudgetSec=%d, want 45", got.RecaptchaLiveBudgetSec)
		}
	})

	t.Run("PUT 超出值域的设置被 400 拒绝且不落盘", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"recaptcha_prefetch_ttl_sec":    115,
				"recaptcha_prefetch_budget_sec": 300,
				"recaptcha_live_budget_sec":     300,
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT status=%d, want 400（超值域应在入口拒绝，而非静默钳制后落盘）", resp.StatusCode)
		}

		// 拒绝即整批不写入：被拒请求不得改动任何一项。
		config.InvalidateCache()
		got := config.Load()
		if got.RecaptchaPrefetchTTLSec == 115 || got.RecaptchaPrefetchBudgetSec == 300 || got.RecaptchaLiveBudgetSec == 300 {
			t.Errorf("超值域请求不应写入配置: ttl=%d budget=%d live=%d",
				got.RecaptchaPrefetchTTLSec, got.RecaptchaPrefetchBudgetSec, got.RecaptchaLiveBudgetSec)
		}
	})

	t.Run("PUT 值域边界值被接受", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"recaptcha_prefetch_ttl_sec":    110,
				"recaptcha_prefetch_budget_sec": 180,
				"recaptcha_live_budget_sec":     120,
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		config.InvalidateCache()
		got := config.Load()
		if got.RecaptchaPrefetchTTLSec != 110 {
			t.Errorf("边界 TTL 应为 110, got %d", got.RecaptchaPrefetchTTLSec)
		}
		if got.RecaptchaPrefetchBudgetSec != 180 {
			t.Errorf("边界预取单轮预算应为 180, got %d", got.RecaptchaPrefetchBudgetSec)
		}
		if got.RecaptchaLiveBudgetSec != 120 {
			t.Errorf("边界实时单轮预算应为 120, got %d", got.RecaptchaLiveBudgetSec)
		}
	})

	t.Run("PUT 类型错误的设置被 400 拒绝", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"max_concurrent": "abc", // 字符串冒充数字：此前会被原样写进 config.json
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT status=%d, want 400（类型错误必须在入口拒绝）", resp.StatusCode)
		}
	})
}

// TestAdminSettings_SpecCoversGETResponse 锁定 PUT 规格表与 GET 响应的键集合一致：
// 规格表缺键 → 新设置项保存被静默丢弃；GET 缺键 → 面板输入框空白。
// 两份清单此前靠人眼同步，本测试把漂移变成 CI 失败。
func TestAdminSettings_SpecCoversGETResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)
	body := adminGetSettingsRaw(t, fx.server.URL, cookie)
	settings, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatalf("GET /settings 响应缺少 settings 对象: %v", body)
	}

	getKeys := make(map[string]bool, len(settings))
	for k := range settings {
		getKeys[k] = true
	}
	for k := range adminSettingSpecs {
		if !getKeys[k] {
			t.Errorf("规格表键 %q 未出现在 GET 响应（面板将显示空白）", k)
		}
	}
	for k := range getKeys {
		if _, ok := adminSettingSpecs[k]; !ok {
			t.Errorf("GET 响应键 %q 不在 PUT 规格表中（保存会被静默丢弃）", k)
		}
	}
}

// TestAdminSettings_HealthProbeFailThresholdRoundTrip 锁定 health_probe_fail_threshold 的
// GET/PUT 全链路（探测阶段失败阈值，默认 2，范围 0~10，0 = 无限探测）。
func TestAdminSettings_HealthProbeFailThresholdRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 返回默认 2", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		got, ok := settings["health_probe_fail_threshold"].(float64)
		if !ok {
			t.Fatalf("settings 缺少 health_probe_fail_threshold, got %v", settings["health_probe_fail_threshold"])
		}
		if got != 2 {
			t.Errorf("health_probe_fail_threshold=%v, want 2", got)
		}
		if _, stale := settings["health_disable_threshold"]; stale {
			t.Error("GET 不应再返回已弃用的旧键 health_disable_threshold")
		}
	})

	t.Run("PUT 持久化新值", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{"health_probe_fail_threshold": 5},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		config.InvalidateCache()
		if got := config.Load().HealthProbeFailThreshold(); got != 5 {
			t.Errorf("PUT 后 HealthProbeFailThreshold=%d, want 5", got)
		}
	})

	t.Run("PUT 超出值域被 400 拒绝", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{"health_probe_fail_threshold": 11},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT status=%d, want 400", resp.StatusCode)
		}
		config.InvalidateCache()
		if got := config.Load().HealthProbeFailThreshold(); got == 11 {
			t.Error("超值域请求不应写入配置")
		}
	})
}

// TestAdminSettings_HealthSelfHealRoundTrip 锁定自愈探测开关与间隔的 GET/PUT 全链路。
func TestAdminSettings_HealthSelfHealRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 返回默认 开/120", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		if got, ok := settings["health_selfheal_enabled"].(bool); !ok || !got {
			t.Errorf("health_selfheal_enabled=%v, want true", settings["health_selfheal_enabled"])
		}
		if got, ok := settings["health_selfheal_interval_minutes"].(float64); !ok || got != 120 {
			t.Errorf("health_selfheal_interval_minutes=%v, want 120", settings["health_selfheal_interval_minutes"])
		}
		if got, ok := settings["health_selfheal_probe_concurrency"].(float64); !ok || got != 5 {
			t.Errorf("health_selfheal_probe_concurrency=%v, want 5", settings["health_selfheal_probe_concurrency"])
		}
		if got, ok := settings["health_global_fault_guard_enabled"].(bool); !ok || !got {
			t.Errorf("health_global_fault_guard_enabled=%v, want true", settings["health_global_fault_guard_enabled"])
		}
		if got, ok := settings["health_global_fault_min_nodes"].(float64); !ok || got != 3 {
			t.Errorf("health_global_fault_min_nodes=%v, want 3", settings["health_global_fault_min_nodes"])
		}
		if got, ok := settings["health_global_fault_window_seconds"].(float64); !ok || got != 120 {
			t.Errorf("health_global_fault_window_seconds=%v, want 120", settings["health_global_fault_window_seconds"])
		}
	})

	t.Run("PUT 持久化关闭与自定义间隔", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"health_selfheal_enabled":          false,
				"health_selfheal_interval_minutes": 30,
			},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		config.InvalidateCache()
		got := config.Load()
		if got.HealthSelfHealEnabled {
			t.Error("PUT 后 HealthSelfHealEnabled=true, want false")
		}
		if got.HealthSelfHealIntervalMinutes != 30 {
			t.Errorf("PUT 后 HealthSelfHealIntervalMinutes=%d, want 30", got.HealthSelfHealIntervalMinutes)
		}
	})

	t.Run("PUT 超出值域被 400 拒绝", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{"health_selfheal_interval_minutes": 1441},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT status=%d, want 400", resp.StatusCode)
		}
		config.InvalidateCache()
		if got := config.Load().HealthSelfHealIntervalMinutes; got == 1441 {
			t.Error("超值域请求不应写入配置")
		}
	})
}
