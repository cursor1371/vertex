package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

// loadFingerprintCfg 在临时目录写入仅含指纹字段的配置并加载（归一化后返回）。
func loadFingerprintCfg(t *testing.T, raw string) AppConfig {
	t.Helper()
	d := t.TempDir()
	origCached, origCacheTime := cached, cacheTime
	t.Cleanup(func() { cached, cacheTime = origCached, origCacheTime })
	_ = os.Setenv("VPROXY_CONFIG", filepath.Join(d, "config.json"))
	t.Cleanup(func() { _ = os.Unsetenv("VPROXY_CONFIG") })
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	return Load()
}

// loadAppCfg 用结构体序列化写入配置（规避手拼 JSON 的引号转义问题）。
func loadAppCfg(t *testing.T, cfg AppConfig) AppConfig {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return loadFingerprintCfg(t, string(data))
}

func TestLoad_BrowserFingerprintNormalize(t *testing.T) {
	const validCustom = `{"user_agent":"UA-X"}`

	t.Run("默认关闭且 preset 为默认值", func(t *testing.T) {
		cfg := loadFingerprintCfg(t, `{}`)
		if cfg.BrowserFingerprintEnabled {
			t.Error("默认应关闭")
		}
		if cfg.BrowserFingerprintMode != fingerprint.ModePreset {
			t.Errorf("mode=%q, want preset", cfg.BrowserFingerprintMode)
		}
		if cfg.BrowserFingerprintPreset != fingerprint.DefaultPreset {
			t.Errorf("preset=%q, want %q", cfg.BrowserFingerprintPreset, fingerprint.DefaultPreset)
		}
	})

	t.Run("mode 空值与非法值回退 preset", func(t *testing.T) {
		if got := loadFingerprintCfg(t, `{"browser_fingerprint_mode":""}`).BrowserFingerprintMode; got != fingerprint.ModePreset {
			t.Errorf("空 mode 回退 preset, got %q", got)
		}
		if got := loadFingerprintCfg(t, `{"browser_fingerprint_mode":"bogus"}`).BrowserFingerprintMode; got != fingerprint.ModePreset {
			t.Errorf("非法 mode 回退 preset, got %q", got)
		}
		if got := loadFingerprintCfg(t, `{"browser_fingerprint_mode":"custom"}`).BrowserFingerprintMode; got != fingerprint.ModeCustom {
			t.Errorf("custom 应保留, got %q", got)
		}
	})

	t.Run("preset 白名单外回退默认预设", func(t *testing.T) {
		cfg := loadFingerprintCfg(t, `{"browser_fingerprint_mode":"preset","browser_fingerprint_preset":"chrome_120"}`)
		if cfg.BrowserFingerprintPreset != fingerprint.DefaultPreset {
			t.Errorf("preset=%q, want 回退 %q", cfg.BrowserFingerprintPreset, fingerprint.DefaultPreset)
		}
	})

	t.Run("custom 合法时保持开启", func(t *testing.T) {
		cfg := loadAppCfg(t, AppConfig{BrowserFingerprintEnabled: true, BrowserFingerprintMode: fingerprint.ModeCustom, BrowserFingerprintCustom: validCustom})
		if !cfg.BrowserFingerprintEnabled {
			t.Error("合法 custom 应保持开启")
		}
	})

	t.Run("custom 非法时整体回退未启用（内存，不半生效）", func(t *testing.T) {
		cfg := loadAppCfg(t, AppConfig{BrowserFingerprintEnabled: true, BrowserFingerprintMode: fingerprint.ModeCustom, BrowserFingerprintCustom: `{"tls_profile":"chrome_120"}`})
		if cfg.BrowserFingerprintEnabled {
			t.Error("custom 含已迁移的 tls_profile 应回退为未启用")
		}
		cfg = loadAppCfg(t, AppConfig{BrowserFingerprintEnabled: true, BrowserFingerprintMode: fingerprint.ModeCustom, BrowserFingerprintCustom: `{not json}`})
		if cfg.BrowserFingerprintEnabled {
			t.Error("custom 坏 JSON 应回退为未启用")
		}
	})

	t.Run("tls_mode 白名单归一", func(t *testing.T) {
		if got := loadAppCfg(t, AppConfig{BrowserFingerprintTLSMode: "bogus"}).BrowserFingerprintTLSMode; got != fingerprint.TLSModeFollow {
			t.Errorf("非法 tls_mode 回退 follow, got %q", got)
		}
		if got := loadAppCfg(t, AppConfig{BrowserFingerprintTLSMode: ""}).BrowserFingerprintTLSMode; got != fingerprint.TLSModeFollow {
			t.Errorf("空 tls_mode 回退 follow, got %q", got)
		}
		if got := loadAppCfg(t, AppConfig{BrowserFingerprintTLSMode: fingerprint.TLSModeLegacy}).BrowserFingerprintTLSMode; got != fingerprint.TLSModeLegacy {
			t.Errorf("legacy 应保留, got %q", got)
		}
	})

	t.Run("single 模式 profile 非法回退 follow", func(t *testing.T) {
		cfg := loadAppCfg(t, AppConfig{BrowserFingerprintTLSMode: fingerprint.TLSModeSingle, BrowserFingerprintTLSProfile: "chrome_120"})
		if cfg.BrowserFingerprintTLSMode != fingerprint.TLSModeFollow {
			t.Errorf("非法 single profile 应回退 follow, got %q", cfg.BrowserFingerprintTLSMode)
		}
		cfg = loadAppCfg(t, AppConfig{BrowserFingerprintTLSMode: fingerprint.TLSModeSingle, BrowserFingerprintTLSProfile: "chrome_146"})
		if cfg.BrowserFingerprintTLSMode != fingerprint.TLSModeSingle || cfg.BrowserFingerprintTLSProfile != "chrome_146" {
			t.Errorf("合法 single 应保留: mode=%q profile=%q", cfg.BrowserFingerprintTLSMode, cfg.BrowserFingerprintTLSProfile)
		}
	})

	t.Run("range 模式区间越界钳制", func(t *testing.T) {
		cfg := loadAppCfg(t, AppConfig{BrowserFingerprintTLSMode: fingerprint.TLSModeRange, BrowserFingerprintTLSMin: 50, BrowserFingerprintTLSMax: 5000})
		if cfg.BrowserFingerprintTLSMin != 100 || cfg.BrowserFingerprintTLSMax != 999 {
			t.Errorf("区间未钳制: [%d, %d], want [100, 999]", cfg.BrowserFingerprintTLSMin, cfg.BrowserFingerprintTLSMax)
		}
	})

	t.Run("list 模式池过滤非法项", func(t *testing.T) {
		cfg := loadAppCfg(t, AppConfig{
			BrowserFingerprintTLSMode: fingerprint.TLSModeList,
			BrowserFingerprintTLSPool: []string{"chrome_152", "chrome_120", ""},
		})
		if !reflect.DeepEqual(cfg.BrowserFingerprintTLSPool, []string{"chrome_152"}) {
			t.Errorf("池过滤异常: %v", cfg.BrowserFingerprintTLSPool)
		}
	})

	t.Run("locale 白名单归一", func(t *testing.T) {
		if got := loadAppCfg(t, AppConfig{BrowserFingerprintLocale: "bogus"}).BrowserFingerprintLocale; got != fingerprint.DefaultLocale {
			t.Errorf("非法 locale 回退 %q, got %q", fingerprint.DefaultLocale, got)
		}
		if got := loadAppCfg(t, AppConfig{BrowserFingerprintLocale: "en-US"}).BrowserFingerprintLocale; got != "en-US" {
			t.Errorf("合法 locale 应保留, got %q", got)
		}
		if got := loadAppCfg(t, AppConfig{}).BrowserFingerprintLocale; got != fingerprint.DefaultLocale {
			t.Errorf("空 locale 回退默认, got %q", got)
		}
	})

	t.Run("关闭状态不校验 custom（避免误伤待修复数据）", func(t *testing.T) {
		cfg := loadAppCfg(t, AppConfig{BrowserFingerprintEnabled: false, BrowserFingerprintMode: fingerprint.ModeCustom, BrowserFingerprintCustom: `{"tls_profile":"chrome_120"}`})
		if cfg.BrowserFingerprintEnabled {
			t.Error("enabled=false 应保持 false")
		}
		if cfg.BrowserFingerprintCustom != `{"tls_profile":"chrome_120"}` {
			t.Error("custom 原样保留，便于用户修复后重新开启")
		}
	})
}
