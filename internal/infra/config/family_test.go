package config

import (
	"path/filepath"
	"testing"
)

// TestModelFamilyOfHeuristic 验证纯名字启发式的三条分支（含 nano-banana 特例）。
func TestModelFamilyOfHeuristic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	InvalidateModelsCache()
	defer InvalidateModelsCache()

	cases := map[string]string{
		"gemini-2.5-flash":             FamilyNameText,
		"gemini-3.8-flash":             FamilyNameText,
		"gemini-3.1-flash-tts-preview": FamilyNameAudio,
		"gemini-2.5-flash-tts":         FamilyNameAudio,
		"gemini-3.1-flash-image":       FamilyNameImage,
		"gemini-nano-banana-2.1":       FamilyNameImage, // 名称不含 image，靠启发式识别
		"some-imagen-model":            FamilyNameImage,
		"unknown-model-12345":          FamilyNameText,
	}
	for model, want := range cases {
		if got := ModelFamilyOf(model); got != want {
			t.Errorf("ModelFamilyOf(%q) = %q, want %q", model, got, want)
		}
	}
}

// TestFamilyMapOverrideWins 验证 family_map 显式声明优先于名字启发式，
// 且声明为 image/audio 的模型不再被追加文本变体（假非流- / 抗截断-）。
func TestFamilyMapOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	InvalidateModelsCache()
	defer InvalidateModelsCache()

	models := []string{"custom-img", "custom-snd", "gemini-3.1-flash-image", "plain-text"}
	family := map[string]string{
		"custom-img":             FamilyNameImage, // 名字无 image，显式纠正为图像
		"custom-snd":             FamilyNameAudio,
		"gemini-3.1-flash-image": FamilyNameText, // 名字含 image，显式纠正为文本
	}
	if err := WriteModels(models, map[string]string{}, family); err != nil {
		t.Fatalf("WriteModels: %v", err)
	}

	if got := ModelFamilyOf("custom-img"); got != FamilyNameImage {
		t.Errorf("显式 image 声明未生效，got %q", got)
	}
	if got := ModelFamilyOf("custom-snd"); got != FamilyNameAudio {
		t.Errorf("显式 audio 声明未生效，got %q", got)
	}
	if got := ModelFamilyOf("gemini-3.1-flash-image"); got != FamilyNameText {
		t.Errorf("显式 text 声明未覆盖名字启发式，got %q", got)
	}
	if got := ModelFamilyOf("plain-text"); got != FamilyNameText {
		t.Errorf("未声明模型应按启发式判为 text，got %q", got)
	}

	// 显式声明优先的判定必须传导到变体展开：image/audio 不生成文本变体。
	variants := ModelsWithVariants()
	for _, name := range []string{"假非流-custom-img", "抗截断-custom-img", "假非流-custom-snd", "抗截断-custom-snd"} {
		if contains(variants, name) {
			t.Errorf("非文本家族不应有变体 %q，got %v", name, variants)
		}
	}
	// 显式声明为 text 的图模型应生成文本变体。
	if !contains(variants, "假非流-gemini-3.1-flash-image") {
		t.Errorf("显式 text 声明的模型应生成变体，got %v", variants)
	}
	if !contains(variants, "假非流-plain-text") {
		t.Errorf("文本模型应生成变体，got %v", variants)
	}
}

// TestModelFamilyOverrideAccessor 验证「是否显式声明」的区分能力。
func TestModelFamilyOverrideAccessor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	InvalidateModelsCache()
	defer InvalidateModelsCache()

	if err := WriteModels([]string{"a", "b"}, map[string]string{}, map[string]string{"a": FamilyNameImage}); err != nil {
		t.Fatal(err)
	}
	if got, ok := ModelFamilyOverride("a"); !ok || got != FamilyNameImage {
		t.Errorf("a 应有显式声明 image，got (%q,%v)", got, ok)
	}
	if _, ok := ModelFamilyOverride("b"); ok {
		t.Errorf("b 不应有显式声明")
	}
}
