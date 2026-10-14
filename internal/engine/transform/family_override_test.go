package transform

import (
	"path/filepath"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestFamilyOverrideDrivesStrategy 验证 models.json 的 family_map 显式声明
// 能驱动 FamilyFor / IsImageModel / ResolveModel，纠正名字启发式的误判。
func TestFamilyOverrideDrivesStrategy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	defer config.InvalidateModelsCache()

	// custom-img 名字不含 image，声明为 image；gemini-3.1-flash-image 名字含
	// image，反向声明为 text，检验覆盖双向生效。
	if err := config.WriteModels(
		[]string{"custom-img", "custom-text", "gemini-3.1-flash-image"},
		map[string]string{},
		map[string]string{
			"custom-img":             config.FamilyNameImage,
			"gemini-3.1-flash-image": config.FamilyNameText,
		},
	); err != nil {
		t.Fatalf("WriteModels: %v", err)
	}

	if got := FamilyFor("custom-img"); got != FamilyImage {
		t.Errorf("FamilyFor(custom-img) = %v, want FamilyImage", got)
	}
	if !IsImageModel("custom-img") {
		t.Errorf("IsImageModel(custom-img) = false, want true")
	}
	if got := FamilyFor("gemini-3.1-flash-image"); got != FamilyText {
		t.Errorf("FamilyFor(gemini-3.1-flash-image) = %v, want FamilyText", got)
	}
	if IsImageModel("gemini-3.1-flash-image") {
		t.Errorf("IsImageModel(gemini-3.1-flash-image) = true, want false")
	}

	// 未声明模型仍按启发式：nano-banana 归图像、tts 归音频。
	if got := FamilyFor("gemini-nano-banana-2.1"); got != FamilyImage {
		t.Errorf("FamilyFor(gemini-nano-banana-2.1) = %v, want FamilyImage", got)
	}
	if got := FamilyFor("gemini-2.5-flash-tts"); got != FamilyAudio {
		t.Errorf("FamilyFor(gemini-2.5-flash-tts) = %v, want FamilyAudio", got)
	}
}

// TestResolveModelUsesFamilyOverride 验证解析结果里 Family 与 Strategy 一致，
// 不会出现「Family 说图像、Strategy 却是文本」的分裂。
func TestResolveModelUsesFamilyOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	defer config.InvalidateModelsCache()

	if err := config.WriteModels(
		[]string{"custom-img"},
		map[string]string{},
		map[string]string{"custom-img": config.FamilyNameImage},
	); err != nil {
		t.Fatalf("WriteModels: %v", err)
	}

	r := ResolveModel("custom-img", nil)
	if r.Family != FamilyImage {
		t.Fatalf("ResolvedModel.Family = %v, want FamilyImage", r.Family)
	}
	if _, ok := r.Strategy.(*ImageStrategy); !ok {
		t.Fatalf("ResolvedModel.Strategy = %T, want *ImageStrategy", r.Strategy)
	}
}
