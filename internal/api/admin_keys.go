package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

func (adm *AdminHandler) adminGetKeys(w http.ResponseWriter, _ *http.Request) {
	entries, err := adm.keys.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("读取密钥失败 (failed to read keys)"))
		return
	}
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"name":        e.Name,
			"key":         e.Key,
			"key_masked":  maskKey(e.Key),
			"description": e.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (adm *AdminHandler) adminAddKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		Description string `json:"description"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	key := strings.TrimSpace(body.Key)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, adminErr("名称不能为空 (name is required)"))
		return
	}
	if strings.Contains(name, ":") {
		writeJSON(w, http.StatusBadRequest, adminErr("名称不能包含冒号 (name must not contain ':')"))
		return
	}
	if key == "" {
		key = generateAPIKey()
	} else if !strings.HasPrefix(key, "sk-") {
		writeJSON(w, http.StatusBadRequest, adminErr("密钥必须以 sk- 开头 (key must start with 'sk-')"))
		return
	}
	if err := adm.keys.Add(name, key, body.Description); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("写入密钥失败 (failed to write keys)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}

func (adm *AdminHandler) adminDeleteKey(w http.ResponseWriter, r *http.Request, rawName string) {
	if r.Method != http.MethodDelete {
		adm.adminMethodNotAllowed(w)
		return
	}
	name := rawName
	if dec, err := url.PathUnescape(rawName); err == nil {
		name = dec
	}
	ok, err := adm.keys.Delete(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("删除密钥失败 (failed to delete key)"))
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, adminErr("未找到该密钥 (key not found)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminGetModels 返回模型管理页所需的完整状态：
//
//   - 本地三字段（models/alias_map/family_map，models.json）；
//   - 本地已知集合：disabled_models（已知但未启用）与 custom_models（本地自定义）；
//   - model_options 是**显式**三态值（nil=未配置），model_options_effective 是应用
//     家族默认与旧键回退后的**生效**值——两者分离，前端编辑只回写显式值，
//     避免一次保存把默认值固化成永久配置；
//   - model_meta 给出每个已知模型的家族、来源（override/heuristic）、可用性与原因；
//   - catalog/diff 是上游目录快照状态与三类差异（removed_confirmed 表示"已移除"是否可执行）。
func (adm *AdminHandler) adminGetModels(w http.ResponseWriter, _ *http.Request) {
	known := config.KnownModels()

	meta := make(map[string]any, len(known))
	effective := make(map[string]any, len(known))
	for _, m := range known {
		family := config.ModelFamilyOf(m)
		source := "heuristic"
		if _, ok := config.ModelFamilyOverride(m); ok {
			source = "override"
		}
		supported := family != config.FamilyNameVideo
		entry := map[string]any{
			"family":        family,
			"family_source": source,
			"supported":     supported,
			"custom":        config.IsModelCustom(m),
		}
		if !supported {
			entry["unavailable_reason"] = "视频生成 API 尚未实现"
		}
		meta[m] = entry

		eff := config.EffectiveOptions(m)
		effective[m] = map[string]any{
			"applicable":        eff.Applicable,
			"anti_truncate":     eff.AntiTruncate,
			"non_stream_prefix": eff.NonStreamPrefix,
			"trailing_turn_fix": eff.TrailingTurnFix,
		}
	}

	catalogStatus, diff := catalogView()

	writeJSON(w, http.StatusOK, map[string]any{
		"models":                  config.BaseModels(),
		"alias_map":               config.AliasMap(),
		"family_map":              config.FamilyMap(),
		"disabled_models":         config.DisabledModels(),
		"custom_models":           config.CustomModels(),
		"model_options":           config.ModelOptionsMap(),
		"model_options_effective": effective,
		"model_meta":              meta,
		"families":                config.ValidFamilyNames(),
		"catalog":                 catalogStatus,
		"diff":                    diff,
	})
}

// validFamilyName 校验 family_map 的值是否在允许的家族名内（text/image/audio/vedio）。
func validFamilyName(v string) bool {
	for _, n := range config.ValidFamilyNames() {
		if v == n {
			return true
		}
	}
	return false
}

// modelConfigRequest 是 PUT /api/admin/models 的请求体。
//
// 覆盖语义（写死）：
//   - models/alias_map/family_map：完整替换（与历史行为一致）；
//   - disabled_models/custom_models/model_options：**缺省 = 保持不变**（兼容旧客户端，
//     不会因字段缺失意外清空设置），显式给出（含空数组/空对象）则整字段完整替换。
type modelConfigRequest struct { //nolint:govet // 字段顺序对应接口契约，可读性优先
	Models         []string                        `json:"models"`
	AliasMap       map[string]string               `json:"alias_map"`
	FamilyMap      map[string]string               `json:"family_map"`
	DisabledModels *[]string                       `json:"disabled_models"`
	CustomModels   *[]string                       `json:"custom_models"`
	ModelOptions   *map[string]config.ModelOptions `json:"model_options"`
}

// modelConfigNormalized 是校验归一化后的模型配置（事务写入输入）。
type modelConfigNormalized struct { //nolint:govet // 字段顺序对应写入内容，可读性优先
	Models    []string
	AliasMap  map[string]string
	FamilyMap map[string]string
	Settings  config.ModelSettings
}

// previousKnownModels 返回**上一次**的本地已知集合（models ∪ disabled ∪ custom，去重保序）。
//
// 供"移除模型"的关联配置清理判定：本次提交后不在 models∪disabled 中、且曾在上次集合中的
// 模型即为被移除，其 family 覆盖、model_options、custom 标记与指向它的别名都应一并清理。
func previousKnownModels(settings config.ModelSettings) []string {
	seen := make(map[string]bool, len(settings.DisabledModels)+len(settings.CustomModels))
	out := make([]string, 0, len(settings.DisabledModels)+len(settings.CustomModels))
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
	add(config.BaseModels())
	add(settings.DisabledModels)
	add(settings.CustomModels)
	return out
}

// hasReservedVariantPrefix 判定名称是否以保留的变体前缀开头（假非流-/抗截断-）。
// 基础模型名与别名键命中即拒绝：否则「前缀+同名」会与变体语义产生解析歧义。
func hasReservedVariantPrefix(name string) bool {
	for _, p := range append(append([]string{}, config.FakePrefixes()...), config.AntiTruncatePrefixes()...) {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// dedupeStrings 去空白、去空项、去重（保序）。
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// normalizeModelConfig 校验并归一化一次模型配置提交（四操作语义的服务端守卫）。
//
// 四操作（导入/启用/停用/移除）由管理面板组装为完整状态提交，服务端保证不变量：
//   - 启用清单非空且去重；启用与停用互斥（交集 → 400）；
//   - 基础模型名与别名键不得使用保留变体前缀；别名不得指向另一个别名（禁链）、
//     不得遮蔽已知模型（同名歧义）；
//   - 视频家族（vedio）不得出现在启用清单（后端能力未实现，禁止启用）；
//     注：family_map 显式覆盖优先，`veo-*` 被显式声明为 text 属允许行为（不另设硬规则）；
//   - **移除模型的关联配置全量清理**（服务端最终不变量，不依赖管理面板）：
//     custom_models / model_options[model] / family_map[model] / 指向该模型的别名，
//     凡属"上次已知、本次不在 models∪disabled"的模型，一并清理，避免日后重新导入时
//     复活旧的 Family/别名配置；
//   - 从未在任何本地清单出现过的模型的 family_map 声明属于前瞻配置，不属移除、保持原样；
//   - 家族值域含 vedio；alias/family 的空键空值静默丢弃（历史行为）。
//
// prevKnown 是**上一次**的本地已知集合（models ∪ disabled ∪ custom），由调用方在写入前读取。
func normalizeModelConfig(models []string, aliasMap, familyMap map[string]string,
	disabled, custom []string, options map[string]config.ModelOptions,
	prevKnown []string) (modelConfigNormalized, error) {
	var out modelConfigNormalized

	models = dedupeStrings(models)
	if len(models) == 0 {
		return out, errors.New("模型列表不能为空 (models must not be empty)")
	}
	disabled = dedupeStrings(disabled)

	enabledSet := make(map[string]bool, len(models))
	for _, m := range models {
		enabledSet[m] = true
	}
	for _, m := range disabled {
		if enabledSet[m] {
			return out, fmt.Errorf("模型 %s 同时出现在启用与停用清单中 (model appears in both enabled and disabled lists)", m)
		}
	}
	knownSet := make(map[string]bool, len(models)+len(disabled))
	for _, m := range models {
		knownSet[m] = true
	}
	for _, m := range disabled {
		knownSet[m] = true
	}
	for m := range knownSet {
		if hasReservedVariantPrefix(m) {
			return out, fmt.Errorf("模型名不能以保留前缀开头：%s（假非流-/抗截断- 用于变体语义）(reserved prefix)", m)
		}
	}

	// 本次"移除"的模型：上次已知、本次不在 models∪disabled（停用不算移除，配置保留）。
	removedSet := make(map[string]bool)
	for _, m := range prevKnown {
		if !knownSet[m] {
			removedSet[m] = true
		}
	}

	// custom_models 裁剪到已知集合：移除模型即自动清除自定义标记，避免"已移除的模型
	// 因残留标记继续出现在本地自定义列表中"（防复活）。
	custom = dedupeStrings(custom)
	customKept := make([]string, 0, len(custom))
	for _, m := range custom {
		if knownSet[m] {
			customKept = append(customKept, m)
		}
	}

	family := map[string]string{}
	for k, v := range familyMap {
		k, v = strings.TrimSpace(k), strings.ToLower(strings.TrimSpace(v))
		if k == "" || v == "" {
			continue
		}
		if removedSet[k] {
			continue // 已移除模型的家族覆盖随移除一并清理
		}
		if !validFamilyName(v) {
			return out, fmt.Errorf("家族名非法：%s=%s，只允许 %s (invalid family name)",
				k, v, strings.Join(config.ValidFamilyNames(), " / "))
		}
		family[k] = v
	}

	// 别名键集合（用于禁链判定）：以归一化后的键为准。
	aliasKeys := make(map[string]bool, len(aliasMap))
	for k := range aliasMap {
		if k = strings.TrimSpace(k); k != "" {
			aliasKeys[k] = true
		}
	}
	alias := map[string]string{}
	for k, v := range aliasMap {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		if removedSet[v] {
			continue // 指向已移除模型的别名随移除一并清理
		}
		if hasReservedVariantPrefix(k) {
			return out, fmt.Errorf("别名不能以保留前缀开头：%s（会与变体语义冲突）(reserved prefix)", k)
		}
		if v != k && aliasKeys[v] {
			return out, fmt.Errorf("别名不得指向另一个别名：%s → %s（请直接指向真实模型名）(alias chains are not allowed)", k, v)
		}
		if knownSet[k] {
			return out, fmt.Errorf("别名 %s 与已知模型同名，会造成解析歧义 (alias collides with a known model)", k)
		}
		alias[k] = v
	}

	for _, m := range models {
		if config.FamilyNameWithOverrides(m, family) == config.FamilyNameVideo {
			return out, fmt.Errorf("视频模型暂不支持启用：%s（视频生成 API 尚未实现）(video models cannot be enabled)", m)
		}
	}

	opts := map[string]config.ModelOptions{}
	for k, v := range options {
		k = strings.TrimSpace(k)
		if k == "" || v.IsEmpty() || !knownSet[k] {
			continue
		}
		opts[k] = v
	}

	out.Models = models
	out.AliasMap = alias
	out.FamilyMap = family
	out.Settings = config.ModelSettings{
		DisabledModels: disabled,
		CustomModels:   customKept,
		ModelOptions:   opts,
	}
	return out, nil
}

func (adm *AdminHandler) adminPutModels(w http.ResponseWriter, r *http.Request) {
	var body modelConfigRequest
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}

	// 新字段缺省 = 保持磁盘现值（兼容旧客户端：不会因字段缺失意外清空设置）。
	current := config.ModelSettingsSnapshot()
	disabled, custom, options := current.DisabledModels, current.CustomModels, current.ModelOptions
	if body.DisabledModels != nil {
		disabled = *body.DisabledModels
	}
	if body.CustomModels != nil {
		custom = *body.CustomModels
	}
	if body.ModelOptions != nil {
		options = *body.ModelOptions
	}

	normalized, nerr := normalizeModelConfig(body.Models, body.AliasMap, body.FamilyMap, disabled, custom, options,
		previousKnownModels(current))
	if nerr != nil {
		writeJSON(w, http.StatusBadRequest, adminErr(nerr.Error()))
		return
	}

	// 双文件事务写入：任一失败都回滚为两侧原内容（含崩溃前滚收敛，见 config.SaveModelConfigTx）。
	if err := config.SaveModelConfigTx(normalized.Models, normalized.AliasMap, normalized.FamilyMap, normalized.Settings); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("写入模型失败 (failed to write models)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
