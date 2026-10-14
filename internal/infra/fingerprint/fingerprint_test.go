package fingerprint

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdanfinn/tls-client/profiles"
)

// majorOf 从 UA 中提取 Chrome 主版本号，供自洽性断言。
func majorOf(ua string) string {
	i := strings.Index(ua, "Chrome/")
	if i < 0 {
		return ""
	}
	rest := ua[i+len("Chrome/"):]
	if j := strings.Index(rest, "."); j >= 0 {
		return rest[:j]
	}
	return rest
}

// sameProfile 比较 ClientProfile：含函数字段不可 ==，改用 fmt 渲染指纹
// （同一 profile 变量的函数值地址相同，渲染结果一致；map 字段按序输出稳定）。
func sameProfile(a, b profiles.ClientProfile) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func TestPresets_Registry(t *testing.T) {
	ps := Presets()
	if len(ps) != 3 {
		t.Fatalf("预设数量=%d, want 3", len(ps))
	}
	if ps[0].Name != DefaultPreset {
		t.Fatalf("首个预设应为默认 %q, got %q", DefaultPreset, ps[0].Name)
	}
	for _, p := range ps {
		if !IsPreset(p.Name) {
			t.Errorf("IsPreset(%q)=false", p.Name)
		}
		// 自洽性：UA 主版本与预设名一致，sec-ch-ua 引用同一主版本。
		if got := majorOf(p.UA); got != strings.TrimPrefix(p.Name, "chrome_") {
			t.Errorf("预设 %q UA 主版本=%q, 不一致", p.Name, got)
		}
		if !strings.Contains(p.SecCHUA, `"`+strings.TrimPrefix(p.Name, "chrome_")+`"`) {
			t.Errorf("预设 %q sec-ch-ua 未引用自身主版本: %s", p.Name, p.SecCHUA)
		}
		if p.UA == "" || p.SecCHUAFullVersion == "" || p.SecCHUAFullVersionList == "" || p.XBrowserYear == "" {
			t.Errorf("预设 %q 头集字段缺失: %+v", p.Name, p)
		}
		// TLS 池：预设自带同名单值池。
		if len(p.TLSPool) != 1 || p.TLSPool[0] != p.Name {
			t.Errorf("预设 %q TLSPool=%v, want [%s]", p.Name, p.TLSPool, p.Name)
		}
		// 平台描述与地区字段全部非空（自洽基线）。
		for name, v := range map[string]string{
			"SecCHUAPlatform":    p.SecCHUAPlatform,
			"SecCHUAArch":        p.SecCHUAArch,
			"SecCHUABitness":     p.SecCHUABitness,
			"SecCHUAMobile":      p.SecCHUAMobile,
			"SecCHUAModel":       p.SecCHUAModel,
			"SecCHUAWow64":       p.SecCHUAWow64,
			"SecCHUAFormFactors": p.SecCHUAFormFactors,
			"AcceptLanguage":     p.AcceptLanguage,
			"Locale":             p.Locale,
			"Timezone":           p.Timezone,
			"RecaptchaHl":        p.RecaptchaHl,
		} {
			if v == "" {
				t.Errorf("预设 %q 缺少 %s", p.Name, name)
			}
		}
	}
	if !presets[0].Verified {
		t.Errorf("chrome_150 应标注 Verified=true（唯一实测头集）")
	}
	for _, p := range ps[1:] {
		if p.Verified {
			t.Errorf("派生预设 %q 不应标注 Verified", p.Name)
		}
	}
	// Presets 返回副本：调用方修改不影响内部注册表。
	ps[0].UA = "tampered"
	if Presets()[0].UA == "tampered" {
		t.Errorf("Presets() 应返回副本，防止外部改写预设")
	}
}

func TestResolveTLSPool(t *testing.T) {
	cases := []struct {
		name      string
		policy    TLSPolicy
		presetTLS string
		want      []string
	}{
		{"follow 用预设自带 TLS", TLSPolicy{Mode: TLSModeFollow}, "chrome_152", []string{"chrome_152"}},
		{"follow 预设非法回退默认", TLSPolicy{Mode: TLSModeFollow}, "chrome_999", []string{DefaultPreset}},
		{"legacy 复现默认池", TLSPolicy{Mode: TLSModeLegacy}, "chrome_150", []string{"chrome_144", "chrome_146"}},
		{"single 合法", TLSPolicy{Mode: TLSModeSingle, Single: "chrome_133"}, "chrome_150", []string{"chrome_133"}},
		{"single 非法回退 follow", TLSPolicy{Mode: TLSModeSingle, Single: "chrome_120"}, "chrome_146", []string{"chrome_146"}},
		{"range 命中区间", TLSPolicy{Mode: TLSModeRange, Min: 144, Max: 152}, "chrome_150",
			[]string{"chrome_144", "chrome_146", "chrome_150", "chrome_152"}},
		{"range 上下界交换", TLSPolicy{Mode: TLSModeRange, Min: 146, Max: 144}, "chrome_150",
			[]string{"chrome_144", "chrome_146"}},
		{"range 越界只留命中", TLSPolicy{Mode: TLSModeRange, Min: 144, Max: 145}, "chrome_150", []string{"chrome_144"}},
		{"range 空命中回退 follow", TLSPolicy{Mode: TLSModeRange, Min: 700, Max: 800}, "chrome_150", []string{"chrome_150"}},
		{"list 过滤非法项并升序", TLSPolicy{Mode: TLSModeList, Pool: []string{"chrome_152", "chrome_120", "chrome_133", ""}},
			"chrome_150", []string{"chrome_133", "chrome_152"}},
		{"list 全非法回退 follow", TLSPolicy{Mode: TLSModeList, Pool: []string{"chrome_120"}}, "chrome_146", []string{"chrome_146"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveTLSPool(c.policy, c.presetTLS)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestBundlePick_StaysInPool(t *testing.T) {
	pool := []string{"chrome_133", "chrome_146", "chrome_152"}
	b := presetChrome150
	b.TLSPool = pool
	seen := map[string]bool{}
	for range 200 {
		p := b.Pick()
		hit := false
		for _, name := range pool {
			if sameProfile(p, tlsClientProfiles[name]) {
				hit = true
				seen[name] = true
			}
		}
		if !hit {
			t.Fatalf("Pick() 返回了池外 profile")
		}
	}
	if len(seen) != len(pool) {
		t.Errorf("200 次采样应覆盖全池, 只命中 %v", seen)
	}
	// 空池回退默认预设。
	empty := presetChrome150
	empty.TLSPool = nil
	if !sameProfile(empty.Pick(), tlsClientProfiles[DefaultPreset]) {
		t.Error("空池 Pick 应回退默认预设")
	}
}

func TestParseCustom(t *testing.T) {
	t.Run("空/超长拒绝", func(t *testing.T) {
		if _, err := ParseCustom(""); err == nil {
			t.Error("空包应报错")
		}
		if _, err := ParseCustom(`{"pad":"` + strings.Repeat("x", maxCustomJSONBytes) + `"}`); err == nil {
			t.Error("超长包应报错")
		}
	})

	t.Run("坏 JSON 与非字符串字段拒绝", func(t *testing.T) {
		if _, err := ParseCustom("{not json}"); err == nil {
			t.Error("坏 JSON 应报错")
		}
		if _, err := ParseCustom(`{"user_agent":150}`); err == nil {
			t.Error("非字符串字段应报错")
		}
	})

	t.Run("未知字段拒绝并列出允许清单", func(t *testing.T) {
		_, err := ParseCustom(`{"user_agent_x":"x"}`)
		if err == nil {
			t.Fatal("未知字段应报错")
		}
		if !strings.Contains(err.Error(), "user_agent_x") || !strings.Contains(err.Error(), "user_agent,") {
			t.Errorf("错误信息应含字段名与允许清单, got %v", err)
		}
	})

	t.Run("tls_profile 显式拒绝并给迁移提示", func(t *testing.T) {
		_, err := ParseCustom(`{"tls_profile":"chrome_150"}`)
		if err == nil {
			t.Fatal("tls_profile 应报错（已迁移到 TLS 策略）")
		}
		if !strings.Contains(err.Error(), "TLS") {
			t.Errorf("错误信息应包含迁移提示, got %v", err)
		}
	})

	t.Run("稀疏覆盖继承基线", func(t *testing.T) {
		b, err := ParseCustom(`{"user_agent":"UA-X"}`)
		if err != nil {
			t.Fatalf("ParseCustom: %v", err)
		}
		if b.UA != "UA-X" {
			t.Errorf("UA=%q, want UA-X", b.UA)
		}
		if b.SecCHUA != presetChrome150.SecCHUA {
			t.Errorf("未覆盖字段应继承 chrome_150 基线")
		}
		if b.Locale != baselineLocale.Locale || b.Timezone != baselineLocale.Timezone {
			t.Errorf("地区字段应继承基线: %+v", b)
		}
		if b.Verified {
			t.Error("手动包不应继承 Verified 标记")
		}
	})

	t.Run("平台与地区字段可覆盖", func(t *testing.T) {
		raw := `{"sec_ch_ua_arch":"\"arm\"","sec_ch_ua_bitness":"\"32\"","accept_language":"en-US,en;q=0.9","locale":"en_US","timezone":"America/Los_Angeles","recaptcha_hl":"en"}`
		b, err := ParseCustom(raw)
		if err != nil {
			t.Fatalf("ParseCustom: %v", err)
		}
		if b.SecCHUAArch != `"arm"` || b.SecCHUABitness != `"32"` {
			t.Errorf("平台字段覆盖异常: arch=%q bitness=%q", b.SecCHUAArch, b.SecCHUABitness)
		}
		if b.AcceptLanguage != "en-US,en;q=0.9" || b.RecaptchaHl != "en" {
			t.Errorf("地区字段覆盖异常: %+v", b)
		}
	})
}

func TestApply_AndActive(t *testing.T) {
	restore := func() {
		_, _ = Apply(Selection{})
	}
	t.Cleanup(restore)

	t.Run("关闭发布 nil（既有行为）", func(t *testing.T) {
		ok, err := Apply(Selection{Enabled: false})
		if err != nil || ok {
			t.Errorf("关闭时应 (false, nil), got (%v, %v)", ok, err)
		}
		if Active() != nil {
			t.Error("关闭时 Active() 应为 nil")
		}
	})

	t.Run("预设模式默认 follow 池=预设 TLS", func(t *testing.T) {
		ok, err := Apply(Selection{Enabled: true, Mode: ModePreset, Preset: "chrome_146"})
		if err != nil || !ok {
			t.Fatalf("Apply: (%v, %v)", ok, err)
		}
		if got := Active(); got == nil || !reflect.DeepEqual(got.TLSPool, []string{"chrome_146"}) {
			t.Errorf("TLSPool=%v, want [chrome_146]", got.TLSPool)
		}
	})

	t.Run("TLS 策略与预设解耦（UA 152 + TLS legacy 池）", func(t *testing.T) {
		ok, err := Apply(Selection{
			Enabled: true, Mode: ModePreset, Preset: "chrome_152",
			TLS: TLSPolicy{Mode: TLSModeLegacy},
		})
		if err != nil || !ok {
			t.Fatalf("Apply: (%v, %v)", ok, err)
		}
		got := Active()
		if got == nil || got.UA != PresetByName("chrome_152").UA {
			t.Fatalf("UA 未取自 chrome_152 预设")
		}
		if !reflect.DeepEqual(got.TLSPool, legacyTLSPool) {
			t.Errorf("TLS 池=%v, want legacy %v（与 UA 版本独立）", got.TLSPool, legacyTLSPool)
		}
	})

	t.Run("TLS 策略 range 模式", func(t *testing.T) {
		ok, _ := Apply(Selection{
			Enabled: true, Mode: ModePreset, Preset: "chrome_150",
			TLS: TLSPolicy{Mode: TLSModeRange, Min: 146, Max: 152},
		})
		if !ok {
			t.Fatal("应生效")
		}
		want := []string{"chrome_146", "chrome_150", "chrome_152"}
		if got := Active(); !reflect.DeepEqual(got.TLSPool, want) {
			t.Errorf("TLS 池=%v, want %v", got.TLSPool, want)
		}
	})

	t.Run("手动包非法发布 nil 且返回错误", func(t *testing.T) {
		ok, err := Apply(Selection{Enabled: true, Mode: ModeCustom, Custom: `{"tls_profile":"chrome_150"}`})
		if ok || err == nil {
			t.Fatalf("应 (false, err), got (%v, %v)", ok, err)
		}
		if Active() != nil {
			t.Error("手动包非法时 Active() 应为 nil（宁可不生效，不半生效）")
		}
	})

	t.Run("手动包合法且 TLS 走策略", func(t *testing.T) {
		ok, err := Apply(Selection{
			Enabled: true, Mode: ModeCustom, Custom: `{"user_agent":"UA-X"}`,
			TLS: TLSPolicy{Mode: TLSModeSingle, Single: "chrome_133"},
		})
		if err != nil || !ok {
			t.Fatalf("Apply: (%v, %v)", ok, err)
		}
		got := Active()
		if got == nil || got.Name != "custom" || got.UA != "UA-X" {
			t.Fatalf("Active()=%+v", got)
		}
		if !reflect.DeepEqual(got.TLSPool, []string{"chrome_133"}) {
			t.Errorf("TLS 池=%v, want [chrome_133]", got.TLSPool)
		}
	})
}

func TestLegacy_MatchesChrome150Baseline(t *testing.T) {
	l := Legacy()
	base := PresetByName(DefaultPreset)
	if !reflect.DeepEqual(l, base) {
		t.Errorf("Legacy() 应与 chrome_150 预设共享同一组头值（单一事实来源）")
	}
	// 关闭态基线：TLS 语义与预设解耦（legacy 池由 transport 侧维护），但头值一致。
	if !reflect.DeepEqual(ActiveOrLegacy(), base) {
		t.Error("未启用时 ActiveOrLegacy 应回返回 Legacy 基线")
	}
}

func TestLocales(t *testing.T) {
	if got := LocalePresetNames(); !reflect.DeepEqual(got, []string{"zh-CN", "en-US", "ja-JP"}) {
		t.Fatalf("地区预设=%v", got)
	}
	if !IsLocale("en-US") || IsLocale("xx-XX") {
		t.Error("IsLocale 判定异常")
	}
	en := Locale("en-US")
	if en.Locale != "en_US" || en.Timezone != "America/Los_Angeles" || en.RecaptchaHl != "en" {
		t.Errorf("en-US 预设值异常: %+v", en)
	}
	// 未命中回退默认。
	if got := Locale("xx-XX"); got.Name != DefaultLocale {
		t.Errorf("未命中应回退 %q, got %q", DefaultLocale, got.Name)
	}
}
