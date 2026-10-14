package api

import (
	"net/http"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

// fingerprintHeadersView 把 Bundle 的头集导出为只读展示用 map（键与手动包
// JSON 键一致，前端可直接拿去生成"从预设导入为手动包"的编辑起点）。
func fingerprintHeadersView(b fingerprint.Bundle) map[string]string {
	return map[string]string{
		"user_agent":                  b.UA,
		"sec_ch_ua":                   b.SecCHUA,
		"sec_ch_ua_full_version":      b.SecCHUAFullVersion,
		"sec_ch_ua_full_version_list": b.SecCHUAFullVersionList,
		"sec_ch_ua_platform":          b.SecCHUAPlatform,
		"sec_ch_ua_platform_version":  b.SecCHUAPlatformVersion,
		"sec_ch_ua_arch":              b.SecCHUAArch,
		"sec_ch_ua_bitness":           b.SecCHUABitness,
		"sec_ch_ua_mobile":            b.SecCHUAMobile,
		"sec_ch_ua_model":             b.SecCHUAModel,
		"sec_ch_ua_wow64":             b.SecCHUAWow64,
		"sec_ch_ua_form_factors":      b.SecCHUAFormFactors,
		"x_browser_year":              b.XBrowserYear,
		"accept_language":             b.AcceptLanguage,
		"locale":                      b.Locale,
		"timezone":                    b.Timezone,
		"recaptcha_hl":                b.RecaptchaHl,
	}
}

// adminGetFingerprint 返回浏览器指纹页所需数据：
//   - 当前配置（含 TLS 策略与地区预设）；
//   - 预设清单（后端下发，前端不硬编码预设内容，杜绝两端漂移；预设只读）；
//   - TLS 池解析结果（按当前配置即时计算，供 UI 显示"区间 → 实际池"）；
//   - 当前实际生效的包（enabled/来源/名称/TLS 池/UA），供页面展示"生效预览"——
//     配置开启但手动包非法时 effective.active=false，与运行时一致。
func (adm *AdminHandler) adminGetFingerprint(w http.ResponseWriter, _ *http.Request) {
	presets := fingerprint.Presets()
	presetViews := make([]map[string]any, 0, len(presets))
	for i := range presets {
		p := presets[i]
		presetViews = append(presetViews, map[string]any{
			"name":     p.Name,
			"verified": p.Verified,
			"tls_pool": p.TLSPool,
			"headers":  fingerprintHeadersView(p),
		})
	}

	policy := fingerprint.TLSPolicy{
		Mode:   adm.cfg.BrowserFingerprintTLSMode(),
		Single: adm.cfg.BrowserFingerprintTLSProfile(),
		Min:    adm.cfg.BrowserFingerprintTLSMin(),
		Max:    adm.cfg.BrowserFingerprintTLSMax(),
		Pool:   adm.cfg.BrowserFingerprintTLSPool(),
	}
	presetTLS := "chrome_150"
	if fingerprint.IsPreset(adm.cfg.BrowserFingerprintPreset()) {
		presetTLS = fingerprint.PresetByName(adm.cfg.BrowserFingerprintPreset()).TLSPool[0]
	}
	resolvedPool := fingerprint.ResolveTLSPool(policy, presetTLS)

	effective := map[string]any{"active": false, "source": "legacy"}
	if b := fingerprint.Active(); b != nil {
		source := "preset"
		if b.Name == "custom" {
			source = "custom"
		}
		effective = map[string]any{
			"active":   true,
			"source":   source,
			"name":     b.Name,
			"tls_pool": b.TLSPool,
			"ua":       b.UA,
			"verified": b.Verified,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": adm.cfg.BrowserFingerprintEnabled(),
		"mode":    adm.cfg.BrowserFingerprintMode(),
		"preset":  adm.cfg.BrowserFingerprintPreset(),
		"custom":  adm.cfg.BrowserFingerprintCustom(),
		"tls": map[string]any{
			"mode":          policy.Mode,
			"profile":       policy.Single,
			"min":           policy.Min,
			"max":           policy.Max,
			"pool":          policy.Pool,
			"resolved_pool": resolvedPool,
		},
		"locale":       adm.cfg.BrowserFingerprintLocale(),
		"locales":      fingerprint.LocalePresetNames(),
		"tls_profiles": fingerprint.TLSProfileNames(),
		"presets":      presetViews,
		"effective":    effective,
	})
}

// FingerprintSelection 从 provider 快照组装指纹生效计算的输入。
// 导出供 main 启动装配与 ApplyRuntimeHotSettings 热更新共用同一实现。
func FingerprintSelection(cfg config.ConfigProvider) fingerprint.Selection {
	return fingerprint.Selection{
		Enabled: cfg.BrowserFingerprintEnabled(),
		Mode:    cfg.BrowserFingerprintMode(),
		Preset:  cfg.BrowserFingerprintPreset(),
		Custom:  cfg.BrowserFingerprintCustom(),
		TLS: fingerprint.TLSPolicy{
			Mode:   cfg.BrowserFingerprintTLSMode(),
			Single: cfg.BrowserFingerprintTLSProfile(),
			Min:    cfg.BrowserFingerprintTLSMin(),
			Max:    cfg.BrowserFingerprintTLSMax(),
			Pool:   cfg.BrowserFingerprintTLSPool(),
		},
	}
}
