package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// setupModelFiles 把三个配置文件（config/models/model_settings）指向同一临时目录并清空全部缓存。
func setupModelFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	t.Setenv("VPROXY_MODEL_SETTINGS", filepath.Join(dir, "model_settings.json"))
	t.Setenv("VPROXY_CONFIG", filepath.Join(dir, "config.json"))
	InvalidateModelsCache()
	InvalidateModelSettingsCache()
	InvalidateCache()
	t.Cleanup(func() {
		InvalidateModelsCache()
		InvalidateModelSettingsCache()
		InvalidateCache()
	})
	return dir
}

func optBool(v bool) *bool { return &v }

func writeRawFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

func readConfigRaw(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("读取 config.json 失败: %v", err)
	}
	raw := map[string]any{}
	if uerr := json.Unmarshal(data, &raw); uerr != nil {
		t.Fatalf("解析 config.json 失败: %v", uerr)
	}
	return raw
}

func readSettingsRaw(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "model_settings.json"))
	if err != nil {
		t.Fatalf("读取 model_settings.json 失败: %v", err)
	}
	raw := map[string]any{}
	if uerr := json.Unmarshal(data, &raw); uerr != nil {
		t.Fatalf("解析 model_settings.json 失败: %v", uerr)
	}
	return raw
}

// TestFamilyVideoHeuristicAndValidNames 验证 vedio 家族：veo 名字启发式 + 值域纳入。
func TestFamilyVideoHeuristicAndValidNames(t *testing.T) {
	setupModelFiles(t)

	cases := map[string]string{
		"veo-3.0-generate-preview": FamilyNameVideo,
		"veo-2.0-generate-001":     FamilyNameVideo,
		"GEMINI-VEO-X":             FamilyNameVideo, // 大小写不敏感
		"gemini-3.6-flash":         FamilyNameText,
		"gemini-3.1-flash-image":   FamilyNameImage,
	}
	for model, want := range cases {
		if got := ModelFamilyOf(model); got != want {
			t.Errorf("ModelFamilyOf(%q) = %q, want %q", model, got, want)
		}
	}
	if got := HeuristicFamilyName("veo-3.0-generate-preview"); got != FamilyNameVideo {
		t.Errorf("HeuristicFamilyName(veo) = %q, want %q", got, FamilyNameVideo)
	}
	if !contains(ValidFamilyNames(), FamilyNameVideo) {
		t.Errorf("ValidFamilyNames 应包含 %q，got %v", FamilyNameVideo, ValidFamilyNames())
	}
}

// TestDefaultModelsArgonWithoutMerge 验证 gemini-4-argon 进入默认清单，
// 但已有 models.json 不会被默认清单合并补齐（用户移除的模型不复活）。
func TestDefaultModelsArgonWithoutMerge(t *testing.T) {
	if !contains(defaultModels, "gemini-4-argon") {
		t.Fatal("defaultModels 应包含 gemini-4-argon")
	}
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"), `{"models":["gemini-3.6-flash"]}`)
	InvalidateModelsCache()

	got := BaseModels()
	if contains(got, "gemini-4-argon") {
		t.Fatalf("已有 models.json 不应被默认清单合并补齐，got %v", got)
	}
	// 即使不在上游目录，gemini-4-argon 的家族启发式也必须落在 text，避免走错策略。
	if fam := ModelFamilyOf("gemini-4-argon"); fam != FamilyNameText {
		t.Fatalf("gemini-4-argon 家族应为 text，got %q", fam)
	}
}

// TestEffectiveOptionsDefaultsAndExplicit 验证三态语义：
// 文本家族默认（anti/non 开、trailing 关）、显式值优先、非文本家族不适用。
func TestEffectiveOptionsDefaultsAndExplicit(t *testing.T) {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"),
		`{"models":["gemini-3.6-flash","gemini-3.1-flash-image"]}`)
	InvalidateModelsCache()

	eff := EffectiveOptions("gemini-3.6-flash")
	if !eff.Applicable || !eff.AntiTruncate || !eff.NonStreamPrefix || eff.TrailingTurnFix {
		t.Fatalf("文本家族默认生效值不符: %+v", eff)
	}

	img := EffectiveOptions("gemini-3.1-flash-image")
	if img.Applicable || img.AntiTruncate || img.NonStreamPrefix || img.TrailingTurnFix {
		t.Fatalf("非文本家族不应适用任何开关: %+v", img)
	}

	if err := WriteModelSettings(nil, nil, map[string]ModelOptions{
		"gemini-3.6-flash": {AntiTruncate: optBool(false), TrailingTurnFix: optBool(true)},
	}); err != nil {
		t.Fatalf("WriteModelSettings: %v", err)
	}
	eff = EffectiveOptions("gemini-3.6-flash")
	if eff.AntiTruncate {
		t.Error("显式 anti_truncate=false 未生效")
	}
	if !eff.NonStreamPrefix {
		t.Error("未显式配置的 non_stream_prefix 应保持默认 true")
	}
	if !eff.TrailingTurnFix {
		t.Error("显式 trailing_turn_fix=true 未生效")
	}
}

// TestModelSettingsReadDoesNotWrite 读取路径绝不创建/改写 model_settings.json。
func TestModelSettingsReadDoesNotWrite(t *testing.T) {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"), `{"models":["gemini-3.6-flash"]}`)
	InvalidateModelsCache()

	_ = DisabledModels()
	_ = CustomModels()
	_ = ModelOptionsMap()
	_ = EffectiveOptions("gemini-3.6-flash")
	_ = TrailingTurnFixEnabled("gemini-3.6-flash")
	_ = ModelsWithVariants()

	if _, err := os.Stat(filepath.Join(dir, "model_settings.json")); !os.IsNotExist(err) {
		t.Fatalf("读取路径不应创建 model_settings.json, stat err=%v", err)
	}
}

// TestModelsWithVariantsRespectsOptions 变体按模型级显式选项增删；vedio 家族整体不暴露。
func TestModelsWithVariantsRespectsOptions(t *testing.T) {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"),
		`{"models":["gemini-3.6-flash","gemini-3.7-flash","veo-3.0-generate-preview","gemini-3.1-flash-image"]}`)
	InvalidateModelsCache()
	if err := WriteModelSettings(nil, nil, map[string]ModelOptions{
		"gemini-3.6-flash": {AntiTruncate: optBool(false)},
		"gemini-3.7-flash": {AntiTruncate: optBool(false), NonStreamPrefix: optBool(false)},
	}); err != nil {
		t.Fatalf("WriteModelSettings: %v", err)
	}

	v := ModelsWithVariants()
	absent := []string{
		"抗截断-gemini-3.6-flash",     // anti 显式关闭
		"抗截断-gemini-3.7-flash",     // anti 显式关闭
		"假非流-gemini-3.7-flash",     // non 显式关闭
		"veo-3.0-generate-preview", // vedio 不暴露
		"抗截断-veo-3.0-generate-preview",
		"假非流-veo-3.0-generate-preview",
		"假非流-gemini-3.1-flash-image", // 非文本无变体
	}
	for _, name := range absent {
		if contains(v, name) {
			t.Errorf("ModelsWithVariants 不应包含 %q, got %v", name, v)
		}
	}
	present := []string{"gemini-3.6-flash", "假非流-gemini-3.6-flash", "gemini-3.7-flash"}
	for _, name := range present {
		if !contains(v, name) {
			t.Errorf("ModelsWithVariants 应包含 %q, got %v", name, v)
		}
	}
}

// TestWriteModelSettingsBackupAndContent 单文件写入：内容正确、写前备份 .bak、原子替换。
func TestWriteModelSettingsBackupAndContent(t *testing.T) {
	dir := setupModelFiles(t)
	path := filepath.Join(dir, "model_settings.json")

	if err := WriteModelSettings([]string{" disabled-m "}, []string{"custom-m"}, map[string]ModelOptions{
		"custom-m": {NonStreamPrefix: optBool(false)},
	}); err != nil {
		t.Fatalf("WriteModelSettings: %v", err)
	}
	raw := readSettingsRaw(t, dir)
	disabled, _ := raw["disabled_models"].([]any)
	if len(disabled) != 1 || disabled[0] != "disabled-m" {
		t.Fatalf("disabled_models 应去空格: %v", raw["disabled_models"])
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatal("首次写入（原文件不存在）不应产生 .bak")
	}

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if werr := WriteModelSettings([]string{"other"}, nil, nil); werr != nil {
		t.Fatalf("第二次 WriteModelSettings: %v", werr)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("第二次写入应产生 .bak: %v", err)
	}
	if !bytes.Equal(bak, first) {
		t.Fatal(".bak 应等于写入前的内容")
	}
}

// TestMigrateTrailingFixWritesExplicitAndRemovesKeys 迁移覆盖 models ∪ disabled ∪ custom，
// 显式值不被覆盖，成功后旧键移除且二次加载幂等。
func TestMigrateTrailingFixWritesExplicitAndRemovesKeys(t *testing.T) {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"),
		`{"models":["gemini-3.6-flash","gemini-3.5-flash-lite"]}`)
	writeRawFile(t, filepath.Join(dir, "model_settings.json"),
		`{"disabled_models":["gemini-3.5-flash-lite"],"custom_models":["my-custom-flash"],`+
			`"model_options":{"gemini-3.6-flash":{"trailing_turn_fix":false}}}`)
	writeRawFile(t, filepath.Join(dir, "config.json"),
		`{"trailing_model_fix_enabled":true,"trailing_fix_models":["3.6-flash","3.5-flash-lite","custom-flash"],"max_n":8}`)

	Load() // 触发迁移

	raw := readConfigRaw(t, dir)
	if _, ok := raw[trailingFixEnabledKey]; ok {
		t.Fatal("迁移成功后 config.json 不应保留 trailing_model_fix_enabled")
	}
	if _, ok := raw[trailingFixModelsKey]; ok {
		t.Fatal("迁移成功后 config.json 不应保留 trailing_fix_models")
	}
	if raw["max_n"] != float64(8) {
		t.Fatalf("迁移不应影响其它配置键, max_n=%v", raw["max_n"])
	}

	settings := readSettingsRaw(t, dir)
	opts, _ := settings["model_options"].(map[string]any)
	if opts == nil {
		t.Fatalf("迁移应写入 model_options, got %v", settings)
	}
	// 显式 false 不被旧全局开关覆盖
	if v, ok := opts["gemini-3.6-flash"].(map[string]any); !ok || v["trailing_turn_fix"] != false {
		t.Fatalf("已存在的显式 false 不应被迁移覆盖: %v", opts["gemini-3.6-flash"])
	}
	// 停用集合中的命中模型与自定义命中模型都应补上显式 true
	for _, m := range []string{"gemini-3.5-flash-lite", "my-custom-flash"} {
		v, ok := opts[m].(map[string]any)
		if !ok || v["trailing_turn_fix"] != true {
			t.Fatalf("模型 %s 应迁移写入显式 true, got %v", m, opts[m])
		}
	}
	// 旧键回退与新显式值在迁移后行为一致
	if !TrailingTurnFixEnabled("gemini-3.5-flash-lite") {
		t.Fatal("迁移后回退应保持尾部兼容生效")
	}
	if TrailingTurnFixEnabled("gemini-3.6-flash") {
		t.Fatal("显式 false 迁移后仍应保持关闭")
	}

	// 幂等：清缓存重新 Load 不再改写任何文件
	before := snapshotFiles(t, dir, "config.json", "model_settings.json")
	InvalidateCache()
	InvalidateModelSettingsCache()
	InvalidateModelsCache()
	Load()
	after := snapshotFiles(t, dir, "config.json", "model_settings.json")
	for name, b := range before {
		if !bytes.Equal(b, after[name]) {
			t.Fatalf("二次加载应幂等，%s 被改写", name)
		}
	}
}

// TestMigrateTrailingFixDisabledOnlyRemovesKeys 旧开关为关闭：只删旧键、不创建设置文件。
func TestMigrateTrailingFixDisabledOnlyRemovesKeys(t *testing.T) {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"), `{"models":["gemini-3.6-flash"]}`)
	writeRawFile(t, filepath.Join(dir, "config.json"),
		`{"trailing_model_fix_enabled":false,"trailing_fix_models":["gemini-3.6-flash"],"max_n":8}`)

	Load()

	raw := readConfigRaw(t, dir)
	if _, ok := raw[trailingFixEnabledKey]; ok {
		t.Fatal("关闭态也应移除旧键")
	}
	if _, ok := raw[trailingFixModelsKey]; ok {
		t.Fatal("关闭态也应移除旧清单键")
	}
	if _, err := os.Stat(filepath.Join(dir, "model_settings.json")); !os.IsNotExist(err) {
		t.Fatal("关闭态迁移不应创建 model_settings.json")
	}
	if TrailingTurnFixEnabled("gemini-3.6-flash") {
		t.Fatal("关闭态尾部兼容不应生效")
	}
}

// TestTrailingFixMigrationFailureKeepsLegacyFallback 迁移写入失败：
// 旧键保留、运行时按旧键回退继续生效；清障后重启迁移成功。
func TestTrailingFixMigrationFailureKeepsLegacyFallback(t *testing.T) {
	dir := setupModelFiles(t)
	writeRawFile(t, filepath.Join(dir, "models.json"), `{"models":["gemini-3.6-flash"]}`)
	writeRawFile(t, filepath.Join(dir, "config.json"),
		`{"trailing_model_fix_enabled":true,"trailing_fix_models":["3.6-flash"]}`)

	// 故障注入：目标路径先占为目录，令备份/写入失败（跨平台通用）。
	settingsPath := filepath.Join(dir, "model_settings.json")
	if err := os.MkdirAll(settingsPath, 0o755); err != nil {
		t.Fatal(err)
	}

	Load()

	raw := readConfigRaw(t, dir)
	if _, ok := raw[trailingFixEnabledKey]; !ok {
		t.Fatal("迁移失败时不得删除旧键")
	}
	if !TrailingTurnFixEnabled("gemini-3.6-flash") {
		t.Fatal("迁移失败时应按旧键回退继续生效")
	}

	// 清障后重试 → 迁移成功
	if err := os.RemoveAll(settingsPath); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	InvalidateModelSettingsCache()
	Load()
	raw = readConfigRaw(t, dir)
	if _, ok := raw[trailingFixEnabledKey]; ok {
		t.Fatal("清障后应迁移成功并删除旧键")
	}
	if !TrailingTurnFixEnabled("gemini-3.6-flash") {
		t.Fatal("迁移成功后尾部兼容应继续生效（转为显式值）")
	}
}

// TestMigrateTrailingFixMatchSemantics 锁定旧清单匹配语义：大小写不敏感子串、空项忽略。
func TestMigrateTrailingFixMatchSemantics(t *testing.T) {
	if !matchTrailingFixList("Gemini-3.6-Flash", []string{"3.6-flash"}) {
		t.Error("匹配应大小写不敏感")
	}
	if !matchTrailingFixList("gemini-3.6-flash", []string{"flash"}) {
		t.Error("匹配应为子串语义")
	}
	if matchTrailingFixList("gemini-3.6-flash", []string{"", "  "}) {
		t.Error("空项不应匹配任何模型")
	}
	if matchTrailingFixList("gemini-3.6-flash", nil) {
		t.Error("空清单不应匹配任何模型")
	}
}

func snapshotFiles(t *testing.T, dir string, names ...string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("快照 %s 失败: %v", name, err)
		}
		out[name] = data
	}
	return out
}
