// 本文件实现模型级设置文件 model_settings.json 的加载与写入。
//
// 该文件与 models.json 分离：models.json 仍是"对外暴露的模型清单 + 别名 + 家族覆盖"
// （scripts/更新模型.bat 会按这三个字段整体重写它），本文件保存"本地已知但未启用"
// 的模型与模型级开关，二者互不干扰、不会互相丢字段：
//
//	{
//	  "disabled_models": ["..."],   // 已知但未启用（不对客户端暴露）
//	  "custom_models":   ["..."],   // 本地自定义（上游目录未发现）
//	  "model_options":   {"<模型>": {"anti_truncate": true, "non_stream_prefix": true,
//	                                "trailing_turn_fix": true}}
//	}
//
// 选项字段是**三态指针**：nil = 未显式配置（按家族默认规则生效），显式值优先；
// 读取路径绝不把默认值回写进文件——默认值只作为新建模型时的初始化规则（见管理面板）。
package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ModelOptions 是单个模型的显式开关（nil = 未显式配置，按家族默认生效）。
//
// 三个开关都只作用于文本家族：
//   - AntiTruncate：是否在对外清单暴露「抗截断-」变体（默认 true）；
//   - NonStreamPrefix：是否暴露「假非流-」变体（默认 true）；
//   - TrailingTurnFix：历史以 model 回合结尾时是否追加「继续」user 回合（默认 false）。
type ModelOptions struct {
	AntiTruncate    *bool `json:"anti_truncate,omitempty"`
	NonStreamPrefix *bool `json:"non_stream_prefix,omitempty"`
	TrailingTurnFix *bool `json:"trailing_turn_fix,omitempty"`
}

// IsEmpty 判定三项均未显式配置（用于写盘时清理空条目）。
func (o ModelOptions) IsEmpty() bool {
	return o.AntiTruncate == nil && o.NonStreamPrefix == nil && o.TrailingTurnFix == nil
}

// EffectiveModelOptions 是应用家族默认规则后的生效开关。
type EffectiveModelOptions struct {
	// Applicable 表示模型属文本家族；三个开关都只作用于文本策略，
	// 非文本家族一律不适用（其余字段恒为 false）。
	Applicable      bool
	AntiTruncate    bool
	NonStreamPrefix bool
	TrailingTurnFix bool
}

// ModelSettings 是 model_settings.json 内容结构（导出供管理端组装事务写入输入）。
type ModelSettings struct { //nolint:govet
	DisabledModels []string                `json:"disabled_models"`
	CustomModels   []string                `json:"custom_models"`
	ModelOptions   map[string]ModelOptions `json:"model_options"`
}

var (
	//nolint:gochecknoglobals // Global model settings cache
	settingsMu sync.Mutex
	//nolint:gochecknoglobals // Global model settings cache
	cachedSettings *ModelSettings
	//nolint:gochecknoglobals // Global model settings cache
	settingsCacheTime time.Time
)

// InvalidateModelSettingsCache 强制清除 model_settings.json 缓存
// （SIGHUP 热重载、事务提交与迁移完成后调用）。
func InvalidateModelSettingsCache() {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	cachedSettings = nil
	settingsCacheTime = time.Time{}
}

// ModelSettingsPath 解析 model_settings.json 路径：
// VPROXY_MODEL_SETTINGS 环境变量 > 与 models.json 同目录（保证跟随后者一起迁移/打包）。
func ModelSettingsPath() string {
	if p := os.Getenv("VPROXY_MODEL_SETTINGS"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(modelsPath()), "model_settings.json")
}

// loadModelSettings 读 model_settings.json（带 60 秒缓存）；文件缺失/损坏返回空设置。
//
// 锁序约束：本函数只取 settingsMu，绝不调用 Load()（config.mu）——Load 在持有
// config.mu 时会调用本加载器（尾部兼容迁移），反向调用会形成锁序环。
// 同理，任何调用方在持有本缓存锁期间不得调用 Load。
func loadModelSettings() *ModelSettings {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	if cachedSettings != nil && time.Since(settingsCacheTime) < cacheTTL {
		return cachedSettings
	}

	ms := &ModelSettings{
		DisabledModels: []string{},
		CustomModels:   []string{},
		ModelOptions:   map[string]ModelOptions{},
	}
	if data, err := os.ReadFile(ModelSettingsPath()); err == nil {
		var parsed ModelSettings
		if errUnm := json.Unmarshal(data, &parsed); errUnm != nil { //nolint:govet
			log.Printf("[Config] 解析 model_settings.json 失败: %v", errUnm)
		} else {
			if parsed.DisabledModels != nil {
				ms.DisabledModels = parsed.DisabledModels
			}
			if parsed.CustomModels != nil {
				ms.CustomModels = parsed.CustomModels
			}
			if parsed.ModelOptions != nil {
				ms.ModelOptions = parsed.ModelOptions
			}
		}
	} else if !os.IsNotExist(err) {
		log.Printf("[Config] 读取 model_settings.json 失败: %v", err)
	}
	cachedSettings = ms
	settingsCacheTime = time.Now()
	return ms
}

// DisabledModels 返回"已知但未启用"的模型清单副本。
func DisabledModels() []string {
	ms := loadModelSettings()
	out := make([]string, len(ms.DisabledModels))
	copy(out, ms.DisabledModels)
	return out
}

// CustomModels 返回"本地自定义（上游目录未发现）"的模型清单副本。
func CustomModels() []string {
	ms := loadModelSettings()
	out := make([]string, len(ms.CustomModels))
	copy(out, ms.CustomModels)
	return out
}

// ModelOptionsMap 返回模型 → 显式选项副本（仅含显式值，供 admin 展示/编辑）。
func ModelOptionsMap() map[string]ModelOptions {
	ms := loadModelSettings()
	out := make(map[string]ModelOptions, len(ms.ModelOptions))
	for k, v := range ms.ModelOptions {
		out[k] = v
	}
	return out
}

// ModelSettingsSnapshot 返回当前 model_settings.json 的完整副本（供管理端读改写组装事务输入）。
func ModelSettingsSnapshot() ModelSettings {
	ms := loadModelSettings()
	out := ModelSettings{
		DisabledModels: append([]string(nil), ms.DisabledModels...),
		CustomModels:   append([]string(nil), ms.CustomModels...),
		ModelOptions:   make(map[string]ModelOptions, len(ms.ModelOptions)),
	}
	for k, v := range ms.ModelOptions {
		out.ModelOptions[k] = v
	}
	return out
}

// IsModelDisabled 判定模型是否在未启用集合中。
func IsModelDisabled(model string) bool {
	for _, m := range loadModelSettings().DisabledModels {
		if m == model {
			return true
		}
	}
	return false
}

// IsModelCustom 判定模型是否被标记为本地自定义（上游目录未发现）。
func IsModelCustom(model string) bool {
	for _, m := range loadModelSettings().CustomModels {
		if m == model {
			return true
		}
	}
	return false
}

// legacyTrailingSnapshot 读取尾部回合兼容旧全局键的归一化值，供迁移完成前的只读回退。
//
// 必须在**不持有**任何本包缓存锁时调用（Load 内部会取 config.mu）。
func legacyTrailingSnapshot() (bool, []string) {
	cfg := Load()
	return cfg.TrailingModelFixEnabled, cfg.TrailingFixModels
}

// matchTrailingFixList 复刻尾部兼容清单的匹配语义（大小写不敏感子串匹配，
// 与旧实现 transform.matchTrailingFixModel 一致）；空项与空清单不匹配任何模型。
func matchTrailingFixList(model string, list []string) bool {
	lower := strings.ToLower(model)
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(item)) {
			return true
		}
	}
	return false
}

// computeEffectiveOptions 在已加载的快照上计算生效开关（纯函数，便于测试）：
// 显式值 > 家族默认 > 旧全局键回退。
func computeEffectiveOptions(ms *ModelSettings, family, model string,
	legacyEnabled bool, legacyList []string) EffectiveModelOptions {
	if family != FamilyNameText {
		return EffectiveModelOptions{}
	}
	opt := ms.ModelOptions[model]
	eff := EffectiveModelOptions{Applicable: true, AntiTruncate: true, NonStreamPrefix: true}
	if opt.AntiTruncate != nil {
		eff.AntiTruncate = *opt.AntiTruncate
	}
	if opt.NonStreamPrefix != nil {
		eff.NonStreamPrefix = *opt.NonStreamPrefix
	}
	if opt.TrailingTurnFix != nil {
		eff.TrailingTurnFix = *opt.TrailingTurnFix
	} else {
		eff.TrailingTurnFix = legacyEnabled && matchTrailingFixList(model, legacyList)
	}
	return eff
}

// EffectiveOptions 返回模型在当前配置下的生效开关。
//
// 家族判定走 ModelFamilyOf（family_map 显式覆盖优先）；非文本家族 Applicable=false。
// 注意：本函数依赖 Load()（旧键回退），只可在不持有本包其它缓存锁时调用。
func EffectiveOptions(model string) EffectiveModelOptions {
	mf := loadModelsFile()
	ms := loadModelSettings()
	family := modelFamilyOf(mf, model)
	legacyEnabled, legacyList := legacyTrailingSnapshot()
	return computeEffectiveOptions(ms, family, model, legacyEnabled, legacyList)
}

// TrailingTurnFixEnabled 判定模型是否启用尾部回合兼容（文本策略消费）。
// 非文本家族恒为 false。
func TrailingTurnFixEnabled(model string) bool {
	return EffectiveOptions(model).TrailingTurnFix
}

// normalizeModelOptions 清理空白键与三项全空的空条目。
func normalizeModelOptions(in map[string]ModelOptions) map[string]ModelOptions {
	out := make(map[string]ModelOptions, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" || v.IsEmpty() {
			continue
		}
		out[k] = v
	}
	return out
}

// KnownModels 返回本地已知模型（models ∪ disabled ∪ custom，去重保序）。
//
// models.json 的 models 是"启用/对外暴露"集合；本函数返回"本地管理面"的已知集合，
// 供上游差异计算（新/已有/已移除）与管理面板元数据使用。
func KnownModels() []string {
	return knownModelsFor(loadModelSettings())
}

// knownModelsFor 返回本地已知模型（models ∪ disabled ∪ custom，去重保序）。
// 供尾部兼容迁移与 admin 元数据生成覆盖全部本地模型（含已停用/自定义，避免漏配）。
//
// 注意：内部调用 BaseModels()（modelsMu）；与 settingsMu 顺序获取、不嵌套持有。
func knownModelsFor(ms *ModelSettings) []string {
	seen := make(map[string]bool, len(ms.DisabledModels)+len(ms.CustomModels))
	out := make([]string, 0, len(ms.DisabledModels)+len(ms.CustomModels))
	add := func(list []string) {
		for _, m := range list {
			m = strings.TrimSpace(m)
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	add(BaseModels())
	add(ms.DisabledModels)
	add(ms.CustomModels)
	return out
}

// backupFileIfExists 把现有文件复制为 path+".bak"（用户可恢复备份）；文件缺失视为空操作。
// 与 scripts/更新模型.bat 的 .bak 惯例保持一致。
func backupFileIfExists(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("error: %w", err)
	}
	if werr := os.WriteFile(path+".bak", data, 0o644); werr != nil {
		return fmt.Errorf("error: %w", werr)
	}
	return nil
}

// WriteModelSettings 把本地已知集合与模型选项写回 model_settings.json
// （写前备份 .bak、原子替换），并清空缓存使下次读取即生效。
//
// 注意：管理端保存模型配置走事务入口 config.SaveModelConfigTx（T4），本函数
// 供单文件场景与测试使用。
func WriteModelSettings(disabled, custom []string, options map[string]ModelOptions) error {
	ms := ModelSettings{
		DisabledModels: normalizeModelList(disabled),
		CustomModels:   normalizeModelList(custom),
		ModelOptions:   normalizeModelOptions(options),
	}
	if err := backupFileIfExists(ModelSettingsPath()); err != nil {
		return err
	}
	if err := writeJSONFile(ModelSettingsPath(), ms); err != nil {
		return err
	}
	InvalidateModelSettingsCache()
	return nil
}
