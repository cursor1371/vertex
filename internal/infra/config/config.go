package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

const (
	defaultAnonAPIKey          = "AIzaSyCI-zsRP85UVOi0DjtiCwWBwQ1djDy741g"
	defaultCountTokensQuerySig = "2/mENOSldfC+HZM+tGhVuJLrl8M6gEyK3HRjUKuA5AM58="
)

type AppConfig struct { //nolint:govet
	PortAPI                   int               `json:"port_api"`
	MaxRetries                int               `json:"max_retries"`
	AdminPassword             string            `json:"admin_password"`
	AggregateStream           bool              `json:"aggregate_stream"`
	DropMaxTokens             bool              `json:"drop_max_tokens"`
	SafetySettings            map[string]string `json:"safety_settings"`
	VertexAPIKey              string            `json:"vertex_api_key"`
	CountTokensQuerySignature string            `json:"count_tokens_query_signature"`
	MaxN                      int               `json:"max_n"`
	MaxSpillMB                int               `json:"max_spill_mb"`
	MaxRequestMB              int               `json:"max_request_mb"`
	RequestTimeoutSeconds     int               `json:"request_timeout_seconds"`
	// RequestTotalTimeoutSeconds 是单请求总时长上限（秒，0=不限，默认 0）。
	// request_timeout_seconds 只约束单个竞速候选；长流式下补位候选接续发射，
	// 请求整体可能远超单候选时限。本字段为整个请求生命周期（含竞速与流式收包）
	// 的总闸：到点中止并按截断交付。对齐 vertex-streaming-anti-truncation 的
	// UPSTREAM_TIMEOUT_MS 语义。默认 0 保持既有行为。
	RequestTotalTimeoutSeconds int `json:"request_total_timeout_seconds"`

	// 并发池与节点锁定配置
	ActiveNodeURI       string `json:"active_node_uri"`
	ParallelPoolEnabled bool   `json:"parallel_pool_enabled"`
	ParallelPoolSize    int    `json:"parallel_pool_size"`
	// RTCompensationMs 是 rT 第一发补偿的静置间隔（毫秒，默认 500）。
	// 上游对每个真实请求的第一发必以 "Failed to verify action" 拒绝，须以同一 token
	// 立即重发第二发；实测等待时长与成功率无关，0 表示跳过静置立即重发。
	RTCompensationMs int `json:"rt_compensation_ms"`
	// rT token 预取池（默认关）：后台按消费节奏预抓 reCAPTCHA token，请求命中即取用，
	// 把 0.5~2 秒的抓取耗时从关键路径上摘掉。命中失败一律回退既有实时抓取路径。
	RecaptchaTokenPrefetch bool `json:"recaptcha_token_prefetch"`
	// RecaptchaPrefetchDepth 是池中常备 token 数（默认 5）。热 token 候选必然先到达，
	// 故深度取并发数的三分之一左右即可吃到绝大部分延迟收益。
	RecaptchaPrefetchDepth int `json:"recaptcha_prefetch_depth"`
	// RecaptchaPrefetchTTLSec 是池内 token 的最长存活秒数（默认 90）。Google 侧 token
	// 硬上限 2 分钟且过期后报 timeout-or-duplicate（不落在第一发补偿的识别范围内），
	// 故必须留足余量，上限钳到 110 秒。
	RecaptchaPrefetchTTLSec int `json:"recaptcha_prefetch_ttl_sec"`
	// RecaptchaPrefetchAdaptive 是预取池的"无收益自停"开关（默认开）：池内曾补满、
	// 随后出现过期 token（说明这池预取根本没被消费）即暂停补货，直到新的连接请求到达
	// 才立刻恢复，避免稀疏流量下每来一次请求就白抓一池 token。关闭即退回"有请求就补满"。
	RecaptchaPrefetchAdaptive bool `json:"recaptcha_prefetch_adaptive"`
	// MaxConcurrent 是入站请求的全局并发闸门（0=不限，默认 64）。
	// 超过该值的并发入站请求返回 429+Retry-After，压制 N×parallel_pool_size 的上游放大。
	MaxConcurrent int  `json:"max_concurrent"`
	DebugPprof    bool `json:"debug_pprof"`
	DebugMode     bool `json:"debug_mode"`
	// AdminAPILogEnabled 控制 admin 控制台每个请求一行的 stdout 日志（默认关）：
	// 请求监控等页面的 3 秒级轮询会把进程日志刷成噪声，排查 admin 侧问题时再开。
	// /stats/active 与 /stats/rt-token 两个轮询端点无论开关都不打（admin_handler 跳过名单）。
	AdminAPILogEnabled      bool     `json:"admin_api_log_enabled"`
	TrailingModelFixEnabled bool     `json:"trailing_model_fix_enabled"`
	TrailingFixModels       []string `json:"trailing_fix_models,omitempty"`
	// RecaptchaTryEntryOrDirect 是"优先尝试前置/直连抓取 rT"开关（默认关）。
	// 关闭时直接走出口候选节点，仅在候选全部不可用且未开 RecaptchaNoDirectFetch 时
	// 兜底直连；前置代理与真·直连在代码里同属 proxyURI=="" 一档，无法区分。
	RecaptchaTryEntryOrDirect bool `json:"recaptcha_try_entry_or_direct"`
	// RecaptchaNoDirectFetch 是 rT 抓取的硬开关（默认关）：开启后只走出口候选节点，
	// 「入口/直连」整条路径一律不尝试（含前置代理——该路径在代码里与真·直连同属
	// proxyURI=="" 一档，无法区分，故一并禁止），并覆盖 recaptcha_try_entry_or_direct。
	// 没有可用出口候选时 rT 抓取直接失败（候选以 infra 错误出局），不再静默降级直连。
	RecaptchaNoDirectFetch bool `json:"recaptcha_no_direct_fetch"`
	// RecaptchaPrefetchBudgetSec 是后台补货单次抓取的超时预算（秒，默认 15，与请求路径一致）。
	// 开启「禁止直连抓 rT」后抓取只能走出口候选（每候选最多 3 次尝试），15s 常连一个候选都
	// 跑不完，可适当上调（如 45）。后台无人在等，调大不影响请求延迟。范围 5~180。
	RecaptchaPrefetchBudgetSec int `json:"recaptcha_prefetch_budget_sec"`
	// RecaptchaLiveBudgetSec 是请求路径（实时抓取）单轮 rT 抓取的预算（秒，默认 15）。
	// 注意它在**关键路径**上：prepareCandidate 会等这次抓取，故调大多少就等于客户端最坏
	// 多等多久；且实际生效值 = min(候选剩余时限, 该值)，晚发的补位候选仍可能用不到。
	// 调大不解决上游慢，反而可能因并发抓取持续时间变长而加剧限流。范围 5~120。
	RecaptchaLiveBudgetSec int `json:"recaptcha_live_budget_sec"`

	// 外观配置
	BackgroundImage string   `json:"background_image"`
	FontSize        string   `json:"font_size"`
	FontColorType   string   `json:"font_color_type"`
	FontColor       string   `json:"font_color"`
	CustomBgPresets []string `json:"custom_bg_presets"`
	AutoRefreshLogs *bool    `json:"auto_refresh_logs,omitempty"`

	// 默认图档位（客户端不传 size 时的兜底值）
	DefaultImageSize string `json:"default_image_size"`

	// 默认思考等级（客户端不传 reasoning_effort/thinking 时的兜底值）
	DefaultThinkingLevel string `json:"default_thinking_level"`

	// 默认响应模态：TEXT_IMAGE（图片和文本，默认）或 IMAGE_ONLY（仅图片）
	DefaultResponseModalities string `json:"default_response_modalities"`

	// 流式包间空闲超时秒数（默认 20 秒），超过此时间无数据即判定超时
	StreamIdleTimeoutSeconds int `json:"stream_idle_timeout_seconds"`

	// IPv6 出口策略
	ForceNodeIPv6     bool   `json:"force_node_ipv6"`               // 强制出口节点（server 为域名）走 IPv6 拨号
	HostIPv6Interface string `json:"host_ipv6_interface,omitempty"` // 宿主机出口绑定网卡名（空=不绑定）
	HostIPv6Address   string `json:"host_ipv6_address,omitempty"`   // 宿主机出口绑定 IPv6 地址（空=不绑定）

	// 请求历史保留天数（默认 90），超出自动清理
	HistoryRetentionDays int `json:"history_retention_days"`

	// race_log 保留天数（默认 7）。竞速明细量远大于 usage_log（约 3 万行/天），
	// 且只用于诊断，故单独设更短的保留窗口以控制库体积增长。
	RaceLogRetentionDays int `json:"race_log_retention_days"`

	// HTTP 请求级日志（http_log）：默认关闭；开启后记录每次上游请求的完整 URL/headers/body
	HTTPLogEnabled       bool `json:"http_log_enabled"`
	HTTPLogRetentionDays int  `json:"http_log_retention_days"`
	// HTTPLogBodyCapKB 是单条请求/响应体捕获上限（KB），0 表示不截断（默认 64KB）。
	HTTPLogBodyCapKB int `json:"http_log_body_cap_kb"`

	// 强制工具调用（合成 Tool 封装）：注入合成工具使模型回答经 tool call 承载，
	// 回程解包为纯文本交付客户端。Mode 取 关闭/优先/强制；
	// ForcedToolCallModels 为空表示不限制模型（仅受"文本家族 + 支持工具"约束）。
	ForcedToolCallMode   string   `json:"forced_tool_call_mode"`
	ForcedToolCallModels []string `json:"forced_tool_call_models,omitempty"`

	// OpenAI SSE 心跳间隔秒数（默认 0 = 关闭）。仅作用于 /v1/chat/completions 的流式响应：
	// 每隔该秒数向流中写一行 SSE 注释帧 ": ping"，防止长思考期间下游网关
	// （new-api / Nginx 等）按 proxy_read_timeout 先行断流。0 表示不发心跳。
	OpenAISSEHeartbeatSeconds int `json:"openai_sse_heartbeat_seconds"`

	// ProbeFailThreshold 是**探测阶段**失败阈值的全局默认值（JSON 键
	// health_probe_fail_threshold，默认 2，范围 0~10）。
	//
	// 语义（策略可配置化后）：该类别的策略为"禁用后恢复探测"且未单独自定义阈值时，
	// 自动探测连续失败达 N 次即转永久禁用；**0 = 无限探测**（不因探测失败转永久，
	// 探测成功仍照常恢复）。它**不再门控"首次移出可用池"**——首次移出由策略表决定。
	//
	// 用指针而非 int：0 已是合法值，无法再用 0 表示"未配置"。nil = 未配置 → 取默认 2。
	ProbeFailThreshold *int `json:"health_probe_fail_threshold,omitempty"`
	// ProbeFailThresholdLegacy 是旧键 health_disable_threshold 的**只读兼容位**。
	// 新键缺失时取其值（旧语义是"首次移出阈值"，在新模型里对应探测阶段阈值），
	// 并打一条迁移日志提示改用新键。保留一个版本后移除。
	ProbeFailThresholdLegacy *int `json:"health_disable_threshold,omitempty"`
	// probeFailThreshold 是归一后的生效值（不参与 JSON 序列化；见 normalizeConfig）。
	probeFailThreshold int

	// 健康禁用自动探测开关（默认开）：后台周期性对"健康判定触发禁用"的出口节点发起
	// 真实探查——完整 reCAPTCHA token 抓取（anchor+reload），抓取成功才算通过，不是单纯
	// 连通性——成功即自动解除禁用。被禁节点已退出调度、请求路径触达不了，关闭本开关后
	// 这类节点只能人工恢复（管理页单节点测速 /「启用」按钮）。仅出口池生效；人工禁用、
	// admin 测试禁用与永久禁用不受本开关影响（从来不会被自动探测）。
	HealthSelfHealEnabled bool `json:"health_selfheal_enabled"`
	// HealthSelfHealIntervalMinutes 是自动探测的周期分钟数（默认 120，范围 1~1440）。
	// 上一轮探测结束后按当前配置重新等待，面板改值后下一轮即生效，无需重启。
	HealthSelfHealIntervalMinutes int `json:"health_selfheal_interval_minutes"`
	// HealthSelfHealProbeConcurrency 是单轮自动探测的并发度（默认 5，范围 1~20）。
	// 探测含完整 rT 抓取、单节点可能持续数秒，串行会让节点多的池一轮跑很久；
	// 5 是"尽快探完"与"不与线上请求抢出口带宽"之间的折中。
	HealthSelfHealProbeConcurrency int `json:"health_selfheal_probe_concurrency"`

	// 全局故障保护（默认开）：窗口内 ≥N 个**不同节点**出现同一类节点级故障时，判定为
	// 疑似公共依赖/环境故障并暂停该类别的自动禁用。没有它时一次本地 DNS/链路故障会让
	// 整池节点各自"连败达阈值"而被逐个误禁，而节点本身毫无问题。
	HealthGlobalFaultGuardEnabled bool `json:"health_global_fault_guard_enabled"`
	// HealthGlobalFaultMinNodes 是触发全局故障判定所需的不同节点数（默认 3，范围 2~50）。
	HealthGlobalFaultMinNodes int `json:"health_global_fault_min_nodes"`
	// HealthGlobalFaultWindowSeconds 是全局故障判定的滑动窗口秒数（默认 120，范围 10~3600）。
	HealthGlobalFaultWindowSeconds int `json:"health_global_fault_window_seconds"`

	// 浏览器指纹（上游 TLS ClientHello + HTTP 请求头）双轨制：
	// Enabled 为总开关，默认关——关闭时逐字节复现既有行为（TLS 在 Chrome
	// 144/146 间随机 + Chrome 150 请求头），开启后按 Mode 使用预设（只读，
	// 代码内置）或手动导入的自定义包（可编辑，与预设互不覆盖）。
	BrowserFingerprintEnabled bool   `json:"browser_fingerprint_enabled"`
	BrowserFingerprintMode    string `json:"browser_fingerprint_mode"`   // "preset" | "custom"
	BrowserFingerprintPreset  string `json:"browser_fingerprint_preset"` // 预设名，如 "chrome_150"
	BrowserFingerprintCustom  string `json:"browser_fingerprint_custom"` // 手动导入的 JSON 包（原样存储）

	// TLS 指纹策略：与请求头（UA）来源完全独立，可任意组合（如 TLS 144 + UA 150）。
	// Mode: follow=跟随预设 / legacy=复现默认 144-146 随机 / single=单值 /
	// range=版本区间（Min~Max 内全部可用 profile 成池）/ list=显式列表（Pool）。
	BrowserFingerprintTLSMode    string   `json:"browser_fingerprint_tls_mode"`
	BrowserFingerprintTLSProfile string   `json:"browser_fingerprint_tls_profile"`
	BrowserFingerprintTLSMin     int      `json:"browser_fingerprint_tls_min"`
	BrowserFingerprintTLSMax     int      `json:"browser_fingerprint_tls_max"`
	BrowserFingerprintTLSPool    []string `json:"browser_fingerprint_tls_pool,omitempty"`

	// 地区语言预设：Accept-Language / recaptcha hl / payload Locale+Timezone
	// 四处消费点同源，避免各写各的导致自相矛盾。
	BrowserFingerprintLocale string `json:"browser_fingerprint_locale"`
}

func DefaultConfig() AppConfig {
	return AppConfig{ //nolint:exhaustruct
		PortAPI:                    2156,
		MaxRetries:                 10, // 补位轮数：总发射预算 = (max_retries+1) × parallel_pool_size；补位经严格/宽松选点天然绕开刚 429 的节点（冷却排除），预算越大轮到的新鲜节点越多
		VertexAPIKey:               defaultAnonAPIKey,
		CountTokensQuerySignature:  defaultCountTokensQuerySig,
		MaxN:                       8,
		MaxSpillMB:                 2048,
		RequestTimeoutSeconds:      180,
		RequestTotalTimeoutSeconds: 0, // 请求总时长总闸默认不限，避免误伤长流式
		ParallelPoolEnabled:        true,
		ParallelPoolSize:           15,    // 默认为 15 并发
		MaxConcurrent:              64,    // 默认入站全局并发闸门 64
		RTCompensationMs:           500,   // rT 第一发补偿静置间隔，实测与成功率无关，保留可调
		RecaptchaTokenPrefetch:     false, // 预取池默认关：验证期保守，确认稳定后再开
		RecaptchaNoDirectFetch:     false, // 默认允许入口/直连抓 rT（保持既有降级行为）
		RecaptchaPrefetchDepth:     5,
		RecaptchaPrefetchTTLSec:    90,
		RecaptchaPrefetchBudgetSec: 15,    // 单轮预取抓取预算（秒），与请求路径 15s 对齐
		RecaptchaLiveBudgetSec:     15,    // 请求路径单轮 rT 抓取预算（秒），关键路径
		RecaptchaPrefetchAdaptive:  true,  // 无收益（池满后过期）即自停补货，请求变密时自动恢复
		RecaptchaTryEntryOrDirect:  false, // 默认关闭「优先前置/直连抓取 RT」：优先走出口候选
		BackgroundImage:            "url('background.jpg')",
		FontSize:                   "14px",
		FontColorType:              "adaptive",
		FontColor:                  "#f6f1e9",
		CustomBgPresets:            []string{},
		DefaultImageSize:           "1K",
		DefaultThinkingLevel:       "自动",
		DefaultResponseModalities:  "图文",
		StreamIdleTimeoutSeconds:   20,
		HistoryRetentionDays:       90,
		RaceLogRetentionDays:       7,
		HTTPLogEnabled:             false,
		HTTPLogRetentionDays:       30,
		HTTPLogBodyCapKB:           64,
		AdminAPILogEnabled:         false, // admin 控制台请求行默认关：/api/admin 的 3 秒级轮询会刷屏，排查时再开
		ForcedToolCallMode:         ForcedModeOff,
		OpenAISSEHeartbeatSeconds:  0, // 默认关闭：仅 /v1/chat/completions 流式心跳，0 = 不发
		// 新键**留空**（nil = 未配置）——0 已是合法值，不能再用 0 当缺省哨兵；
		// 默认值只写进归一后的生效字段，由 normalizeConfig 统一解析。
		probeFailThreshold:             defaultProbeFailThreshold,
		HealthSelfHealEnabled:          true,
		HealthSelfHealIntervalMinutes:  120, // 自动探测周期（分钟），默认 120 分钟一轮
		HealthSelfHealProbeConcurrency: 5,   // 单轮探测并发度
		HealthGlobalFaultGuardEnabled:  true,
		HealthGlobalFaultMinNodes:      3,
		HealthGlobalFaultWindowSeconds: 120,
		// 浏览器指纹默认关闭：关闭态逐字节复现既有行为，开启才改变实际请求特征。
		BrowserFingerprintEnabled: false,
		BrowserFingerprintMode:    fingerprint.ModePreset,
		BrowserFingerprintPreset:  fingerprint.DefaultPreset,
		BrowserFingerprintCustom:  "",
		BrowserFingerprintTLSMode: fingerprint.DefaultTLSMode,
		BrowserFingerprintLocale:  fingerprint.DefaultLocale,
		TrailingFixModels: []string{
			"gemini-3.5-flash-lite",
			"gemini-3.6-flash",
			"gemini-3.7-flash",
		},
	}
}

var (
	//nolint:gochecknoglobals // Global configuration cache
	mu sync.Mutex
	//nolint:gochecknoglobals // Global configuration cache
	cached *AppConfig
	//nolint:gochecknoglobals // Global configuration cache
	cacheTime time.Time
	// badCfgLogAt 节流"解析失败"日志：Load 是请求路径热点，坏配置存活期间不能刷屏。
	//nolint:gochecknoglobals
	badCfgLogAt atomic.Int64
)

const cacheTTL = 60 * time.Second

// defaultProbeFailThreshold 是 health_probe_fail_threshold 未配置时的默认值。
// 与 nodestore.DefaultProbeFailThreshold 一致（config 不反向依赖节点域，故各持一份常量，
// 由 config 侧测试锁定两者相等）。
const defaultProbeFailThreshold = 2

// intPtr 取 int 的指针（配置里的"可空整数"）。
func intPtr(v int) *int { return &v }

// HealthProbeFailThreshold 返回归一后的探测阶段失败阈值（0 = 无限探测）。
func (c AppConfig) HealthProbeFailThreshold() int { return c.probeFailThreshold }

func configPath() string {
	if p := os.Getenv("VPROXY_CONFIG"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "config.json")
		if _, errStat := os.Stat(p); errStat == nil { //nolint:govet
			return p
		}
	}
	return filepath.Join("config", "config.json")
}

func ConfigPath() string { return configPath() }

func ConfigDir() string { return filepath.Dir(configPath()) }

func WriteSettings(updates map[string]any) error {
	path := configPath()
	raw := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &raw)
	}
	for k, v := range updates {
		raw[k] = v
	}

	// 拦截并在面板保存配置时限制并发上限为 100
	if val, ok := raw["parallel_pool_size"].(float64); ok && val > 100 {
		log.Printf("[Config] 面板设置并发数过高 (%v)，已强制保存为上限 100", val)
		raw["parallel_pool_size"] = 100
	} else if val, ok2 := raw["parallel_pool_size"].(int); ok2 && val > 100 { //nolint:govet
		log.Printf("[Config] 面板设置并发数过高 (%v)，已强制保存为上限 100", val)
		raw["parallel_pool_size"] = 100
	}

	// 预取 TTL 必须留足 2 分钟硬上限的余量，面板保存时钳到 110 秒。
	if val, ok := raw["recaptcha_prefetch_ttl_sec"].(float64); ok && val > 110 {
		log.Printf("[Config] 面板设置预取 TTL 过高 (%v)，已强制保存为上限 110", val)
		raw["recaptcha_prefetch_ttl_sec"] = 110
	}
	if val, ok := raw["recaptcha_prefetch_depth"].(float64); ok && val > 100 {
		log.Printf("[Config] 面板设置预取深度过高 (%v)，已强制保存为上限 100", val)
		raw["recaptcha_prefetch_depth"] = 100
	}

	if err := writeJSONFile(path, raw); err != nil {
		return err
	}
	InvalidateCache()
	return nil
}

// legacyProbeFailThresholdKey 是 health_probe_fail_threshold 的旧键名。
// 语义已从"首次移出阈值"变为"探测阶段失败阈值"，故改名；旧键保留只读兼容一个版本。
const legacyProbeFailThresholdKey = "health_disable_threshold"

// migrateLegacyConfigKeys 把配置文件里的旧键就地改写成新键（原子写）。
//
// 行为：
//   - 文件里没有旧键 → 空操作（绝大多数情况）；
//   - 有旧键 → 写入新键（新键已存在则以其为准），删除旧键，整文件原子替换；
//   - 读不出/解析不了原文件，或写入失败 → 只记一条日志，不影响本次加载
//     （旧键取值已在 normalizeConfig 里生效，功能不受影响，下次启动再试）。
//
// 有意不在这里提示用户去改配置：迁移由程序完成，日志只留一行审计。
func migrateLegacyConfigKeys(resolved int) {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if _, hasLegacy := raw[legacyProbeFailThresholdKey]; !hasLegacy {
		return
	}
	if _, hasNew := raw["health_probe_fail_threshold"]; !hasNew {
		raw["health_probe_fail_threshold"] = resolved
	}
	delete(raw, legacyProbeFailThresholdKey)
	if err := writeJSONFile(path, raw); err != nil {
		log.Printf("[Config] 旧键 %s 自动迁移失败（本次仍按已生效值运行，下次启动重试）: %v",
			legacyProbeFailThresholdKey, err)
		return
	}
	log.Printf("[Config] 已自动迁移配置键 %s → health_probe_fail_threshold（值 %d），文件已更新",
		legacyProbeFailThresholdKey, resolved)
}

// 尾部回合兼容的旧全局键名（已从设置页下线，迁移为模型级开关后移除）。
const (
	trailingFixEnabledKey = "trailing_model_fix_enabled"
	trailingFixModelsKey  = "trailing_fix_models"
)

// migrateTrailingFixToModelSettings 把尾部回合兼容的旧全局键迁移为模型级显式选项。
//
// 旧模型（设置页两个键，已下线）：trailing_model_fix_enabled（全局开关）+
// trailing_fix_models（全局清单）；新模型：model_settings.json 的
// model_options[<模型>].trailing_turn_fix（三态，可逐模型控制）。
//
// 迁移规则：
//   - 覆盖范围是**全部本地已知模型**（models ∪ disabled ∪ custom），避免停用/自定义
//     模型漏配；仅当旧全局开关为开启且模型命中旧清单（大小写不敏感子串匹配）时
//     写入显式 true；不匹配的模型不写（保持默认 false）；
//   - 已存在的显式值一律不覆盖（显式 false 永远优先于旧全局配置）；
//   - 旧开关为关闭：只删旧键，保留已有模型级显式值；
//   - 写 model_settings.json 失败 → **不删旧键**（运行时按旧键回退继续生效，下次启动重试）；
//   - 仅两个旧键都成功删除后视为迁移完成；成功后清空 model_settings 缓存。
//
// 调用位置与前提：在 Load() 内、config.mu 持有下调用（与 migrateLegacyConfigKeys 同址），
// 此时模型清单与旧配置均已可读；因此**严禁在此调用 Load()**（会死锁），只使用入参的归一化值。
func migrateTrailingFixToModelSettings(cfg *AppConfig) {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	_, hasEnabled := raw[trailingFixEnabledKey]
	_, hasList := raw[trailingFixModelsKey]
	if !hasEnabled && !hasList {
		return
	}

	written := 0
	if cfg.TrailingModelFixEnabled && len(cfg.TrailingFixModels) > 0 {
		ms := loadModelSettings()
		opts := make(map[string]ModelOptions, len(ms.ModelOptions))
		for k, v := range ms.ModelOptions {
			opts[k] = v
		}
		for _, model := range knownModelsFor(ms) {
			if !matchTrailingFixList(model, cfg.TrailingFixModels) {
				continue
			}
			cur, ok := opts[model]
			if ok && cur.TrailingTurnFix != nil {
				continue // 已有显式值：迁移不得覆盖用户决定
			}
			enabled := true
			cur.TrailingTurnFix = &enabled
			opts[model] = cur
			written++
		}
		if written > 0 {
			target := ModelSettings{
				DisabledModels: normalizeModelList(ms.DisabledModels),
				CustomModels:   normalizeModelList(ms.CustomModels),
				ModelOptions:   normalizeModelOptions(opts),
			}
			if err := backupFileIfExists(ModelSettingsPath()); err != nil {
				log.Printf("[Config] 尾部回合兼容迁移写入失败，旧配置键保留（本次仍按旧键运行，下次启动重试）: %v", err)
				return
			}
			if err := writeJSONFile(ModelSettingsPath(), target); err != nil {
				log.Printf("[Config] 尾部回合兼容迁移写入失败，旧配置键保留（本次仍按旧键运行，下次启动重试）: %v", err)
				return
			}
			InvalidateModelSettingsCache()
		}
	}
	delete(raw, trailingFixEnabledKey)
	delete(raw, trailingFixModelsKey)
	if err := writeJSONFile(path, raw); err != nil {
		log.Printf("[Config] 尾部回合兼容旧键移除失败（本次仍按旧键运行，下次启动重试）: %v", err)
		return
	}
	log.Printf("[Config] 尾部回合兼容已迁移为模型级开关（写入显式值 %d 个），旧配置键已移除", written)
}

func writeJSONFile(path string, v any) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("error: %w", err)

	}
	return os.Rename(tmp, path) //nolint:wrapcheck
}

func Load() AppConfig {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil && time.Since(cacheTime) < cacheTTL {
		return *cached
	}
	cfg := DefaultConfig()
	if _, statErr := os.Stat(configPath()); os.IsNotExist(statErr) {
		examplePath := filepath.Join(filepath.Dir(configPath()), "config.example.json")
		if exampleData, readErr := os.ReadFile(examplePath); readErr == nil {
			if mkErr := os.MkdirAll(filepath.Dir(configPath()), 0o755); mkErr == nil {
				if writeErr := os.WriteFile(configPath(), exampleData, 0o644); writeErr == nil {
					log.Printf("[Config] 已从 config.example.json 创建初始配置")
				}
			}
		}
	}
	if data, err := os.ReadFile(configPath()); err == nil {
		if errUnm := json.Unmarshal(data, &cfg); errUnm != nil {
			// 解析失败：返回纯默认值且不缓存。半成品（部分字段覆盖默认值）被缓存
			// cacheTTL 会把一次误操作放大成全服务行为漂移；纯默认值自洽且经 normalize
			// 保证合法，配置修复后下一次 Load 自动恢复。日志按分钟节流防刷屏。
			// 注意打印 errUnm（真实解析错误），外层 err 此时必为 nil。
			if now := time.Now().Unix(); now-badCfgLogAt.Load() >= 60 {
				badCfgLogAt.Store(now)
				log.Printf("[Config] 解析 config.json 失败（期间按默认配置运行，修复后自动生效）: %v", errUnm)
			}
			def := DefaultConfig()
			normalizeConfig(&def)
			return def
		}
		normalizeConfig(&cfg)
		// 未完成的模型配置事务（双文件写入被崩溃打断）先前滚收敛，再做旧键迁移：
		// 保证后续读取到的 models.json / model_settings.json 处于一致状态。
		RecoverModelConfigTx()
		// 旧键自动迁移：就地改写配置文件，而不是只在日志里提示用户手工改。
		// 理由：新键缺省时旧键仍生效，长期"运行在新键、文件里躺着弃用键"会让后续排查
		// 与备份恢复都踩坑；这里直接把文件对齐到新键（原子写，失败只记日志不阻断启动）。
		migrateLegacyConfigKeys(cfg.probeFailThreshold)
		// 尾部回合兼容（设置页旧键）迁移为模型级显式选项：与旧键迁移同址，
		// 覆盖全部本地已知模型；失败只记日志，运行时按旧键回退继续生效。
		migrateTrailingFixToModelSettings(&cfg)
		log.Printf("[Config] 成功加载配置文件 config.json")
	} else if !os.IsNotExist(err) {
		log.Printf("[Config] 读取 config.json 失败: %v", err)
	}
	cached = &cfg
	cacheTime = time.Now()
	return cfg
}

// normalizeConfig 收敛全部钳制/回退逻辑：成功解析与解析失败（半成品默认值）两条
// 路径都必须经过，保证离开 Load 的配置永远在合法值域内。只打日志，不返回错误。
func normalizeConfig(cfg *AppConfig) {
	// 拦截在文件读取配置时过高的并发数限制为 100
	if cfg.ParallelPoolSize > 100 {
		log.Printf("[Config] 警告: 并发数配置过高 (%d)，已强制限制为上限 100", cfg.ParallelPoolSize)
		cfg.ParallelPoolSize = 100
	}
	if cfg.DefaultImageSize == "" {
		cfg.DefaultImageSize = "1K"
	} else if t := normalizeImageSizeTier(cfg.DefaultImageSize); t != "" {
		cfg.DefaultImageSize = t
	} else {
		log.Printf("[Config] default_image_size 非法 (%q)，回退 1K", cfg.DefaultImageSize)
		cfg.DefaultImageSize = "1K"
	}
	if cfg.DefaultThinkingLevel == "" {
		cfg.DefaultThinkingLevel = "自动"
	} else if t := normalizeThinkingLevel(cfg.DefaultThinkingLevel); t != "" {
		cfg.DefaultThinkingLevel = t
	} else {
		log.Printf("[Config] default_thinking_level 非法 (%q)，回退 自动", cfg.DefaultThinkingLevel)
		cfg.DefaultThinkingLevel = "自动"
	}
	if cfg.DefaultResponseModalities == "" {
		cfg.DefaultResponseModalities = "图文"
	} else if cfg.DefaultResponseModalities != "仅图片" {
		cfg.DefaultResponseModalities = "图文"
	}
	if cfg.StreamIdleTimeoutSeconds <= 0 {
		cfg.StreamIdleTimeoutSeconds = 20
	}
	// 保留天数下界必须 >=1：0/负值会让清理 cutoff 落到"现在"，一次删光全部历史。
	if cfg.RaceLogRetentionDays < 1 {
		if cfg.RaceLogRetentionDays != 0 {
			log.Printf("[Config] race_log_retention_days 非法 (%d)，回退 7", cfg.RaceLogRetentionDays)
		}
		cfg.RaceLogRetentionDays = 7
	}
	if cfg.OpenAISSEHeartbeatSeconds < 0 {
		log.Printf("[Config] openai_sse_heartbeat_seconds 非法 (%d)，回退 0（关闭）", cfg.OpenAISSEHeartbeatSeconds)
		cfg.OpenAISSEHeartbeatSeconds = 0
	} else if cfg.OpenAISSEHeartbeatSeconds > 600 {
		log.Printf("[Config] openai_sse_heartbeat_seconds 过大 (%d)，已钳制为上限 600", cfg.OpenAISSEHeartbeatSeconds)
		cfg.OpenAISSEHeartbeatSeconds = 600
	}
	if cfg.RTCompensationMs < 0 {
		log.Printf("[Config] rt_compensation_ms 非法 (%d)，回退 500", cfg.RTCompensationMs)
		cfg.RTCompensationMs = 500
	}
	if cfg.RecaptchaPrefetchDepth <= 0 {
		if cfg.RecaptchaPrefetchDepth != 0 {
			log.Printf("[Config] recaptcha_prefetch_depth 非法 (%d)，回退 5", cfg.RecaptchaPrefetchDepth)
		}
		cfg.RecaptchaPrefetchDepth = 5
	} else if cfg.RecaptchaPrefetchDepth > 100 {
		log.Printf("[Config] 预取深度过高 (%d)，已钳制为上限 100", cfg.RecaptchaPrefetchDepth)
		cfg.RecaptchaPrefetchDepth = 100
	}
	// Google 侧 token 硬上限 2 分钟，且过期 token 报 timeout-or-duplicate
	// （不落在第一发补偿的识别范围内），故 TTL 必须留足余量，上限钳到 110 秒。
	if cfg.RecaptchaPrefetchTTLSec <= 0 {
		if cfg.RecaptchaPrefetchTTLSec != 0 {
			log.Printf("[Config] recaptcha_prefetch_ttl_sec 非法 (%d)，回退 90", cfg.RecaptchaPrefetchTTLSec)
		}
		cfg.RecaptchaPrefetchTTLSec = 90
	} else if cfg.RecaptchaPrefetchTTLSec > 110 {
		log.Printf("[Config] 预取 TTL 过高 (%d 秒)，已钳制为上限 110", cfg.RecaptchaPrefetchTTLSec)
		cfg.RecaptchaPrefetchTTLSec = 110
	}
	// 预取单轮预算：后台补货一次抓取的超时。开启「禁止直连抓 rT」后抓取只能走
	// 出口候选（每个候选最多 3 次尝试 × 2~3 个 HTTP），15s 常连一个候选都跑不完，
	// 故开放上调；后台无人在等，调大不影响请求延迟，只影响后台会话持有时长。
	if cfg.RecaptchaPrefetchBudgetSec <= 0 {
		if cfg.RecaptchaPrefetchBudgetSec != 0 {
			log.Printf("[Config] recaptcha_prefetch_budget_sec 非法 (%d)，回退 15",
				cfg.RecaptchaPrefetchBudgetSec)
		}
		cfg.RecaptchaPrefetchBudgetSec = 15
	} else if cfg.RecaptchaPrefetchBudgetSec < 5 {
		log.Printf("[Config] 预取单轮预算过小 (%d 秒)，已钳制为下限 5", cfg.RecaptchaPrefetchBudgetSec)
		cfg.RecaptchaPrefetchBudgetSec = 5
	} else if cfg.RecaptchaPrefetchBudgetSec > 180 {
		log.Printf("[Config] 预取单轮预算过大 (%d 秒)，已钳制为上限 180", cfg.RecaptchaPrefetchBudgetSec)
		cfg.RecaptchaPrefetchBudgetSec = 180
	}
	// 实时单轮预算：请求关键路径上的一次 rT 抓取超时。上限比预取更保守（120s），
	// 因为它直接等于客户端最坏等待时间。
	if cfg.RecaptchaLiveBudgetSec <= 0 {
		if cfg.RecaptchaLiveBudgetSec != 0 {
			log.Printf("[Config] recaptcha_live_budget_sec 非法 (%d)，回退 15",
				cfg.RecaptchaLiveBudgetSec)
		}
		cfg.RecaptchaLiveBudgetSec = 15
	} else if cfg.RecaptchaLiveBudgetSec < 5 {
		log.Printf("[Config] 实时单轮预算过小 (%d 秒)，已钳制为下限 5", cfg.RecaptchaLiveBudgetSec)
		cfg.RecaptchaLiveBudgetSec = 5
	} else if cfg.RecaptchaLiveBudgetSec > 120 {
		log.Printf("[Config] 实时单轮预算过大 (%d 秒)，已钳制为上限 120", cfg.RecaptchaLiveBudgetSec)
		cfg.RecaptchaLiveBudgetSec = 120
	}
	if cfg.RequestTimeoutSeconds < 100 {
		cfg.RequestTimeoutSeconds = 100
	} else if cfg.RequestTimeoutSeconds > 600 {
		// 上界钳制：requestTimeout 是单候选超时与优雅关闭排干的基准，
		// 无上限时一次误配会让在途请求拖住整个关停窗口。
		log.Printf("[Config] 请求超时配置过大 (%d 秒)，已钳制为上限 600", cfg.RequestTimeoutSeconds)
		cfg.RequestTimeoutSeconds = 600
	}
	if cfg.RequestTotalTimeoutSeconds < 0 {
		cfg.RequestTotalTimeoutSeconds = 0
	} else if cfg.RequestTotalTimeoutSeconds > 1800 {
		log.Printf("[Config] 请求总时长上限过大 (%d 秒)，已钳制为上限 1800", cfg.RequestTotalTimeoutSeconds)
		cfg.RequestTotalTimeoutSeconds = 1800
	}
	cfg.TrailingFixModels = normalizeModelList(cfg.TrailingFixModels)
	cfg.ForcedToolCallModels = normalizeModelList(cfg.ForcedToolCallModels)
	if cfg.ForcedToolCallMode == "" {
		cfg.ForcedToolCallMode = ForcedModeOff
	} else if m := normalizeForcedMode(cfg.ForcedToolCallMode); m != "" {
		cfg.ForcedToolCallMode = m
	} else {
		log.Printf("[Config] forced_tool_call_mode 非法 (%q)，回退 关闭", cfg.ForcedToolCallMode)
		cfg.ForcedToolCallMode = ForcedModeOff
	}
	// max_request_mb 出厂显式默认：0 的语义是"不限"，未配置时按 32MB 兜底，
	// 避免默认就允许任意大小请求体打爆内存（负值由下方统一收敛）。
	if cfg.MaxRequestMB == 0 {
		cfg.MaxRequestMB = 32
	} else if cfg.MaxRequestMB < 0 {
		log.Printf("[Config] max_request_mb 非法 (%d)，回退 32", cfg.MaxRequestMB)
		cfg.MaxRequestMB = 32
	}
	// 探测失败阈值：新键优先；缺失时读旧键（health_disable_threshold）作为兼容取值，
	// 由 Load 在归一后**自动把旧键改写成新键**（见 migrateLegacyConfigKeys），不打扰用户。
	// 两者都缺 → 默认 2。0 是合法值（无限探测），故"未配置"只能用 nil 判定。
	switch {
	case cfg.ProbeFailThreshold != nil:
		cfg.probeFailThreshold = *cfg.ProbeFailThreshold
	case cfg.ProbeFailThresholdLegacy != nil:
		// 旧键仍生效（只读兼容），随后由 migrateLegacyConfigKeys 把它就地改写成新键。
		cfg.probeFailThreshold = *cfg.ProbeFailThresholdLegacy
	default:
		cfg.probeFailThreshold = defaultProbeFailThreshold
	}
	if cfg.probeFailThreshold < 0 {
		log.Printf("[Config] health_probe_fail_threshold 非法 (%d)，回退 %d", cfg.probeFailThreshold, defaultProbeFailThreshold)
		cfg.probeFailThreshold = defaultProbeFailThreshold
	} else if cfg.probeFailThreshold > 10 {
		log.Printf("[Config] health_probe_fail_threshold 过大 (%d)，已钳制为上限 10", cfg.probeFailThreshold)
		cfg.probeFailThreshold = 10
	}
	if cfg.HealthSelfHealIntervalMinutes <= 0 {
		if cfg.HealthSelfHealIntervalMinutes != 0 {
			log.Printf("[Config] health_selfheal_interval_minutes 非法 (%d)，回退 120", cfg.HealthSelfHealIntervalMinutes)
		}
		cfg.HealthSelfHealIntervalMinutes = 120
	} else if cfg.HealthSelfHealIntervalMinutes > 1440 {
		log.Printf("[Config] health_selfheal_interval_minutes 过大 (%d)，已钳制为上限 1440", cfg.HealthSelfHealIntervalMinutes)
		cfg.HealthSelfHealIntervalMinutes = 1440
	}
	if cfg.HealthSelfHealProbeConcurrency <= 0 {
		if cfg.HealthSelfHealProbeConcurrency != 0 {
			log.Printf("[Config] health_selfheal_probe_concurrency 非法 (%d)，回退 5", cfg.HealthSelfHealProbeConcurrency)
		}
		cfg.HealthSelfHealProbeConcurrency = 5
	} else if cfg.HealthSelfHealProbeConcurrency > 20 {
		log.Printf("[Config] health_selfheal_probe_concurrency 过大 (%d)，已钳制为上限 20", cfg.HealthSelfHealProbeConcurrency)
		cfg.HealthSelfHealProbeConcurrency = 20
	}
	// 全局故障保护：下界必须 >=2——"1 个节点同因失败"就是节点自身故障，
	// 设成 1 会让这台保护器永远生效，自动禁用彻底失效。
	if cfg.HealthGlobalFaultMinNodes < 2 {
		if cfg.HealthGlobalFaultMinNodes != 0 {
			log.Printf("[Config] health_global_fault_min_nodes 非法 (%d)，回退 3", cfg.HealthGlobalFaultMinNodes)
		}
		cfg.HealthGlobalFaultMinNodes = 3
	} else if cfg.HealthGlobalFaultMinNodes > 50 {
		log.Printf("[Config] health_global_fault_min_nodes 过大 (%d)，已钳制为上限 50", cfg.HealthGlobalFaultMinNodes)
		cfg.HealthGlobalFaultMinNodes = 50
	}
	if cfg.HealthGlobalFaultWindowSeconds <= 0 {
		if cfg.HealthGlobalFaultWindowSeconds != 0 {
			log.Printf("[Config] health_global_fault_window_seconds 非法 (%d)，回退 120", cfg.HealthGlobalFaultWindowSeconds)
		}
		cfg.HealthGlobalFaultWindowSeconds = 120
	} else if cfg.HealthGlobalFaultWindowSeconds > 3600 {
		log.Printf("[Config] health_global_fault_window_seconds 过大 (%d)，已钳制为上限 3600", cfg.HealthGlobalFaultWindowSeconds)
		cfg.HealthGlobalFaultWindowSeconds = 3600
	} else if cfg.HealthGlobalFaultWindowSeconds < 10 {
		log.Printf("[Config] health_global_fault_window_seconds 过小 (%d)，已钳制为下限 10", cfg.HealthGlobalFaultWindowSeconds)
		cfg.HealthGlobalFaultWindowSeconds = 10
	}
	// 浏览器指纹：mode/preset 白名单归一；custom 手动包非法时整体回退"未启用"
	// （仅内存生效，文件不动），绝不半生效——半生效意味着 TLS 与请求头可能各用
	// 一半版本，重新制造版本错配。
	switch cfg.BrowserFingerprintMode {
	case "":
		cfg.BrowserFingerprintMode = fingerprint.ModePreset
	case fingerprint.ModePreset, fingerprint.ModeCustom:
	default:
		log.Printf("[Config] browser_fingerprint_mode 非法 (%q)，回退 preset", cfg.BrowserFingerprintMode)
		cfg.BrowserFingerprintMode = fingerprint.ModePreset
	}
	if cfg.BrowserFingerprintMode == fingerprint.ModePreset && !fingerprint.IsPreset(cfg.BrowserFingerprintPreset) {
		log.Printf("[Config] browser_fingerprint_preset 非法 (%q)，回退 %s", cfg.BrowserFingerprintPreset, fingerprint.DefaultPreset)
		cfg.BrowserFingerprintPreset = fingerprint.DefaultPreset
	}
	if cfg.BrowserFingerprintEnabled && cfg.BrowserFingerprintMode == fingerprint.ModeCustom {
		if _, err := fingerprint.ParseCustom(cfg.BrowserFingerprintCustom); err != nil {
			log.Printf("[Config] browser_fingerprint_custom 非法 (%v)，本次加载等效关闭浏览器指纹", err)
			cfg.BrowserFingerprintEnabled = false
		}
	}
	// TLS 策略归一：模式白名单、single 非法回退 follow、区间收敛、池过滤非法项。
	switch cfg.BrowserFingerprintTLSMode {
	case "":
		cfg.BrowserFingerprintTLSMode = fingerprint.TLSModeFollow
	case fingerprint.TLSModeFollow, fingerprint.TLSModeLegacy, fingerprint.TLSModeSingle,
		fingerprint.TLSModeRange, fingerprint.TLSModeList:
	default:
		log.Printf("[Config] browser_fingerprint_tls_mode 非法 (%q)，回退 follow", cfg.BrowserFingerprintTLSMode)
		cfg.BrowserFingerprintTLSMode = fingerprint.TLSModeFollow
	}
	if cfg.BrowserFingerprintTLSMode == fingerprint.TLSModeSingle &&
		!fingerprint.IsTLSProfile(cfg.BrowserFingerprintTLSProfile) {
		log.Printf("[Config] browser_fingerprint_tls_profile 非法 (%q)，回退 follow", cfg.BrowserFingerprintTLSProfile)
		cfg.BrowserFingerprintTLSMode = fingerprint.TLSModeFollow
	}
	// 区间语义在 ResolveTLSPool 内做 min/max 交换与"命中为空回退 follow"；
	// 此处只做越界钳制，保证落盘值在可用 profile 版本范围内。
	clampTLSVer := func(v *int) {
		lo, hi := 100, 999
		if *v < lo || *v > hi {
			log.Printf("[Config] browser_fingerprint_tls 版本 %d 越界，钳制到 [%d, %d]", *v, lo, hi)
			*v = min(max(*v, lo), hi)
		}
	}
	if cfg.BrowserFingerprintTLSMode == fingerprint.TLSModeRange {
		clampTLSVer(&cfg.BrowserFingerprintTLSMin)
		clampTLSVer(&cfg.BrowserFingerprintTLSMax)
	}
	if cfg.BrowserFingerprintTLSMode == fingerprint.TLSModeList {
		pool := make([]string, 0, len(cfg.BrowserFingerprintTLSPool))
		for _, name := range cfg.BrowserFingerprintTLSPool {
			if fingerprint.IsTLSProfile(name) {
				pool = append(pool, name)
			} else if name != "" {
				log.Printf("[Config] browser_fingerprint_tls_pool 忽略非法项 %q", name)
			}
		}
		cfg.BrowserFingerprintTLSPool = pool
	}
	// 地区语言预设归一。
	if !fingerprint.IsLocale(cfg.BrowserFingerprintLocale) {
		if cfg.BrowserFingerprintLocale != "" {
			log.Printf("[Config] browser_fingerprint_locale 非法 (%q)，回退 %s", cfg.BrowserFingerprintLocale, fingerprint.DefaultLocale)
		}
		cfg.BrowserFingerprintLocale = fingerprint.DefaultLocale
	}
}

func InvalidateCache() {
	mu.Lock()
	defer mu.Unlock()
	cached = nil
}

func (c AppConfig) ConfigDir() string  { return ConfigDir() }
func (c AppConfig) ConfigPath() string { return ConfigPath() }

func (c *AppConfig) WriteSettings(updates map[string]any) error { return WriteSettings(updates) }
func (c *AppConfig) WriteModels(models []string, aliasMap, familyMap map[string]string) error {
	return WriteModels(models, aliasMap, familyMap)
}

func (c AppConfig) GetAutoRefreshLogs() bool {
	if c.AutoRefreshLogs == nil {
		return true
	}
	return *c.AutoRefreshLogs
}

var allowedImageSizeTiers = map[string]bool{"512": true, "1K": true, "2K": true, "4K": true}

func normalizeImageSizeTier(s string) string {
	u := strings.ToUpper(strings.TrimSpace(s))
	if allowedImageSizeTiers[u] {
		return u
	}
	return ""
}

var allowedThinkingLevels = map[string]bool{
	"自动": true, "最低": true, "低": true, "中": true, "高": true,
}

func normalizeThinkingLevel(s string) string {
	s = strings.TrimSpace(s)
	if allowedThinkingLevels[s] {
		return s
	}
	return ""
}

// normalizeModelList 对模型清单逐项 TrimSpace、去空、去重。
// 输入为 nil（JSON 缺失/显式 null）时返回空切片；显式空数组 [] 亦被尊重。
func normalizeModelList(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// 强制工具调用模式枚举。沿用 default_thinking_level 的中文存值先例：
// 面板直接读写中文枚举，Load 时按白名单归一，非法值回退 关闭。
const (
	ForcedModeOff      = "关闭"
	ForcedModePrefer   = "优先"
	ForcedModeRequired = "强制"
)

//nolint:gochecknoglobals // Constant-like set of allowed forced tool call modes
var allowedForcedModes = map[string]bool{
	ForcedModeOff: true, ForcedModePrefer: true, ForcedModeRequired: true,
}

// normalizeForcedMode 归一化强制工具调用模式；非法值返回空串（由调用方回退默认档）。
func normalizeForcedMode(s string) string {
	s = strings.TrimSpace(s)
	if allowedForcedModes[s] {
		return s
	}
	return ""
}
