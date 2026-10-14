// Package fingerprint 集中管理"浏览器指纹包"：上游请求的 TLS ClientHello
// （tls-client profile）与 HTTP 请求头（UA / sec-ch-ua / x-browser-*）是两个
// 独立维度，可任意组合——TLS 由 TLSPolicy 决定，请求头由 Bundle 决定。
//
// 本包是纯数据 + 解析层，不依赖 config / transport（二者都引用本包以规避环）。
// 预设是代码常量，只读，无任何写路径；手动导入包存储在配置的独立键中，
// 与预设物理隔离，任何修改都不可能覆盖预设。
package fingerprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/bogdanfinn/tls-client/profiles"
)

// 请求头（Bundle）来源模式：预设（只读）或手动导入包（可编辑）。
const (
	ModePreset = "preset"
	ModeCustom = "custom"
)

// TLS 策略模式：TLS 与请求头解耦，各自独立选择。
const (
	// TLSModeFollow 跟随所选预设的 TLS（默认，等价于"预设成套切换"）。
	TLSModeFollow = "follow"
	// TLSModeLegacy 复现默认行为：Chrome 144/146 随机，与请求头选择无关。
	TLSModeLegacy = "legacy"
	// TLSModeSingle 单值：固定用 TLSProfile 指定的一个 profile。
	TLSModeSingle = "single"
	// TLSModeRange 版本区间：取 [Min, Max] 内所有可用 profile 成随机池。
	TLSModeRange = "range"
	// TLSModeList 显式列表：Pool 中列出的 profile 成随机池。
	TLSModeList = "list"
)

const (
	// DefaultPreset 是预设模式的默认预设（chrome_150 头集经实测 PoC 验证）。
	DefaultPreset = "chrome_150"
	// DefaultTLSMode 默认 TLS 策略：跟随预设，行为与"预设成套切换"一致。
	DefaultTLSMode = TLSModeFollow
	// DefaultLocale 默认地区语言预设。
	DefaultLocale = "zh-CN"
)

// maxCustomJSONBytes 限制手动导入包体积，防御性上限（实际头集远小于此）。
const maxCustomJSONBytes = 16 << 10

// legacyTLSPool 是 TLSModeLegacy 的池：复现开关关闭时的既有行为。
var legacyTLSPool = []string{"chrome_144", "chrome_146"}

// TLSPolicy 决定 TLS ClientHello 的选择方式，与请求头来源完全独立。
// 两者版本不一致（如 TLS 144 + UA 150）是合法组合，但会提高被检出概率。
type TLSPolicy struct {
	Mode   string   // follow|legacy|single|range|list
	Single string   // single 模式的 profile 名
	Min    int      // range 下界（含）
	Max    int      // range 上界（含）
	Pool   []string // list 模式的 profile 名
}

// Bundle 是一个完整的浏览器指纹包（请求头侧）。字段值均为"上线后逐字节原样
// 发送"的 wire 值（sec-ch-ua 系列本身含双引号）；x-browser-validation 不入包，
// 始终由 UA 现算（见 transport.GenerateXBrowserValidation），保证自洽。
type Bundle struct {
	Name string // 标识：预设名或 "custom"

	// 版本类：与浏览器主版本强绑定，切换时须整体切换。
	UA                     string
	SecCHUA                string
	SecCHUAFullVersion     string
	SecCHUAFullVersionList string
	XBrowserYear           string

	// 平台描述类：与浏览器主版本无关，但与 SecCHUAPlatform 必须自洽
	// （改 platform 不改 arch/bitness 会自相矛盾）。
	SecCHUAPlatform        string
	SecCHUAPlatformVersion string
	SecCHUAArch            string
	SecCHUABitness         string
	SecCHUAMobile          string
	SecCHUAModel           string
	SecCHUAWow64           string
	SecCHUAFormFactors     string

	// 地区语言类：四处消费点（Accept-Language、recaptcha hl、payload
	// Locale/Timezone）同源，避免各写各的导致自相矛盾。
	AcceptLanguage string
	Locale         string
	Timezone       string
	RecaptchaHl    string

	// TLSPool 是本包生效时的 TLS 随机池；空表示由调用方按 follow 语义处理。
	TLSPool  []string
	Verified bool // 头集是否经实测 PoC 验证（当前仅 chrome_150）
}

// tlsClientProfiles 是允许被 TLS 策略引用的 tls-client profile 清单（v1.16.0）。
// 不接受任意 ClientHello——自定义 ClientHelloSpec 需要手工拼全部扩展，
// 极易拼错被 JA3/JA4 检出，因此只开放官方预设 profile 的选择。
//
//nolint:gochecknoglobals // Constant-like profile registry
var tlsClientProfiles = map[string]profiles.ClientProfile{
	"chrome_133": profiles.Chrome_133,
	"chrome_144": profiles.Chrome_144,
	"chrome_146": profiles.Chrome_146,
	"chrome_150": profiles.Chrome_150,
	"chrome_152": profiles.Chrome_152,
}

// TLSProfileNames 返回允许的 tls-client profile 名（按版本号升序），供校验提示与 UI。
func TLSProfileNames() []string {
	return []string{"chrome_133", "chrome_144", "chrome_146", "chrome_150", "chrome_152"}
}

// IsTLSProfile 报告 name 是否为允许的 tls-client profile。
func IsTLSProfile(name string) bool { _, ok := tlsClientProfiles[name]; return ok }

// versionOf 从 profile 名提取主版本号（chrome_146 → 146），非数字时返回 false。
func versionOf(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "chrome_")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ResolveTLSPool 把 TLS 策略解析为实际随机池（已过滤非法项、去重、按版本升序）。
// presetTLS 是所选预设自带的 TLS profile，供 follow 模式使用。
// 任何模式下解析结果为空都回退为 [presetTLS]（宁可跟随预设，不留空池）。
func ResolveTLSPool(p TLSPolicy, presetTLS string) []string {
	switch p.Mode {
	case TLSModeLegacy:
		return append([]string(nil), legacyTLSPool...)
	case TLSModeSingle:
		if IsTLSProfile(p.Single) {
			return []string{p.Single}
		}
	case TLSModeRange:
		min, max := p.Min, p.Max
		if min > max {
			min, max = max, min
		}
		pool := make([]string, 0, len(tlsClientProfiles))
		for _, name := range TLSProfileNames() {
			v, ok := versionOf(name)
			if ok && v >= min && v <= max {
				pool = append(pool, name)
			}
		}
		if len(pool) > 0 {
			return pool
		}
	case TLSModeList:
		seen := make(map[string]bool, len(p.Pool))
		pool := make([]string, 0, len(p.Pool))
		for _, name := range p.Pool {
			if IsTLSProfile(name) {
				seen[name] = true
			}
		}
		for _, name := range TLSProfileNames() { // 按升序输出，便于 UI 稳定展示
			if seen[name] {
				pool = append(pool, name)
			}
		}
		if len(pool) > 0 {
			return pool
		}
	}
	// follow 与所有"解析为空"的兜底。
	if IsTLSProfile(presetTLS) {
		return []string{presetTLS}
	}
	return []string{DefaultPreset}
}

// Pick 按本包的 TLS 随机池随机返回一个 ClientProfile。池为空时回退默认预设。
// 全局 math/rand 源已加锁、并发安全（与 recaptcha 随机串同风格）。
func (b Bundle) Pick() profiles.ClientProfile {
	pool := b.TLSPool
	if len(pool) == 0 {
		return tlsClientProfiles[DefaultPreset]
	}
	return tlsClientProfiles[pool[rand.Intn(len(pool))]] //nolint:gosec // 指纹随机不需要密码学强度
}

// LocalePreset 是地区语言预设：四处消费点同源的一组值。
type LocalePreset struct {
	Name           string
	AcceptLanguage string
	Locale         string
	Timezone       string
	RecaptchaHl    string
}

//nolint:gochecknoglobals // Constant-like presets
var localePresets = []LocalePreset{
	{
		Name:           "zh-CN",
		AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
		Locale:         "zh_CN",
		Timezone:       "Asia/Hong_Kong",
		RecaptchaHl:    "zh-CN",
	},
	{
		Name:           "en-US",
		AcceptLanguage: "en-US,en;q=0.9",
		Locale:         "en_US",
		Timezone:       "America/Los_Angeles",
		RecaptchaHl:    "en",
	},
	{
		Name:           "ja-JP",
		AcceptLanguage: "ja,en-US;q=0.9,en;q=0.8",
		Locale:         "ja_JP",
		Timezone:       "Asia/Tokyo",
		RecaptchaHl:    "ja",
	},
}

// LocalePresetNames 返回可用地区预设名（稳定顺序）。
func LocalePresetNames() []string {
	out := make([]string, 0, len(localePresets))
	for _, p := range localePresets {
		out = append(out, p.Name)
	}
	return out
}

// Locale 按名返回地区预设；未命中返回默认（zh-CN）。
func Locale(name string) LocalePreset {
	for _, p := range localePresets {
		if p.Name == name {
			return p
		}
	}
	return localePresets[0]
}

// IsLocale 报告 name 是否为可用地区预设。
func IsLocale(name string) bool {
	for _, p := range localePresets {
		if p.Name == name {
			return true
		}
	}
	return false
}

// baselineLocale 是所有预设与手动包的地区基线（zh-CN）。
var baselineLocale = localePresets[0]

// presetChrome150 的头集逐字节抄自实测 PoC（recaptcha reload 与 batchGraphql
// 均以此通过 Google 匿名端点校验），是唯一 Verified=true 的预设。
//
//nolint:gochecknoglobals // Constant-like preset
var presetChrome150 = Bundle{
	Name:                   "chrome_150",
	UA:                     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
	SecCHUA:                `"Not;A=Brand";v="8", "Chromium";v="150", "Google Chrome";v="150"`,
	SecCHUAFullVersion:     `"150.0.7871.13"`,
	SecCHUAFullVersionList: `"Not;A=Brand";v="8.0.0.0", "Chromium";v="150.0.7871.13", "Google Chrome";v="150.0.7871.13"`,
	XBrowserYear:           "2026",

	SecCHUAPlatform:        `"Windows"`,
	SecCHUAPlatformVersion: `"19.0.0"`,
	SecCHUAArch:            `"x86"`,
	SecCHUABitness:         `"64"`,
	SecCHUAMobile:          `?0`,
	SecCHUAModel:           `""`,
	SecCHUAWow64:           `?0`,
	SecCHUAFormFactors:     `"Desktop"`,

	AcceptLanguage: baselineLocale.AcceptLanguage,
	Locale:         baselineLocale.Locale,
	Timezone:       baselineLocale.Timezone,
	RecaptchaHl:    baselineLocale.RecaptchaHl,

	TLSPool:  []string{"chrome_150"},
	Verified: true,
}

// derivePreset 以 chrome_150 实测集为基线按主版本号替换，派生其余预设。
// 其他版本的完整 build 号无法凭空推断，派生值以 "<major>.0.0.0" 占位——仅保证
// 格式自洽，供测试对比 TLS/请求头版本错配的影响；Verified 恒为 false。
func derivePreset(name string) Bundle {
	major := strings.TrimPrefix(name, "chrome_")
	full := `"` + major + `.0.0.0"`
	b := presetChrome150
	b.Name = name
	b.UA = strings.Replace(presetChrome150.UA, "Chrome/150.0.0.0", "Chrome/"+major+".0.0.0", 1)
	b.SecCHUA = strings.Replace(presetChrome150.SecCHUA, `"150"`, `"`+major+`"`, -1)
	b.SecCHUAFullVersion = full
	b.SecCHUAFullVersionList = strings.Replace(presetChrome150.SecCHUAFullVersionList, "150.0.7871.13", major+".0.0.0", -1)
	b.Verified = false
	b.TLSPool = []string{name}
	return b
}

//nolint:gochecknoglobals // Constant-like presets
var presets = []Bundle{
	presetChrome150,
	derivePreset("chrome_152"),
	derivePreset("chrome_146"),
}

// Presets 返回全部预设（只读副本，按稳定顺序）。预设内容不可变、不可写——
// 这是"预设不允许被修改"的机制保证（无任何写路径，配置写入口也不接受预设键）。
func Presets() []Bundle {
	out := make([]Bundle, len(presets))
	copy(out, presets)
	return out
}

// IsPreset 报告 name 是否为内置预设名。
func IsPreset(name string) bool {
	for i := range presets {
		if presets[i].Name == name {
			return true
		}
	}
	return false
}

// PresetByName 返回指定预设的副本；未命中时返回默认预设（调用方可先用 IsPreset 判定）。
func PresetByName(name string) Bundle {
	for i := range presets {
		if presets[i].Name == name {
			return presets[i]
		}
	}
	return presets[0]
}

// Legacy 返回开关关闭时的请求头基线：与既有行为逐字节一致的 Chrome 150 头集。
// 与 chrome_150 预设共享同一组值（单一事实来源），但 TLS 语义不同——关闭态由
// transport 侧维持 legacyTLSPool 随机，不消费本包的 TLSPool 字段。
func Legacy() Bundle { return presetChrome150 }

// customOverlayFields 字段清单：手动包是稀疏覆盖层，未提供的字段继承基线。
// 注意 tls_profile 已不在其中——TLS 迁到独立策略，见 ParseCustom 的迁移提示。
var customOverlayFields = []string{
	"user_agent", "sec_ch_ua", "sec_ch_ua_full_version", "sec_ch_ua_full_version_list",
	"sec_ch_ua_platform", "sec_ch_ua_platform_version", "sec_ch_ua_arch", "sec_ch_ua_bitness",
	"sec_ch_ua_mobile", "sec_ch_ua_model", "sec_ch_ua_wow64", "sec_ch_ua_form_factors",
	"x_browser_year", "accept_language", "locale", "timezone", "recaptcha_hl",
}

// errTLSProfileMigrated 是手动包中出现 tls_profile 时的迁移提示：
// TLS 已迁到独立的 TLS 策略，双源会冲突，必须显式移除而非静默忽略。
const errTLSProfileMigrated = "tls_profile 已从手动包移除：TLS 指纹改为「TLS 策略」独立配置，请从本包删除该键（旧配置若含此键，浏览器指纹将整体回退为未启用）"

// ParseCustom 解析并校验手动导入包（JSON 对象，稀疏覆盖 chrome_150 基线）：
//   - 未知字段一律拒绝（防止拼错键名后静默失效）；
//   - tls_profile 显式拒绝并给出迁移提示（见 errTLSProfileMigrated）；
//   - x-browser-validation 由 UA 现算，不接受导入。
func ParseCustom(raw string) (Bundle, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Bundle{}, errors.New("手动导入包为空")
	}
	if len(raw) > maxCustomJSONBytes {
		return Bundle{}, fmt.Errorf("手动导入包过大（%d 字节，上限 %d）", len(raw), maxCustomJSONBytes)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return Bundle{}, fmt.Errorf("JSON 解析失败: %w", err)
	}
	out := presetChrome150
	out.Name = "custom"
	out.Verified = false
	for k, v := range fields {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return Bundle{}, fmt.Errorf("字段 %s 需为字符串", k)
		}
		s = strings.TrimSpace(s)
		switch k {
		case "user_agent":
			out.UA = s
		case "sec_ch_ua":
			out.SecCHUA = s
		case "sec_ch_ua_full_version":
			out.SecCHUAFullVersion = s
		case "sec_ch_ua_full_version_list":
			out.SecCHUAFullVersionList = s
		case "sec_ch_ua_platform":
			out.SecCHUAPlatform = s
		case "sec_ch_ua_platform_version":
			out.SecCHUAPlatformVersion = s
		case "sec_ch_ua_arch":
			out.SecCHUAArch = s
		case "sec_ch_ua_bitness":
			out.SecCHUABitness = s
		case "sec_ch_ua_mobile":
			out.SecCHUAMobile = s
		case "sec_ch_ua_model":
			out.SecCHUAModel = s
		case "sec_ch_ua_wow64":
			out.SecCHUAWow64 = s
		case "sec_ch_ua_form_factors":
			out.SecCHUAFormFactors = s
		case "x_browser_year":
			out.XBrowserYear = s
		case "accept_language":
			out.AcceptLanguage = s
		case "locale":
			out.Locale = s
		case "timezone":
			out.Timezone = s
		case "recaptcha_hl":
			out.RecaptchaHl = s
		case "tls_profile":
			return Bundle{}, errors.New(errTLSProfileMigrated)
		default:
			return Bundle{}, fmt.Errorf("未知字段 %q（允许: %s）", k, strings.Join(customOverlayFields, ", "))
		}
	}
	return out, nil
}

// Selection 是一次"生效计算"的输入：请求头来源与 TLS 策略相互独立。
type Selection struct {
	Enabled bool
	Mode    string    // 请求头来源：preset | custom
	Preset  string    // 预设名
	Custom  string    // 手动包 JSON
	TLS     TLSPolicy // TLS 策略（与请求头独立）
}

// active 当前生效的指纹包；nil 表示"既有行为"（legacyTLS 池随机 + Chrome 150 头）。
//
//nolint:gochecknoglobals // Process-wide active bundle singleton
var active atomic.Pointer[Bundle]

// Active 返回当前生效包；nil = 未启用（调用方回退既有行为）。
func Active() *Bundle { return active.Load() }

// ActiveOrLegacy 返回当前生效包；未启用时返回 Legacy 基线。
// 供"未启用也需取值"的消费点（payload 地区字段、recaptcha hl）使用，
// 保证关闭态取到的值与既有硬编码逐字节一致。
func ActiveOrLegacy() Bundle {
	if b := active.Load(); b != nil {
		return *b
	}
	return Legacy()
}

// Apply 把配置解析为当前生效包并发布。enabled=false 或手动包非法时发布 nil
// （= 既有行为），绝不半生效——半生效意味着 TLS 与请求头可能各用一半版本。
// 返回是否生效与失败原因，供调用方告警。
func Apply(sel Selection) (bool, error) {
	if !sel.Enabled {
		active.Store(nil)
		return false, nil
	}
	if sel.Mode == ModeCustom {
		b, err := ParseCustom(sel.Custom)
		if err != nil {
			active.Store(nil)
			return false, err
		}
		b.TLSPool = ResolveTLSPool(sel.TLS, DefaultPreset)
		active.Store(&b)
		return true, nil
	}
	// ModePreset 及未知值：preset 非法时回退默认预设（与 config.Load 归一口径一致）。
	name := sel.Preset
	if !IsPreset(name) {
		name = DefaultPreset
	}
	b := PresetByName(name)
	// TLS 与请求头解耦：池由策略决定，follow 时取预设自带值。
	b.TLSPool = ResolveTLSPool(sel.TLS, b.TLSPool[0])
	active.Store(&b)
	return true, nil
}
