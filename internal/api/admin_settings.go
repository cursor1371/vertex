package api

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
	"github.com/bsfdsagfadg/vertex/internal/node/importer"
)

var (
	//nolint:gochecknoglobals // 测试/未注入场景兜底的内存仓储单例
	memStoreOnce sync.Once
	memStore     *db.UsageStore
)

// settingSpec 是单个设置项的类型与值域规格：白名单、PUT 类型校验与范围钳制统一由此
// 表驱动。此前白名单（bool map）与 PUT 端的手写 switch 是两份独立清单，新增键漏改
// 任意一处都会出现"能读不能写"或"非法值静默落盘"；int 键只做类型断言、断言失败
// 原样写入，负数 max_request_mb 甚至会把 body 限流反转为"不限"。
type settingSpec struct {
	kind byte // 'i' 整数 / 'b' 布尔 / 's' 字符串 / 'l' 字符串列表
	min  int  // 整数下界（含）
	max  int  // 整数上界（含），0 表示不设上界
}

//nolint:gochecknoglobals // Constant-like spec table
var adminSettingSpecs = map[string]settingSpec{
	// 整数项：范围与 config.Load 的 normalizeConfig 钳制口径对齐（PUT 拦一道、Load 兜一道）。
	"max_retries":                        {kind: 'i', min: 0, max: 50},
	"max_spill_mb":                       {kind: 'i', min: 1, max: 65536},
	"max_request_mb":                     {kind: 'i', min: 1, max: 4096},
	"max_n":                              {kind: 'i', min: 1, max: 64},
	"parallel_pool_size":                 {kind: 'i', min: 1, max: 100},
	"max_concurrent":                     {kind: 'i', min: 0, max: 4096}, // 0=不限（与 config.go 注释及前端文案一致）
	"rt_compensation_ms":                 {kind: 'i', min: 0, max: 10000},
	"recaptcha_prefetch_depth":           {kind: 'i', min: 1, max: 100},
	"recaptcha_prefetch_ttl_sec":         {kind: 'i', min: 1, max: 110},
	"recaptcha_prefetch_budget_sec":      {kind: 'i', min: 5, max: 180},
	"recaptcha_live_budget_sec":          {kind: 'i', min: 5, max: 120},
	"stream_idle_timeout_seconds":        {kind: 'i', min: 1, max: 600},
	"request_timeout_seconds":            {kind: 'i', min: 100, max: 600},
	"request_total_timeout_seconds":      {kind: 'i', min: 0, max: 1800}, // 0 = 不限（与 normalizeConfig 钳制口径对齐）
	"history_retention_days":             {kind: 'i', min: 1, max: 3650},
	"race_log_retention_days":            {kind: 'i', min: 1, max: 3650},
	"http_log_enabled":                   {kind: 'b'},
	"http_log_retention_days":            {kind: 'i', min: 1, max: 3650},
	"http_log_body_cap_kb":               {kind: 'i', min: 0, max: 102400}, // 0 = 不截断
	"openai_sse_heartbeat_seconds":       {kind: 'i', min: 0, max: 600},    // 0 = 关闭（仅 /v1/chat/completions 流式）
	"health_probe_fail_threshold":        {kind: 'i', min: 0, max: 10},     // 探测阶段失败阈值（0 = 无限探测；与 normalizeConfig 钳制口径对齐）
	"health_selfheal_enabled":            {kind: 'b'},                      // 健康禁用自动探测开关
	"health_selfheal_interval_minutes":   {kind: 'i', min: 1, max: 1440},   // 自动探测周期分钟数
	"health_selfheal_probe_concurrency":  {kind: 'i', min: 1, max: 20},     // 单轮自动探测并发度
	"health_global_fault_guard_enabled":  {kind: 'b'},                      // 全局故障保护总开关
	"health_global_fault_min_nodes":      {kind: 'i', min: 2, max: 50},     // 触发全局故障判定所需节点数（下界 2：1 个节点同因失败属节点自身故障）
	"health_global_fault_window_seconds": {kind: 'i', min: 10, max: 3600},  // 全局故障判定滑动窗口秒数
	// 布尔项。
	"aggregate_stream":              {kind: 'b'},
	"drop_max_tokens":               {kind: 'b'},
	"parallel_pool_enabled":         {kind: 'b'},
	"recaptcha_token_prefetch":      {kind: 'b'},
	"recaptcha_prefetch_adaptive":   {kind: 'b'},
	"debug_mode":                    {kind: 'b'},
	"auto_refresh_logs":             {kind: 'b'},
	"recaptcha_try_entry_or_direct": {kind: 'b'},
	"recaptcha_no_direct_fetch":     {kind: 'b'},
	"admin_api_log_enabled":         {kind: 'b'}, // admin 控制台请求行开关（默认关）
	"force_node_ipv6":               {kind: 'b'},
	// 字符串项。
	"active_node_uri":             {kind: 's'},
	"background_image":            {kind: 's'},
	"font_size":                   {kind: 's'},
	"font_color_type":             {kind: 's'},
	"font_color":                  {kind: 's'},
	"default_image_size":          {kind: 's'},
	"default_thinking_level":      {kind: 's'},
	"default_response_modalities": {kind: 's'},
	"host_ipv6_interface":         {kind: 's'},
	"host_ipv6_address":           {kind: 's'},
	"forced_tool_call_mode":       {kind: 's'},
	// 浏览器指纹（详见 fingerprint 包）：enabled 总开关；mode = preset|custom；
	// preset 为预设名；custom 为手动导入的 JSON 包（PUT 时强校验，非法 400）。
	// TLS 策略与请求头解耦：mode = follow|legacy|single|range|list，
	// profile/min/max/pool 分别是单值/区间上下界/列表池。
	// 预设内容是代码常量，无对应写键——提交预设同名字段会被白名单静默忽略，
	// 这是"预设不允许被修改"的机制保证。
	"browser_fingerprint_enabled":     {kind: 'b'},
	"browser_fingerprint_mode":        {kind: 's'},
	"browser_fingerprint_preset":      {kind: 's'},
	"browser_fingerprint_custom":      {kind: 's'},
	"browser_fingerprint_tls_mode":    {kind: 's'},
	"browser_fingerprint_tls_profile": {kind: 's'},
	"browser_fingerprint_tls_min":     {kind: 'i', min: 0, max: 999},
	"browser_fingerprint_tls_max":     {kind: 'i', min: 0, max: 999},
	"browser_fingerprint_locale":      {kind: 's'},
	// 字符串列表项。
	"custom_bg_presets":            {kind: 'l'},
	"forced_tool_call_models":      {kind: 'l'},
	"browser_fingerprint_tls_pool": {kind: 'l'},
}

func (adm *AdminHandler) adminGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": map[string]any{
		"max_retries":           adm.cfg.MaxRetries(),
		"max_spill_mb":          adm.cfg.MaxSpillMB(),
		"max_request_mb":        adm.cfg.MaxRequestMB(),
		"max_n":                 adm.cfg.MaxN(),
		"aggregate_stream":      adm.cfg.AggregateStream(),
		"drop_max_tokens":       adm.cfg.DropMaxTokens(),
		"parallel_pool_enabled": adm.cfg.ParallelPoolEnabled(), "parallel_pool_size": adm.cfg.ParallelPoolSize(), "max_concurrent": adm.cfg.MaxConcurrent(), "rt_compensation_ms": adm.cfg.RTCompensationMs(), "recaptcha_token_prefetch": adm.cfg.RecaptchaTokenPrefetch(), "recaptcha_prefetch_depth": adm.cfg.RecaptchaPrefetchDepth(), "recaptcha_prefetch_ttl_sec": adm.cfg.RecaptchaPrefetchTTLSeconds(), "recaptcha_prefetch_adaptive": adm.cfg.RecaptchaPrefetchAdaptive(), "active_node_uri": adm.cfg.ActiveNodeURI(),
		"background_image":              adm.cfg.BackgroundImage(),
		"font_size":                     adm.cfg.FontSize(),
		"font_color_type":               adm.cfg.FontColorType(),
		"font_color":                    adm.cfg.FontColor(),
		"custom_bg_presets":             adm.cfg.CustomBgPresets(),
		"debug_mode":                    adm.cfg.DebugMode(),
		"auto_refresh_logs":             adm.cfg.AutoRefreshLogs(),
		"default_image_size":            adm.cfg.DefaultImageSize(),
		"default_thinking_level":        adm.cfg.DefaultThinkingLevel(),
		"default_response_modalities":   adm.cfg.DefaultResponseModalities(),
		"stream_idle_timeout_seconds":   adm.cfg.StreamIdleTimeoutSeconds(),
		"request_timeout_seconds":       adm.cfg.RequestTimeoutSeconds(),
		"request_total_timeout_seconds": adm.cfg.RequestTotalTimeoutSeconds(),
		"recaptcha_try_entry_or_direct": adm.cfg.RecaptchaTryEntryOrDirect(),
		"admin_api_log_enabled":         adm.cfg.AdminAPILogEnabled(),
		"recaptcha_no_direct_fetch":     adm.cfg.RecaptchaNoDirectFetch(), "recaptcha_prefetch_budget_sec": adm.cfg.RecaptchaPrefetchBudgetSeconds(), "recaptcha_live_budget_sec": adm.cfg.RecaptchaLiveBudgetSeconds(),
		"force_node_ipv6":                    adm.cfg.ForceNodeIPv6(),
		"host_ipv6_interface":                adm.cfg.HostIPv6Interface(),
		"host_ipv6_address":                  adm.cfg.HostIPv6Address(),
		"history_retention_days":             adm.cfg.HistoryRetentionDays(),
		"race_log_retention_days":            adm.cfg.RaceLogRetentionDays(),
		"http_log_enabled":                   adm.cfg.HTTPLogEnabled(),
		"http_log_retention_days":            adm.cfg.HTTPLogRetentionDays(),
		"http_log_body_cap_kb":               adm.cfg.HTTPLogBodyCapKB(),
		"forced_tool_call_mode":              adm.cfg.ForcedToolCallMode(),
		"forced_tool_call_models":            adm.cfg.ForcedToolCallModels(),
		"openai_sse_heartbeat_seconds":       adm.cfg.OpenAISSEHeartbeatSeconds(),
		"browser_fingerprint_enabled":        adm.cfg.BrowserFingerprintEnabled(),
		"browser_fingerprint_mode":           adm.cfg.BrowserFingerprintMode(),
		"browser_fingerprint_preset":         adm.cfg.BrowserFingerprintPreset(),
		"browser_fingerprint_custom":         adm.cfg.BrowserFingerprintCustom(),
		"browser_fingerprint_tls_mode":       adm.cfg.BrowserFingerprintTLSMode(),
		"browser_fingerprint_tls_profile":    adm.cfg.BrowserFingerprintTLSProfile(),
		"browser_fingerprint_tls_min":        adm.cfg.BrowserFingerprintTLSMin(),
		"browser_fingerprint_tls_max":        adm.cfg.BrowserFingerprintTLSMax(),
		"browser_fingerprint_tls_pool":       adm.cfg.BrowserFingerprintTLSPool(),
		"browser_fingerprint_locale":         adm.cfg.BrowserFingerprintLocale(),
		"health_probe_fail_threshold":        adm.cfg.HealthProbeFailThreshold(),
		"health_selfheal_enabled":            adm.cfg.HealthSelfHealEnabled(),
		"health_selfheal_interval_minutes":   adm.cfg.HealthSelfHealIntervalMinutes(),
		"health_selfheal_probe_concurrency":  adm.cfg.HealthSelfHealProbeConcurrency(),
		"health_global_fault_guard_enabled":  adm.cfg.HealthGlobalFaultGuardEnabled(),
		"health_global_fault_min_nodes":      adm.cfg.HealthGlobalFaultMinNodes(),
		"health_global_fault_window_seconds": adm.cfg.HealthGlobalFaultWindowSeconds(),
	}})
}

func (adm *AdminHandler) adminPutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	updates := map[string]any{}

	for k, v := range body.Settings {
		spec, allowed := adminSettingSpecs[k]
		if !allowed {
			continue // 白名单外静默忽略（维持既有行为）
		}
		switch spec.kind {
		case 'i':
			f, ok := v.(float64)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr("设置 "+k+" 需要数字类型 (number required)"))
				return
			}
			n := int(f)
			if n < spec.min || (spec.max > 0 && n > spec.max) {
				writeJSON(w, http.StatusBadRequest,
					adminErr("设置 "+k+" 超出允许范围 ["+strconv.Itoa(spec.min)+", "+strconv.Itoa(spec.max)+"] (out of range)"))
				return
			}
			updates[k] = n
		case 'b':
			b, ok := v.(bool)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr("设置 "+k+" 需要布尔类型 (boolean required)"))
				return
			}
			updates[k] = b
		case 'l':
			list, ok := v.([]any)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr("设置 "+k+" 需要字符串数组 (string array required)"))
				return
			}
			updates[k] = normalizeModelListInput(list)
		default: // 's'
			s, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr("设置 "+k+" 需要字符串类型 (string required)"))
				return
			}
			updates[k] = s
		}
	}

	// 手动导入包保存前强校验：非法直接 400 并带回具体原因，保证存盘的手动包
	// 永远合法（手改 config.json 绕过校验的残余风险由 config.Load 归一兜底：
	// 回退"未启用"并告警，绝不半生效）。
	if s, ok := updates["browser_fingerprint_custom"]; ok {
		if _, err := fingerprint.ParseCustom(s.(string)); err != nil {
			writeJSON(w, http.StatusBadRequest, adminErr("browser_fingerprint_custom 非法: "+err.Error()))
			return
		}
	}

	if err := config.WriteSettings(updates); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("写入配置失败 (failed to write config)"))
		return
	}

	// 运行时热生效：与 SIGHUP 热重载共用同一实现，避免两处各写一份必然漂移。
	// WriteSettings 成功路径内部已 InvalidateCache，此处读到的是新值。
	ApplyRuntimeHotSettings(adm.cfg)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ApplyRuntimeHotSettings 把"运行时热生效"的配置项从 cfg 同步到各消费单例
// （历史保留期、HTTP 日志开关/保留期/正文截断上限）。
// adminPutSettings（面板保存）与 SIGHUP 热重载共用：此前 SIGHUP 分支只清缓存、
// 不同步这些 setter，手工编辑配置后发信号不生效。
// 调用方需保证 cfg 缓存已失效（WriteSettings 成功路径 / SIGHUP 分支已显式 Invalidate）。
func ApplyRuntimeHotSettings(cfg config.ConfigProvider) {
	stats.SetRetentionDays(cfg.HistoryRetentionDays())
	stats.SetRaceLogRetentionDays(cfg.RaceLogRetentionDays())
	stats.SetHTTPLogEnabled(cfg.HTTPLogEnabled())
	stats.SetHTTPLogRetentionDays(cfg.HTTPLogRetentionDays())
	// HTTPLogBodyCapKB 单位 KB → 字节；PUT 规格表已钳 [0, 102400]，×1024 不溢出。
	stats.SetHTTPLogBodyCapBytes(cfg.HTTPLogBodyCapKB() * 1024)
	// 浏览器指纹包：与 main 启动共用 fingerprint.Apply；返回错误时包内已发布
	// nil（既有行为），此处仅告警。
	if _, err := fingerprint.Apply(FingerprintSelection(cfg)); err != nil {
		log.Printf("[Admin] 浏览器指纹未生效，保持既有行为: %v", err)
	}
}

// normalizeModelListInput 归一化管理后台提交的模型清单（当前只有 forced_tool_call_models；
// 尾部回合兼容已迁移为模型级开关，不再走设置页）：
// 逐项转字符串、TrimSpace、去空、去重，顺序保持稳定。
func normalizeModelListInput(list []any) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		s := strings.TrimSpace(importer.ValueToString(item))
		if s == "" {
			continue
		}
		dup := false
		for _, e := range out {
			if e == s {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, s)
	}
	return out
}

// adminGetStats 返回统计聚合：实时快照 + 今日/累计汇总 + 按模型/Key/节点分组 + 近 30 天趋势。
func (adm *AdminHandler) adminGetStats(w http.ResponseWriter, _ *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	now := time.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	window := int64(30 * 24 * 3600) // 近 30 天
	from := now.Unix() - window
	to := now.Unix() + 1

	// "累计" 聚合以保留窗口为上限（超出窗口的记录已被周期清理），
	// 避免对无界历史做全表扫描。
	allFrom := now.Unix() - int64(stats.RetentionDays())*86400
	if allFrom < 0 {
		allFrom = 0
	}

	today, errToday := store.Aggregate(startOfToday, to)
	if errToday != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (today): "+errToday.Error()))
		return
	}
	all, errAll := store.Aggregate(allFrom, to)
	if errAll != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (all): "+errAll.Error()))
		return
	}
	byModel, errModel := store.ByModel(from, to)
	if errModel != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (by_model): "+errModel.Error()))
		return
	}
	byKey, errKey := store.ByKey(from, to)
	if errKey != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (by_key): "+errKey.Error()))
		return
	}
	byNode, errNode := store.ByNode(from, to)
	if errNode != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (by_node): "+errNode.Error()))
		return
	}
	// 按节点分组的行补齐 raw_uri 与防删锁状态（详见 enrichNodeRows）。
	adm.enrichNodeRows(byNode)
	history, errHist := store.DailySeries(from, to, 86400)
	if errHist != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (history): "+errHist.Error()))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"realtime": stats.Snapshot(),
		"runtime":  metricsBody(),
		"today":    summaryToMap(today),
		"all":      summaryToMap(all),
		"by_model": rowsToMaps(byModel),
		"by_key":   rowsToMaps(byKey),
		"by_node":  rowsToMaps(byNode),
		"history":  pointsToMaps(history),
	})
}

// enrichNodeRows 为按节点分组的聚合行补齐 raw_uri 与防删锁状态。
// 新数据分组键即 node_uri，无需反查；老数据（无 URI）按展示名反查兜底——
// URIForName 未命中时原样返回 name。nil 池（测试兜底路径）保持原值。
// nodeFlagSets 一次性返回当前处于防删锁 / 禁用状态的节点 URI 集合，
// 供统计接口把行内状态带给前端（概览表里可直接在行内防删、禁用）。
// 节点池未注入时两者均为 nil（nil map 读取恒为 false，等价"未置位"）。
func (adm *AdminHandler) nodeFlagSets() (locked, disabled map[string]bool) {
	if adm.deps.Exit == nil {
		return nil, nil
	}
	for _, n := range adm.deps.Exit.LoadNodes() {
		if n.Locked {
			if locked == nil {
				locked = make(map[string]bool)
			}
			locked[n.RawURI] = true
		}
		if n.Disabled {
			if disabled == nil {
				disabled = make(map[string]bool)
			}
			disabled[n.RawURI] = true
		}
	}
	return locked, disabled
}

func (adm *AdminHandler) enrichNodeRows(rows []db.UsageRow) {
	if adm.deps.Exit == nil {
		return
	}
	locked, disabled := adm.nodeFlagSets()
	for i := range rows {
		if rows[i].RawURI == "" {
			rows[i].RawURI = adm.deps.Exit.URIForName(rows[i].Name)
		}
		rows[i].Locked = locked[rows[i].RawURI]
		rows[i].Disabled = disabled[rows[i].RawURI]
	}
}

// adminGetNodeStats 返回指定时间窗口内按代理节点聚合的请求统计（含防删锁状态），
// 供概览「节点统计」子标签使用。query 参数 days：0 或缺省表示"全部"
// （下界取保留期，与 adminGetStats 的累计口径一致）。
func (adm *AdminHandler) adminGetNodeStats(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	days := int64(0)
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && n <= 3650 {
			days = n
		}
	}
	now := time.Now()
	var from int64
	if days > 0 {
		from = now.Unix() - days*86400
	} else {
		from = now.Unix() - int64(stats.RetentionDays())*86400
		if from < 0 {
			from = 0
		}
	}
	byNode, err := store.ByNode(from, now.Unix()+1)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("节点统计查询失败 (by_node): "+err.Error()))
		return
	}
	adm.enrichNodeRows(byNode)
	writeJSON(w, http.StatusOK, map[string]any{
		"days":    days,
		"by_node": rowsToMaps(byNode),
	})
}

// adminGetNodesStatsByURI 返回按节点 URI 聚合的请求/竞速统计，供「订阅与并发」页
// 按成功次数、成功率、Racing 成功次数、Racing 成功率的搜索与排序使用。
// query 参数 days：0 或缺省表示"全部"（下界取保留期）。
func (adm *AdminHandler) adminGetNodesStatsByURI(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	days := int64(0)
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && n <= 3650 {
			days = n
		}
	}
	now := time.Now()
	var from int64
	if days > 0 {
		from = now.Unix() - days*86400
	} else {
		from = now.Unix() - int64(stats.RetentionDays())*86400
		if from < 0 {
			from = 0
		}
	}
	to := now.Unix() + 1

	out := map[string]map[string]any{}
	usageRows, err := store.ByNode(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("节点统计查询失败 (usage): "+err.Error()))
		return
	}
	for i := range usageRows {
		key := adm.nodeStatKey(usageRows[i].Name, usageRows[i].RawURI)
		if key == "" {
			continue
		}
		m := out[key]
		if m == nil {
			m = map[string]any{}
			out[key] = m
		}
		m["requests"] = usageRows[i].Requests
		m["success"] = usageRows[i].Success
		m["errors"] = usageRows[i].Errors
	}
	raceRows, err := store.RaceByNode(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("节点统计查询失败 (racing): "+err.Error()))
		return
	}
	for i := range raceRows {
		key := adm.nodeStatKey(raceRows[i].Name, raceRows[i].RawURI)
		if key == "" {
			continue
		}
		m := out[key]
		if m == nil {
			m = map[string]any{}
			out[key] = m
		}
		m["attempts"] = raceRows[i].Attempts
		m["wins"] = raceRows[i].Wins
		m["collects"] = raceRows[i].Collects
		m["timeouts"] = raceRows[i].Timeouts
		m["race_errors"] = raceRows[i].Errors
		// rate_limits 是 429 正常竞争限流的独立计数：前端成功率分母会扣除它。
		m["rate_limits"] = raceRows[i].RateLimits
		m["avg_ms"] = raceRows[i].AvgMs
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "stats": out})
}

// nodeStatKey 把聚合行的分组键归一到 raw_uri：记录里带 URI 时直接使用；
// 老数据（无 URI）按展示名反查，仍未命中则退回名称本身，保证统计不丢。
func (adm *AdminHandler) nodeStatKey(name, uri string) string {
	if uri != "" {
		return uri
	}
	if adm.deps.Exit != nil && name != "" {
		if u := adm.deps.Exit.URIForName(name); u != "" {
			return u
		}
	}
	return name
}

// adminResetStats 清空全部统计记录。
func (adm *AdminHandler) adminResetStats(w http.ResponseWriter, _ *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	n, err := store.ResetAll()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("重置统计失败 (reset failed)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// adminGetStatsHistory 返回按天/按小时聚合的趋势序列。
// query 参数 days 控制窗口天数（默认 30）；bucket 控制桶粒度：hour 或 day（默认 day）。
func (adm *AdminHandler) adminGetStatsHistory(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	days := int64(30)
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= 3650 {
			days = n
		}
	}
	bucketSec := int64(86400)
	if strings.EqualFold(r.URL.Query().Get("bucket"), "hour") {
		bucketSec = 3600
	}
	now := time.Now()
	from := now.Unix() - days*86400
	series, err := store.DailySeries(from, now.Unix()+1, bucketSec)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计查询失败 (history): "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": pointsToMaps(series), "bucket": bucketSec})
}

// adminGetActiveRequests 返回当前在途（进行中）的请求列表，供"请求监控"实时轮询。
func (adm *AdminHandler) adminGetActiveRequests(w http.ResponseWriter, _ *http.Request) {
	active := stats.ActiveRequests()
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "count": len(active)})
}

// adminGetRTTokens 返回最近的 rT token 抓取记录（实时记录流，newest first）。
// query 参数：limit（默认 30，上限 200）；probe=1 时只返回预取池暂停状态、不返回事件——
// 供前端在补货暂停期间低频探测恢复（同路径，仍命中 admin_handler 的日志跳过名单）。
// paused/pauses 取自 WarmSnapshot：未启用预取池时为零值（paused=false），轮询行为不变。
func (adm *AdminHandler) adminGetRTTokens(w http.ResponseWriter, r *http.Request) {
	paused, pauses := false, int64(0)
	if adm.deps.RTTok != nil {
		ws := adm.deps.RTTok.WarmStats()
		paused, pauses = ws.Paused, ws.Pauses
	}
	if r.URL.Query().Get("probe") == "1" {
		writeJSON(w, http.StatusOK, map[string]any{"paused": paused, "pauses": pauses})
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	events := []recaptcha.TokenEvent{}
	if adm.deps.RTTok != nil {
		if got := adm.deps.RTTok.RecentTokenEvents(limit); len(got) > 0 {
			events = got
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events), "paused": paused, "pauses": pauses})
}

// adminGetHistoryRecords 返回按时间倒序分页的历史请求记录。
// query 参数：page（从 1 起，默认 1）、limit（默认 20，上限 200）。
func (adm *AdminHandler) adminGetHistoryRecords(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	page := 1
	limit := 20
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	pg, err := store.History(page, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("历史记录查询失败 (history): "+err.Error()))
		return
	}
	items := make([]map[string]any, 0, len(pg.Items))
	for _, it := range pg.Items {
		items = append(items, map[string]any{
			"id":              it.ID,
			"ts":              it.TS,
			"req_id":          it.ReqID,
			"model":           it.Model,
			"requested_model": it.RequestedModel,
			"family":          it.Family,
			"api_key_name":    it.APIKeyName,
			"useid":           it.UseID,
			"node_name":       it.NodeName,
			"node_uri":        it.NodeURI,
			"streaming":       it.Streaming,
			"response_mode":   it.ResponseMode,
			"anti_truncate":   it.AntiTruncate,
			// 合成工具传输形态（tool-transport / 空）与响应完整性根因分类。
			"anti_truncate_transport": it.AntiTruncateTransport,
			"integrity":               it.Integrity,
			"success":                 it.Success,
			"status_code":             it.StatusCode,
			"error_reason":            it.ErrorReason,
			"prompt_tokens":           it.PromptTokens,
			"candidates_tokens":       it.CandidatesTokens,
			"total_tokens":            it.TotalTokens,
			"total_ms":                it.TotalMs,
			"ttfb_ms":                 it.TTFBMs,
			"upstream_ttfb_ms":        it.UpstreamTTFBMs,
			"finish_reason":           it.FinishReason,
			"output_ms":               it.OutputMs,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": pg.Total,
		"page":  pg.Page,
		"limit": pg.Limit,
	})
}

// adminGetRacingStats 返回竞速统计：按节点聚合 + 按时间桶的成功率序列。
// query 参数：days（窗口天数，默认 1；0 表示"全部"，下界取保留期）、bucket（hour|day，默认 hour）。
// 口径：一次批量竞速只有胜出者算成功，其余候选（lost/collect/timeout/error）一律视为不成功，
// 故成功率 = wins / attempts。用途：判断各节点竞速胜率，并结合时间区间定位高胜率时段。
func (adm *AdminHandler) adminGetRacingStats(w http.ResponseWriter, r *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	days := int64(1)
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && n <= 3650 {
			days = n
		}
	}
	bucketSec := int64(3600)
	if strings.EqualFold(r.URL.Query().Get("bucket"), "day") {
		bucketSec = 86400
	}
	now := time.Now()
	var from int64
	if days > 0 {
		from = now.Unix() - days*86400
	} else {
		// days=0 表示"全部"：下界取保留期，与节点统计口径一致。
		from = now.Unix() - int64(stats.RetentionDays())*86400
		if from < 0 {
			from = 0
		}
	}
	to := now.Unix() + 1

	byNode, err := store.RaceByNode(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("竞速统计查询失败 (by_node): "+err.Error()))
		return
	}
	series, err := store.RaceSeries(from, to, bucketSec)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("竞速统计查询失败 (series): "+err.Error()))
		return
	}
	// 竞速按节点行同样补上防删锁/禁用状态，供概览 Racing 面板的行内操作使用。
	locked, disabled := adm.nodeFlagSets()
	rows := make([]map[string]any, 0, len(byNode))
	var totAttempts, totWins, totLost, totCollects, totTimeouts, totErrors int
	for _, st := range byNode {
		totAttempts += st.Attempts
		totWins += st.Wins
		totLost += st.Lost
		totCollects += st.Collects
		totTimeouts += st.Timeouts
		totErrors += st.Errors
		rows = append(rows, map[string]any{
			"name":     st.Name,
			"raw_uri":  st.RawURI,
			"locked":   locked[st.RawURI],
			"disabled": disabled[st.RawURI],
			"attempts": st.Attempts,
			"wins":     st.Wins,
			"lost":     st.Lost,
			"collects": st.Collects,
			"timeouts": st.Timeouts,
			"errors":   st.Errors,
			"avg_ms":   st.AvgMs,
		})
	}
	pts := make([]map[string]any, 0, len(series))
	for _, p := range series {
		pts = append(pts, map[string]any{
			"ts":       p.Ts,
			"attempts": p.Attempts,
			"wins":     p.Wins,
			"lost":     p.Lost,
			"collects": p.Collects,
			"timeouts": p.Timeouts,
			"errors":   p.Errors,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":   days,
		"bucket": bucketSec,
		"summary": map[string]any{
			"attempts": totAttempts,
			"wins":     totWins,
			"lost":     totLost,
			"collects": totCollects,
			"timeouts": totTimeouts,
			"errors":   totErrors,
		},
		"by_node":  rows,
		"series":   pts,
		"realtime": stats.Snapshot(),
	})
}

// adminResetRacingStats 清空竞速统计记录。
func (adm *AdminHandler) adminResetRacingStats(w http.ResponseWriter, _ *http.Request) {
	store, ok := adm.requireUsageStore(w)
	if !ok {
		return
	}
	n, err := store.ResetRaceAll()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("重置竞速统计失败 (reset failed)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// usageStore 返回注入的统计仓储；未注入时使用进程内内存兜底（测试/旧装配路径）。
func (adm *AdminHandler) usageStore() *db.UsageStore {
	if s := adm.deps.Stats; s != nil {
		return s
	}
	memStoreOnce.Do(func() {
		if m, err := db.NewMemoryStore(); err == nil {
			memStore = m
		}
	})
	return memStore
}

// requireUsageStore 断言统计仓储可用；不可用时写入 500 并返回 false。
func (adm *AdminHandler) requireUsageStore(w http.ResponseWriter) (*db.UsageStore, bool) {
	s := adm.usageStore()
	if s == nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("统计仓储未初始化 (stats store unavailable)"))
		return nil, false
	}
	return s, true
}

func summaryToMap(s db.UsageSummary) map[string]any {
	return map[string]any{
		"requests":             s.Requests,
		"success":              s.Success,
		"errors":               s.Errors,
		"prompt_tokens":        s.PromptTokens,
		"candidates_tokens":    s.CandidatesTokens,
		"total_tokens":         s.TotalTokens,
		"avg_total_ms":         s.AvgTotalMs,
		"avg_ttfb_ms":          s.AvgTTFBMs,
		"avg_upstream_ttfb_ms": s.AvgUpstreamTTFBMs,
		"avg_output_ms":        s.AvgOutputMs,
	}
}

func rowsToMaps(rows []db.UsageRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"name":              r.Name,
			"raw_uri":           r.RawURI,
			"locked":            r.Locked,
			"disabled":          r.Disabled,
			"requests":          r.Requests,
			"success":           r.Success,
			"errors":            r.Errors,
			"prompt_tokens":     r.PromptTokens,
			"candidates_tokens": r.CandidatesTokens,
			"total_tokens":      r.TotalTokens,
		})
	}
	return out
}

func pointsToMaps(pts []db.UsagePoint) []map[string]any {
	out := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		out = append(out, map[string]any{
			"ts":           p.Ts,
			"requests":     p.Requests,
			"success":      p.Success,
			"errors":       p.Errors,
			"total_tokens": p.TotalTokens,
		})
	}
	return out
}
