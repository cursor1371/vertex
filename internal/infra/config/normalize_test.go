package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeImageSizeTier(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"2K uppercase", "2K", "2K"},
		{"2k lowercase", "2k", "2K"},
		{"1K", "1K", "1K"},
		{"4K", "4K", "4K"},
		{"512", "512", "512"},
		{"8K invalid", "8K", ""},
		{"empty string", "", ""},
		{"whitespace around", "  2K  ", "2K"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeImageSizeTier(c.in); got != c.want {
				t.Errorf("normalizeImageSizeTier(%q)=%q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

func TestDefaultConfig_NewFields(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DefaultResponseModalities != "图文" {
		t.Errorf("DefaultResponseModalities=%q, want 图文", cfg.DefaultResponseModalities)
	}
	if cfg.StreamIdleTimeoutSeconds != 20 {
		t.Errorf("StreamIdleTimeoutSeconds=%d, want 20", cfg.StreamIdleTimeoutSeconds)
	}
	if got := cfg.HealthProbeFailThreshold(); got != 2 {
		t.Errorf("HealthProbeFailThreshold=%d, want 2", got)
	}
	if !cfg.HealthSelfHealEnabled {
		t.Error("HealthSelfHealEnabled=false, want true")
	}
	if cfg.HealthSelfHealIntervalMinutes != 120 {
		t.Errorf("HealthSelfHealIntervalMinutes=%d, want 120", cfg.HealthSelfHealIntervalMinutes)
	}
	if cfg.HealthSelfHealProbeConcurrency != 5 {
		t.Errorf("HealthSelfHealProbeConcurrency=%d, want 5", cfg.HealthSelfHealProbeConcurrency)
	}
	if !cfg.HealthGlobalFaultGuardEnabled {
		t.Error("HealthGlobalFaultGuardEnabled=false, want true")
	}
	if cfg.HealthGlobalFaultMinNodes != 3 {
		t.Errorf("HealthGlobalFaultMinNodes=%d, want 3", cfg.HealthGlobalFaultMinNodes)
	}
	if cfg.HealthGlobalFaultWindowSeconds != 120 {
		t.Errorf("HealthGlobalFaultWindowSeconds=%d, want 120", cfg.HealthGlobalFaultWindowSeconds)
	}
}

// TestLoad_HealthProbeFailThresholdNormalize 锁定新键的取值与钳制口径。
// 关键点：**0 是合法值**（无限探测），只有"键缺失"才回落默认 2——这正是该字段
// 必须用指针而非 int 的原因。
func TestLoad_HealthProbeFailThresholdNormalize(t *testing.T) {
	ptr := func(v int) *int { return &v }
	tests := []struct {
		name     string
		raw      string
		expected int
	}{
		{"键缺失回落默认 2", `{}`, 2},
		{"零保留为无限探测", `{"health_probe_fail_threshold":0}`, 0},
		{"一保留", `{"health_probe_fail_threshold":1}`, 1},
		{"二保留", `{"health_probe_fail_threshold":2}`, 2},
		{"超上限钳到 10", `{"health_probe_fail_threshold":99}`, 10},
		{"负值回落默认 2", `{"health_probe_fail_threshold":-3}`, 2},
		{"旧键生效（迁移兼容）", `{"health_disable_threshold":1}`, 1},
		{"新键优先于旧键", `{"health_probe_fail_threshold":0,"health_disable_threshold":5}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := t.TempDir()
			origCached := cached
			origCacheTime := cacheTime
			t.Cleanup(func() {
				cached = origCached
				cacheTime = origCacheTime
			})
			_ = os.Setenv("VPROXY_CONFIG", filepath.Join(d, "config.json"))
			t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })

			_ = os.WriteFile(filepath.Join(d, "config.json"), []byte(tt.raw), 0o644)
			InvalidateCache()
			if got := Load().HealthProbeFailThreshold(); got != tt.expected {
				t.Errorf("%s: got %d, want %d", tt.raw, got, tt.expected)
			}
		})
	}
	_ = ptr // 保持工具函数被引用（供后续用例复用）
}

// TestLoad_AutoMigratesLegacyConfigKey 旧键不再只是"日志提示"，而是被**就地改写成新键**：
// 运行在新键、文件里躺着弃用键的长期漂移必须消除（备份恢复与排查都会踩这个坑）。
func TestLoad_AutoMigratesLegacyConfigKey(t *testing.T) {
	d := t.TempDir()
	origCached := cached
	origCacheTime := cacheTime
	t.Cleanup(func() {
		cached = origCached
		cacheTime = origCacheTime
	})
	path := filepath.Join(d, "config.json")
	_ = os.Setenv("VPROXY_CONFIG", path)
	t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })

	// 只写旧键（外加一个无关键，验证迁移不会顺手抹掉其它配置）。
	_ = os.WriteFile(path, []byte(`{"health_disable_threshold":3,"port_api":2345}`), 0o644)
	InvalidateCache()
	cfg := Load()
	if got := cfg.HealthProbeFailThreshold(); got != 3 {
		t.Fatalf("旧键取值应生效, got %d", got)
	}

	// 文件已被迁移：新键写入、旧键删除、其它键保留。
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回配置: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("解析迁移后的配置: %v", err)
	}
	if _, hasLegacy := raw["health_disable_threshold"]; hasLegacy {
		t.Error("迁移后旧键应从文件里删除")
	}
	if v, ok := raw["health_probe_fail_threshold"].(float64); !ok || v != 3 {
		t.Errorf("迁移后新键应为 3, got %v", raw["health_probe_fail_threshold"])
	}
	if v, ok := raw["port_api"].(float64); !ok || v != 2345 {
		t.Errorf("迁移不得影响其它配置键, got %v", raw["port_api"])
	}

	// 再次 Load 应为空操作（幂等），值不变。
	InvalidateCache()
	if got := Load().HealthProbeFailThreshold(); got != 3 {
		t.Errorf("二次 Load 值应稳定, got %d", got)
	}
	raw2 := map[string]any{}
	data2, _ := os.ReadFile(path)
	_ = json.Unmarshal(data2, &raw2)
	if _, hasLegacy := raw2[legacyProbeFailThresholdKey]; hasLegacy {
		t.Error("二次 Load 不应把旧键写回来")
	}
}

// TestLoad_LegacyAndNewKeyCoexist 两个键同时存在时以新键为准，且旧键被清掉
// （否则"文件里有两个阈值"会成为长期困惑源）。
func TestLoad_LegacyAndNewKeyCoexist(t *testing.T) {
	d := t.TempDir()
	origCached := cached
	origCacheTime := cacheTime
	t.Cleanup(func() {
		cached = origCached
		cacheTime = origCacheTime
	})
	path := filepath.Join(d, "config.json")
	_ = os.Setenv("VPROXY_CONFIG", path)
	t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })

	_ = os.WriteFile(path, []byte(`{"health_probe_fail_threshold":0,"health_disable_threshold":7}`), 0o644)
	InvalidateCache()
	if got := Load().HealthProbeFailThreshold(); got != 0 {
		t.Fatalf("新键应优先（0 = 无限探测）, got %d", got)
	}
	raw := map[string]any{}
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &raw)
	if _, hasLegacy := raw[legacyProbeFailThresholdKey]; hasLegacy {
		t.Error("旧键应被清理")
	}
	if v, _ := raw["health_probe_fail_threshold"].(float64); v != 0 {
		t.Errorf("新键值应保持不变, got %v", raw["health_probe_fail_threshold"])
	}
}

func TestLoad_HealthSelfHealIntervalNormalize(t *testing.T) {
	tests := []struct {
		name     string
		input    int
		expected int
	}{
		{"zero falls back to 120", 0, 120},
		{"negative falls back to 120", -5, 120},
		{"one preserved", 1, 1},
		{"sixty preserved", 60, 60},
		{"one twenty preserved", 120, 120},
		{"over max clamped to 1440", 2000, 1440},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := t.TempDir()
			origCached := cached
			origCacheTime := cacheTime
			t.Cleanup(func() {
				cached = origCached
				cacheTime = origCacheTime
			})

			_ = os.Setenv("VPROXY_CONFIG", filepath.Join(d, "config.json"))
			t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })

			data, _ := json.Marshal(AppConfig{HealthSelfHealIntervalMinutes: tt.input})
			_ = os.WriteFile(filepath.Join(d, "config.json"), data, 0o644)
			InvalidateCache()
			cfg := Load()
			if cfg.HealthSelfHealIntervalMinutes != tt.expected {
				t.Errorf("got %d, want %d", cfg.HealthSelfHealIntervalMinutes, tt.expected)
			}
		})
	}
}

func TestLoad_DefaultResponseModalitiesNormalize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"空值回退图文", "", "图文"},
		{"仅图片保留", "仅图片", "仅图片"},
		{"图文保留", "图文", "图文"},
		{"非法值回退图文", "INVALID", "图文"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := t.TempDir()
			origCached := cached
			origCacheTime := cacheTime
			t.Cleanup(func() {
				cached = origCached
				cacheTime = origCacheTime
			})

			_ = os.Setenv("VPROXY_CONFIG", filepath.Join(d, "config.json"))
			t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })

			data, _ := json.Marshal(AppConfig{DefaultResponseModalities: tt.input})
			_ = os.WriteFile(filepath.Join(d, "config.json"), data, 0o644)
			InvalidateCache()
			cfg := Load()
			if cfg.DefaultResponseModalities != tt.expected {
				t.Errorf("got %q, want %q", cfg.DefaultResponseModalities, tt.expected)
			}
		})
	}
}

func TestLoad_StreamIdleTimeoutSecondsNormalize(t *testing.T) {
	tests := []struct {
		name     string
		input    int
		expected int
	}{
		{"zero falls back to 20", 0, 20},
		{"negative falls back to 20", -5, 20},
		{"positive preserved", 30, 30},
		{"default 20 preserved", 20, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := t.TempDir()
			origCached := cached
			origCacheTime := cacheTime
			t.Cleanup(func() {
				cached = origCached
				cacheTime = origCacheTime
			})

			_ = os.Setenv("VPROXY_CONFIG", filepath.Join(d, "config.json"))
			t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })

			data, _ := json.Marshal(AppConfig{StreamIdleTimeoutSeconds: tt.input})
			_ = os.WriteFile(filepath.Join(d, "config.json"), data, 0o644)
			InvalidateCache()
			cfg := Load()
			if cfg.StreamIdleTimeoutSeconds != tt.expected {
				t.Errorf("got %d, want %d", cfg.StreamIdleTimeoutSeconds, tt.expected)
			}
		})
	}
}
