package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestAdminSettings_ForcedToolCallRoundTrip 覆盖强制工具调用两个配置键的读写与归一化。
// 注意：admin handler 持有静态 provider 快照，PUT 后的持久化结果须从 config.Load() 验证。
func TestAdminSettings_ForcedToolCallRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})

	cookie := adminLogin(t, fx.server.URL)

	t.Run("GET 返回默认值", func(t *testing.T) {
		body := adminGetSettingsRaw(t, fx.server.URL, cookie)
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 settings 对象: %v", body)
		}
		if got := settings["forced_tool_call_mode"]; got != config.ForcedModeOff {
			t.Errorf("forced_tool_call_mode=%v, want %v", got, config.ForcedModeOff)
		}
		raw, ok := settings["forced_tool_call_models"].([]any)
		if !ok {
			t.Fatalf("settings 缺少 forced_tool_call_models 数组, got %v", settings["forced_tool_call_models"])
		}
		if len(raw) != 0 {
			t.Errorf("forced_tool_call_models 默认应为空数组, got %v", raw)
		}
	})

	t.Run("PUT 归一化去重 trim 后持久化", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{
				"forced_tool_call_mode":   config.ForcedModeRequired,
				"forced_tool_call_models": []string{" gemini-3.7-flash ", "gemini-3.7-flash", "", "  "},
			},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}

		loaded := config.Load()
		if loaded.ForcedToolCallMode != config.ForcedModeRequired {
			t.Errorf("持久化 mode=%q, want %q", loaded.ForcedToolCallMode, config.ForcedModeRequired)
		}
		want := []string{"gemini-3.7-flash"}
		if !reflect.DeepEqual(loaded.ForcedToolCallModels, want) {
			t.Errorf("持久化 models=%v, want %v", loaded.ForcedToolCallModels, want)
		}
	})

	t.Run("PUT 非法 mode 回退关闭", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{"forced_tool_call_mode": "bogus"},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		if got := config.Load().ForcedToolCallMode; got != config.ForcedModeOff {
			t.Errorf("非法 mode 应回退 %q, got %q", config.ForcedModeOff, got)
		}
	})

	t.Run("PUT 优先档可写入", func(t *testing.T) {
		resp := adminRequest(t, fx.server.URL, "/settings", cookie, map[string]any{
			"settings": map[string]any{"forced_tool_call_mode": config.ForcedModePrefer},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status=%d, want 200", resp.StatusCode)
		}
		if got := config.Load().ForcedToolCallMode; got != config.ForcedModePrefer {
			t.Errorf("合法 mode 应写入, got %q", got)
		}
	})
}
