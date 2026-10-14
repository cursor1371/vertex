package transport

import (
	"fmt"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"

	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

// TestParseURI_FingerprintValidation 覆盖导入期非法 uTLS 指纹校验（拒绝方案）：
// 白名单外的 fp 在拨号期必然失败（box.New: unknown uTLS fingerprint），
// 必须在导入期标记 Supported=false，文案含 "not supported"（多处测试硬断言该子串）。
func TestParseURI_FingerprintValidation(t *testing.T) {
	t.Run("非法 fp 标记不支持", func(t *testing.T) {
		for _, uri := range []string{
			"vless://uuid@example.com:443?security=tls&fp=chrome_120",
			"trojan://pw@example.com:443?fp=chrome_120",
			"hysteria2://pw@example.com:443?fp=chrome_120",
			vmessURIFromJSON(t, `{"v":"2","add":"e.com","port":"443","id":"12345678-1234-1234-1234-123456789012","tls":"tls","fp":"chrome_120"}`),
		} {
			n, err := ParseURI(uri)
			if err != nil {
				t.Fatalf("ParseURI(%s): %v", uri, err)
			}
			if n.Supported {
				t.Errorf("fp=chrome_120 应标记不支持, uri=%s", uri)
			}
			if !strings.Contains(n.UnsupportedReason, "not supported") {
				t.Errorf("UnsupportedReason=%q, 应含 not supported", n.UnsupportedReason)
			}
			if !strings.Contains(n.UnsupportedReason, "chrome_120") {
				t.Errorf("UnsupportedReason=%q, 应含非法指纹值", n.UnsupportedReason)
			}
		}
	})

	t.Run("合法 fp 保持支持", func(t *testing.T) {
		for _, fp := range []string{"chrome", "firefox", "random", "randomized", "ios"} {
			n, err := ParseURI("vless://uuid@example.com:443?security=tls&fp=" + fp)
			if err != nil {
				t.Fatalf("ParseURI: %v", err)
			}
			if !n.Supported {
				t.Errorf("fp=%s 应支持, reason=%s", fp, n.UnsupportedReason)
			}
		}
	})

	t.Run("无 fp 与 reality 默认 chrome 均不受影响", func(t *testing.T) {
		n, err := ParseURI("vless://uuid@example.com:443?security=tls")
		if err != nil {
			t.Fatalf("ParseURI: %v", err)
		}
		if !n.Supported || n.TLS.Fingerprint != "" {
			t.Errorf("无 fp 节点应支持且指纹为空, got %+v", n.TLS)
		}
		n, err = ParseURI("vless://uuid@example.com:443?security=reality&pbk=pubkey")
		if err != nil {
			t.Fatalf("ParseURI: %v", err)
		}
		if !n.Supported || n.TLS.Fingerprint != "chrome" {
			t.Errorf("reality 默认 chrome 应支持, got %+v", n.TLS)
		}
	})
}

// hdrValue 取请求头首个值（fhttp 存小写键，Get 会做 MIME 规范化查找而落空，
// 与既有 headers_test 口径一致：直接按存储键访问）。
func hdrValue(h http.Header, key string) string {
	if vs := h[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// TestXHRHeaders_LegacyBaselineAnchor 是升级/改造的回归锚点：指纹开关关闭
// （Active=nil）时，请求头与既有硬编码 Chrome 150 行为逐字节一致。
func TestXHRHeaders_LegacyBaselineAnchor(t *testing.T) {
	if fingerprint.Active() != nil {
		t.Fatalf("测试前提：Active 应为 nil（进程默认未启用）")
	}
	legacy := fingerprint.Legacy()
	h := XHRHeaders("application/json", "*/*", "https://o.example", "https://r.example", "cross-site")
	if got := hdrValue(h, "user-agent"); got != legacy.UA {
		t.Errorf("关闭态 UA 应与 Legacy 基线一致\n got=%q\nwant=%q", got, legacy.UA)
	}
	if got := hdrValue(h, "sec-ch-ua"); got != legacy.SecCHUA {
		t.Errorf("关闭态 sec-ch-ua 应与 Legacy 基线一致, got %q", got)
	}
	if got := hdrValue(h, "x-browser-year"); got != legacy.XBrowserYear {
		t.Errorf("关闭态 x-browser-year=%q, want %q", got, legacy.XBrowserYear)
	}
	// x-browser-validation 始终由 UA 派生（自洽锚点）。
	if got, want := hdrValue(h, "x-browser-validation"), GenerateXBrowserValidation(legacy.UA); got != want {
		t.Errorf("x-browser-validation=%q, want 由 UA 派生的 %q", got, want)
	}
}

// TestXHRHeaders_ActiveBundleSwitch 验证开启指纹开关后请求头成对切换；
// TLS 池与请求头解耦：手动包只改头，TLS 池由策略决定（此处用 legacy 池）。
func TestXHRHeaders_ActiveBundleSwitch(t *testing.T) {
	custom := `{"user_agent":"Mozilla/5.0 TestUA","x_browser_year":"2025","sec_ch_ua_arch":"\"arm\""}`
	ok, err := fingerprint.Apply(fingerprint.Selection{
		Enabled: true, Mode: fingerprint.ModeCustom, Custom: custom,
		TLS: fingerprint.TLSPolicy{Mode: fingerprint.TLSModeLegacy},
	})
	if err != nil || !ok {
		t.Fatalf("Apply: (%v, %v)", ok, err)
	}
	t.Cleanup(func() { _, _ = fingerprint.Apply(fingerprint.Selection{}) })

	h := XHRHeaders("", "*/*", "", "", "")
	if got := hdrValue(h, "user-agent"); got != "Mozilla/5.0 TestUA" {
		t.Errorf("UA 未随手动包切换: %q", got)
	}
	if got := hdrValue(h, "x-browser-year"); got != "2025" {
		t.Errorf("x-browser-year 未随手动包切换: %q", got)
	}
	if got := hdrValue(h, "sec-ch-ua-arch"); got != `"arm"` {
		t.Errorf("平台字段未随手动包切换: %q", got)
	}
	// 未覆盖字段继承 chrome_150 基线。
	if got := hdrValue(h, "sec-ch-ua"); got != fingerprint.Legacy().SecCHUA {
		t.Errorf("未覆盖字段应继承基线, got %q", got)
	}
	// 地区语言同源：Accept-Language 与 FingerprintHL 取同一预设。
	if al, hl := hdrValue(h, "accept-language"), FingerprintHL(); al == "" || hl == "" {
		t.Errorf("地区语言字段缺失: accept-language=%q hl=%q", al, hl)
	}
}

// TestPickProfile_DecoupledFromHeaders 验证 TLS 池与请求头 UA 版本独立：
// UA 取 chrome_152 手动包，TLS 池为 legacy（144/146），pickProfile 应只出 144/146。
func TestPickProfile_DecoupledFromHeaders(t *testing.T) {
	ok, err := fingerprint.Apply(fingerprint.Selection{
		Enabled: true, Mode: fingerprint.ModeCustom, Custom: `{"user_agent":"UA-X"}`,
		TLS: fingerprint.TLSPolicy{Mode: fingerprint.TLSModeLegacy},
	})
	if err != nil || !ok {
		t.Fatalf("Apply: (%v, %v)", ok, err)
	}
	t.Cleanup(func() { _, _ = fingerprint.Apply(fingerprint.Selection{}) })
	for range 20 {
		p := pickProfile()
		if !sameProfile(p, legacyBrowserProfiles[0]) && !sameProfile(p, legacyBrowserProfiles[1]) {
			t.Fatalf("TLS=legacy 池时应只出现 Chrome_144/146, got %+v", p)
		}
	}
}

// sameProfile 比较 ClientProfile：含函数字段不可 ==，改用 fmt 渲染指纹
// （同一 profile 变量的函数值地址相同，渲染结果一致；map 字段按序输出稳定）。
func sameProfile(a, b profiles.ClientProfile) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// TestPickProfile_BundleSwitch 验证 TLS profile 与请求头同源切换。
func TestPickProfile_BundleSwitch(t *testing.T) {
	t.Run("关闭态随机 144/146（既有行为）", func(t *testing.T) {
		if fingerprint.Active() != nil {
			t.Skip("进程已启用指纹包，跳过关闭态断言")
		}
		for range 20 {
			p := pickProfile()
			if !sameProfile(p, legacyBrowserProfiles[0]) && !sameProfile(p, legacyBrowserProfiles[1]) {
				t.Fatalf("关闭态应只出现 Chrome_144/146, got %+v", p)
			}
		}
	})

	t.Run("开启态 follow 用预设指定 profile", func(t *testing.T) {
		ok, err := fingerprint.Apply(fingerprint.Selection{
			Enabled: true, Mode: fingerprint.ModePreset, Preset: "chrome_150",
		})
		if err != nil || !ok {
			t.Fatalf("Apply: (%v, %v)", ok, err)
		}
		t.Cleanup(func() { _, _ = fingerprint.Apply(fingerprint.Selection{}) })
		if got := pickProfile(); !sameProfile(got, fingerprint.Active().Pick()) {
			t.Errorf("开启后 pickProfile 应等于指纹包池内 profile, got %+v", got)
		}
	})

	t.Run("开启态 single 池收敛到单值", func(t *testing.T) {
		ok, err := fingerprint.Apply(fingerprint.Selection{
			Enabled: true, Mode: fingerprint.ModePreset, Preset: "chrome_150",
			TLS: fingerprint.TLSPolicy{Mode: fingerprint.TLSModeSingle, Single: "chrome_133"},
		})
		if err != nil || !ok {
			t.Fatalf("Apply: (%v, %v)", ok, err)
		}
		t.Cleanup(func() { _, _ = fingerprint.Apply(fingerprint.Selection{}) })
		for range 20 {
			if got := pickProfile(); !sameProfile(got, tlsClientProfileByName("chrome_133")) {
				t.Fatalf("single 池应恒为 chrome_133, got %+v", got)
			}
		}
	})
}

// tlsClientProfileByName 在 transport 侧按名取 profile（fingerprint 未导出注册表）。
func tlsClientProfileByName(name string) profiles.ClientProfile {
	switch name {
	case "chrome_133":
		return profiles.Chrome_133
	case "chrome_144":
		return profiles.Chrome_144
	case "chrome_146":
		return profiles.Chrome_146
	case "chrome_150":
		return profiles.Chrome_150
	case "chrome_152":
		return profiles.Chrome_152
	}
	return profiles.DefaultClientProfile
}
