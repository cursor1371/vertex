package config

import (
	"strings"
	"testing"
)

func TestDefaultModelsContainsAllExpected(t *testing.T) {
	expected := []string{
		"gemini-3.1-flash-tts-preview",
		"gemini-3.1-flash-lite-image",
		"gemini-3.5-flash-lite",
		"gemini-3.6-flash",
	}
	for _, m := range expected {
		if !contains(defaultModels, m) {
			t.Errorf("defaultModels 缺少模型: %s", m)
		}
	}
}

func TestModelsWithVariants(t *testing.T) {
	variants := ModelsWithVariants()
	// 文本模型应该有假非流前缀变体
	if !contains(variants, "假非流-gemini-3.6-flash") {
		t.Errorf("ModelsWithVariants 缺少文本模型的假非流变体: 假非流-gemini-3.6-flash")
	}
	// 文本模型也应该有抗截断前缀变体
	if !contains(variants, "抗截断-gemini-3.6-flash") {
		t.Errorf("ModelsWithVariants 缺少文本模型的抗截断变体: 抗截断-gemini-3.6-flash")
	}
	// 图片和语音模型不应该有假非流前缀变体
	if contains(variants, "假非流-gemini-3.1-flash-image") {
		t.Errorf("ModelsWithVariants 不应包含图像模型的假非流变体: 假非流-gemini-3.1-flash-image")
	}
	if contains(variants, "假非流-gemini-3.1-flash-tts-preview") {
		t.Errorf("ModelsWithVariants 不应包含语音模型的假非流变体: 假非流-gemini-3.1-flash-tts-preview")
	}
	// 非文本模型同样不应有抗截断变体
	if contains(variants, "抗截断-gemini-3.1-flash-image") {
		t.Errorf("ModelsWithVariants 不应包含图像模型的抗截断变体: 抗截断-gemini-3.1-flash-image")
	}
	if contains(variants, "抗截断-gemini-3.1-flash-tts-preview") {
		t.Errorf("ModelsWithVariants 不应包含语音模型的抗截断变体: 抗截断-gemini-3.1-flash-tts-preview")
	}
	// 不产出组合变体：两者作用于相反端点，组合会让清单膨胀。
	for _, m := range variants {
		if strings.Contains(m, "抗截断-假非流-") || strings.Contains(m, "假非流-抗截断-") {
			t.Errorf("ModelsWithVariants 不应产出组合变体: %s", m)
		}
	}
}
