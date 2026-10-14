package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// adminModelsFixture 建立模型管理接口测试环境（临时 models/settings 文件 + 测试服务器），
// 返回 fixture、模型目录与已登录 cookie。
func adminModelsFixture(t *testing.T, modelsJSON, settingsJSON string) (*testFixture, string, string) {
	t.Helper()
	dir := t.TempDir()
	if modelsJSON == "" {
		modelsJSON = `{"models":["gemini-3.6-flash"],"alias_map":{},"family_map":{}}`
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(modelsJSON), 0o644); err != nil {
		t.Fatalf("写入 models.json: %v", err)
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "model_settings.json"), []byte(settingsJSON), 0o644); err != nil {
			t.Fatalf("写入 model_settings.json: %v", err)
		}
	}
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	config.InvalidateModelSettingsCache()
	t.Cleanup(func() {
		config.InvalidateModelsCache()
		config.InvalidateModelSettingsCache()
	})

	fx := newTestServerCustomMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"results":[{"data":{}}]}]`))
	}, func(cfg *config.AppConfig) {
		cfg.AdminPassword = "test-admin-pw"
	})
	return fx, dir, adminLogin(t, fx.server.URL)
}

func putModels(t *testing.T, srvURL, cookie string, payload any) (int, map[string]any) {
	t.Helper()
	resp := adminModelsRequest(t, srvURL, cookie, http.MethodPut, payload)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("解析 PUT 响应失败: %v (body=%s)", err, string(raw))
		}
	}
	return resp.StatusCode, out
}

func getModels(t *testing.T, srvURL, cookie string) map[string]any {
	t.Helper()
	resp := adminModelsRequest(t, srvURL, cookie, http.MethodGet, nil)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 GET 响应失败: %v (body=%s)", err, string(raw))
	}
	return out
}

// TestAdminPutModels_Validation 写盘前校验（四操作语义的服务端守卫）：
// 保留前缀、别名禁链/遮蔽、vedio 启用、启用停用交集、家族值域——非法提交一律 400 且不落盘。
func TestAdminPutModels_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cases := []struct { //nolint:govet // 测试表：字段顺序按可读性优先
		name    string
		payload map[string]any
	}{
		{
			name: "模型名以保留前缀开头",
			payload: map[string]any{
				"models": []string{"抗截断-gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
			},
		},
		{
			name: "别名键以保留前缀开头",
			payload: map[string]any{
				"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{"假非流-x": "gemini-3.6-flash"}, "family_map": map[string]string{},
			},
		},
		{
			name: "别名指向另一个别名（禁链）",
			payload: map[string]any{
				"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{"a": "b", "b": "gemini-3.6-flash"}, "family_map": map[string]string{},
			},
		},
		{
			name: "别名遮蔽已知模型",
			payload: map[string]any{
				"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{"gemini-3.6-flash": "gemini-3.7-flash"}, "family_map": map[string]string{},
			},
		},
		{
			name: "视频模型不得启用",
			payload: map[string]any{
				"models": []string{"gemini-3.6-flash", "veo-3.0-generate-preview"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
			},
		},
		{
			name: "family_map 覆盖为 vedio 后不得启用",
			payload: map[string]any{
				"models": []string{"clip-model"}, "alias_map": map[string]string{}, "family_map": map[string]string{"clip-model": "vedio"},
			},
		},
		{
			name: "启用与停用交集",
			payload: map[string]any{
				"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
				"disabled_models": []string{"gemini-3.6-flash"},
			},
		},
		{
			name: "家族值非法（video 拼写）",
			payload: map[string]any{
				"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{"gemini-3.6-flash": "video"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, dir, cookie := adminModelsFixture(t, "", "")
			modelsBefore := readFileBytes(t, filepath.Join(dir, "models.json"))

			status, resp := putModels(t, fx.server.URL, cookie, tc.payload)
			if status != http.StatusBadRequest {
				t.Fatalf("状态码应为 400，实际 %d (resp=%v)", status, resp)
			}
			if got := readFileBytes(t, filepath.Join(dir, "models.json")); string(got) != string(modelsBefore) {
				t.Fatalf("非法提交不得写盘:\n%s", got)
			}
		})
	}
}

// TestAdminPutModels_NewFieldSemantics 新字段覆盖语义：
// 缺省=保持不变（兼容旧客户端）；显式给出（含空）则整字段替换；
// options/custom 自动裁剪到已知集合（移除模型即清理其选项与自定义标记，防复活）。
func TestAdminPutModels_NewFieldSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	fx, dir, cookie := adminModelsFixture(t,
		`{"models":["gemini-3.6-flash","gemini-3.7-flash"],"alias_map":{},"family_map":{}}`,
		`{"disabled_models":["gemini-old"],"custom_models":["gemini-3.7-flash"],`+
			`"model_options":{"gemini-3.7-flash":{"trailing_turn_fix":true}}}`)

	// ① 旧客户端只带三字段：新字段保持不变（不得意外清空）
	status, resp := putModels(t, fx.server.URL, cookie, map[string]any{
		"models": []string{"gemini-3.6-flash", "gemini-3.7-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
	})
	if status != http.StatusOK {
		t.Fatalf("旧客户端提交应成功: %d %v", status, resp)
	}
	got := getModels(t, fx.server.URL, cookie)
	if arr := asStringSlice(t, got["disabled_models"]); len(arr) != 1 || arr[0] != "gemini-old" {
		t.Fatalf("disabled_models 缺省应保持不变: %v", got["disabled_models"])
	}
	if _, ok := got["model_options"].(map[string]any)["gemini-3.7-flash"]; !ok {
		t.Fatalf("model_options 缺省应保持不变: %v", got["model_options"])
	}
	if !config.TrailingTurnFixEnabled("gemini-3.7-flash") {
		t.Fatal("尾部兼容开关应保持显式 true")
	}

	// ② 移除模型：启用清单去掉 3.7 后，其自定义标记与选项自动清理（防复活）
	status, resp = putModels(t, fx.server.URL, cookie, map[string]any{
		"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
	})
	if status != http.StatusOK {
		t.Fatalf("移除提交应成功: %d %v", status, resp)
	}
	settingsRaw := readJSONMap(t, filepath.Join(dir, "model_settings.json"))
	if opts, ok := settingsRaw["model_options"].(map[string]any); ok && len(opts) != 0 {
		t.Fatalf("被移除模型的选项应自动清理: %v", opts)
	}
	if arr, ok := settingsRaw["custom_models"].([]any); ok && len(arr) != 0 {
		t.Fatalf("被移除模型的自定义标记应自动清理: %v", arr)
	}
	for _, m := range config.KnownModels() {
		if m == "gemini-3.7-flash" {
			t.Fatalf("被移除模型不应留在本地已知集合: %v", config.KnownModels())
		}
	}

	// ③ 显式空对象=清空（整字段替换）
	status, resp = putModels(t, fx.server.URL, cookie, map[string]any{
		"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
		"disabled_models": []string{}, "custom_models": []string{}, "model_options": map[string]any{},
	})
	if status != http.StatusOK {
		t.Fatalf("显式清空应成功: %d %v", status, resp)
	}
	got = getModels(t, fx.server.URL, cookie)
	if arr := asStringSlice(t, got["disabled_models"]); len(arr) != 0 {
		t.Fatalf("显式空数组应清空 disabled_models: %v", arr)
	}

	// ④ options 键不在已知集合 → 静默丢弃
	status, resp = putModels(t, fx.server.URL, cookie, map[string]any{
		"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
		"model_options": map[string]any{"not-a-known-model": map[string]any{"anti_truncate": false}},
	})
	if status != http.StatusOK {
		t.Fatalf("未知键选项应被忽略而非报错: %d %v", status, resp)
	}
	settingsRaw = readJSONMap(t, filepath.Join(dir, "model_settings.json"))
	if opts, ok := settingsRaw["model_options"].(map[string]any); ok && len(opts) != 0 {
		t.Fatalf("未知模型的选项应被丢弃: %v", opts)
	}
}

// TestAdminGetModels_ExtendedState GET 返回显式/生效选项、模型元数据、目录状态与差异。
func TestAdminGetModels_ExtendedState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	fx, _, cookie := adminModelsFixture(t,
		`{"models":["gemini-3.6-flash","veo-x-listed","gemini-4-argon"],"alias_map":{},"family_map":{"veo-x-listed":"vedio"}}`,
		`{"disabled_models":[],"custom_models":["gemini-4-argon"],"model_options":{"gemini-3.6-flash":{"anti_truncate":false}}}`)

	// 预置可信快照：上游含 3.6 与一个新模型，不含 argon（本地自定义）
	if err := config.SaveCatalogSnapshot(&config.CatalogSnapshot{
		FetchedAt: "2026-10-10T00:00:00Z", Source: "publishers/google", Complete: true,
		Models: []string{"gemini-3.6-flash", "gemini-3.9-flash"},
	}); err != nil {
		t.Fatalf("预置快照: %v", err)
	}

	got := getModels(t, fx.server.URL, cookie)

	// 显式值（只有 anti_truncate=false 是显式）与生效值分离
	explicit, _ := got["model_options"].(map[string]any)
	entry, _ := explicit["gemini-3.6-flash"].(map[string]any)
	if entry["anti_truncate"] != false {
		t.Fatalf("显式选项应含 anti_truncate=false: %v", explicit)
	}
	if _, exists := entry["non_stream_prefix"]; exists {
		t.Fatalf("未显式配置的项不应出现在显式值中: %v", entry)
	}
	eff, _ := got["model_options_effective"].(map[string]any)
	effEntry, _ := eff["gemini-3.6-flash"].(map[string]any)
	if effEntry["anti_truncate"] != false || effEntry["non_stream_prefix"] != true {
		t.Fatalf("生效值应体现显式覆盖与默认: %v", effEntry)
	}

	// 模型元数据：家族来源、可用性、自定义标记
	meta, _ := got["model_meta"].(map[string]any)
	veoMeta, _ := meta["veo-x-listed"].(map[string]any)
	if veoMeta["family"] != "vedio" {
		t.Fatalf("vedio 元数据不符: %v", veoMeta)
	}
	if veoMeta["supported"] != false || veoMeta["unavailable_reason"] == nil {
		t.Fatalf("vedio 应不可用并给出原因: %v", veoMeta)
	}
	if veoMeta["family_source"] != "override" {
		t.Fatalf("family_source 应为 override: %v", veoMeta)
	}
	argonMeta, _ := meta["gemini-4-argon"].(map[string]any)
	if argonMeta["custom"] != true {
		t.Fatalf("gemini-4-argon 应标注为本地自定义: %v", argonMeta)
	}

	// 目录状态与差异：本地自定义未发现单列；removed_confirmed 可信
	cat, _ := got["catalog"].(map[string]any)
	if cat["count"] != float64(2) || cat["stale"] != false {
		t.Fatalf("catalog 状态不符: %v", cat)
	}
	diff, _ := got["diff"].(map[string]any)
	if arr := asStringSlice(t, diff["new"]); len(arr) != 1 || arr[0] != "gemini-3.9-flash" {
		t.Fatalf("diff.new 不符: %v", diff["new"])
	}
	if arr := asStringSlice(t, diff["custom_unseen"]); len(arr) != 1 || arr[0] != "gemini-4-argon" {
		t.Fatalf("diff.custom_unseen 不符: %v", diff["custom_unseen"])
	}
	if arr := asStringSlice(t, diff["removed"]); len(arr) != 1 || arr[0] != "veo-x-listed" {
		t.Fatalf("diff.removed 不符: %v", diff["removed"])
	}
	if diff["removed_confirmed"] != true {
		t.Fatalf("可信快照下 removed_confirmed 应为 true: %v", diff)
	}

	// 最近一次拉取失败 → stale 且 removed 不可执行
	snap, _ := config.LoadCatalogSnapshot()
	snap.LastError = "upstream boom"
	if err := config.SaveCatalogSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	got = getModels(t, fx.server.URL, cookie)
	cat, _ = got["catalog"].(map[string]any)
	if cat["stale"] != true || cat["last_error"] != "upstream boom" {
		t.Fatalf("stale 状态不符: %v", cat)
	}
	diff, _ = got["diff"].(map[string]any)
	if diff["removed_confirmed"] != false {
		t.Fatalf("拉取失败后 removed 不应可执行: %v", diff)
	}
}

// TestAdminPutModels_ImmediateEffect 保存即时生效：PUT 后客户端模型列表立即反映启停与变体开关。
func TestAdminPutModels_ImmediateEffect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	fx, _, cookie := adminModelsFixture(t,
		`{"models":["gemini-3.6-flash","gemini-3.7-flash"],"alias_map":{},"family_map":{}}`, "")

	status, resp := putModels(t, fx.server.URL, cookie, map[string]any{
		"models": []string{"gemini-3.6-flash"}, "alias_map": map[string]string{}, "family_map": map[string]string{},
		"model_options": map[string]any{
			"gemini-3.6-flash": map[string]any{"anti_truncate": false},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("PUT 失败: %d %v", status, resp)
	}

	req, err := http.NewRequest(http.MethodGet, fx.server.URL+"/v1beta/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-key")
	listResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listResp.Body.Close() }()
	raw, _ := io.ReadAll(listResp.Body)
	body := string(raw)

	if !strings.Contains(body, "gemini-3.6-flash") || !strings.Contains(body, "假非流-gemini-3.6-flash") {
		t.Fatalf("启用模型与其假非流变体应可见: %s", body)
	}
	if strings.Contains(body, "抗截断-gemini-3.6-flash") {
		t.Fatalf("显式关闭抗截断后变体不应可见: %s", body)
	}
	if strings.Contains(body, "gemini-3.7-flash") {
		t.Fatalf("被停用模型不应可见: %s", body)
	}
}

// TestAdminPutModels_RemovalCleansRelatedConfig 服务端"移除模型"最终不变量（评审要求）：
// 即使绕过管理面板直接提交（残留被移除模型的 family 覆盖、指向它的别名、custom 标记与
// 开关配置），保存后这些关联项也必须被清理；而从未在本地清单出现过的模型的 family 声明
// 属于前瞻配置，不属移除、保持原样。
func TestAdminPutModels_RemovalCleansRelatedConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	fx, dir, cookie := adminModelsFixture(t,
		`{"models":["gemini-3.6-flash","gemini-3.7-flash"],`+
			`"alias_map":{"alias-b":"gemini-3.7-flash","alias-a":"gemini-3.6-flash"},`+
			`"family_map":{"gemini-3.6-flash":"text","gemini-3.7-flash":"image"}}`,
		`{"disabled_models":[],"custom_models":["gemini-3.7-flash"],`+
			`"model_options":{"gemini-3.7-flash":{"trailing_turn_fix":true}}}`)

	// 模拟"绕过面板"的提交：移除 gemini-3.7-flash，但提交里仍带它的全部关联配置。
	status, resp := putModels(t, fx.server.URL, cookie, map[string]any{
		"models": []string{"gemini-3.6-flash"},
		"alias_map": map[string]string{
			"alias-b": "gemini-3.7-flash", // 指向被移除模型 → 应清理
			"alias-a": "gemini-3.6-flash", // 指向保留模型 → 应保留
		},
		"family_map": map[string]string{
			"gemini-3.6-flash": "text",
			"gemini-3.7-flash": "image", // 被移除模型 → 应清理
			"future-model":     "image", // 从未出现过的模型（前瞻声明）→ 应保留
		},
		"disabled_models": []string{},
		"custom_models":   []string{"gemini-3.7-flash"},                                                  // 残留自定义标记 → 应清理
		"model_options":   map[string]any{"gemini-3.7-flash": map[string]any{"trailing_turn_fix": true}}, // 残留选项 → 应清理
	})
	if status != http.StatusOK {
		t.Fatalf("移除提交应成功: %d %v", status, resp)
	}

	modelsRaw := readJSONMap(t, filepath.Join(dir, "models.json"))
	familyRaw, _ := modelsRaw["family_map"].(map[string]any)
	if _, exists := familyRaw["gemini-3.7-flash"]; exists {
		t.Fatalf("已移除模型的 family 覆盖应被清理: %v", familyRaw)
	}
	if familyRaw["future-model"] != "image" {
		t.Fatalf("前瞻声明的 family 覆盖应保留: %v", familyRaw)
	}
	aliasRaw, _ := modelsRaw["alias_map"].(map[string]any)
	if _, exists := aliasRaw["alias-b"]; exists {
		t.Fatalf("指向已移除模型的别名应被清理: %v", aliasRaw)
	}
	if aliasRaw["alias-a"] != "gemini-3.6-flash" {
		t.Fatalf("指向保留模型的别名应保留: %v", aliasRaw)
	}

	got := getModels(t, fx.server.URL, cookie)
	if arr := asStringSlice(t, got["custom_models"]); len(arr) != 0 {
		t.Fatalf("残留的 custom 标记应被清理: %v", arr)
	}
	if opts, ok := got["model_options"].(map[string]any); ok && len(opts) != 0 {
		t.Fatalf("残留的 model_options 应被清理: %v", opts)
	}
	for _, m := range config.KnownModels() {
		if m == "gemini-3.7-flash" {
			t.Fatalf("被移除模型不应留在本地已知集合: %v", config.KnownModels())
		}
	}
}

// readJSONMap 读取 JSON 文件为 map（api 测试用）。
func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(readFileBytes(t, path), &out); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return out
}
