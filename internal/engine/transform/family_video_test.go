package transform

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// setupVideoFamilyConfig 建立可控的 models.json（含别名与 family_map 覆盖），
// 并把 models 缓存指向临时文件；测试结束自动失效缓存避免污染同包其它用例。
func setupVideoFamilyConfig(t *testing.T, models []string, alias, family map[string]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VPROXY_MODELS", filepath.Join(dir, "models.json"))
	config.InvalidateModelsCache()
	t.Cleanup(config.InvalidateModelsCache)
	if err := config.WriteModels(models, alias, family); err != nil {
		t.Fatalf("WriteModels: %v", err)
	}
}

// TestVideoFamilyHeuristicAndOverride 视频家族（vedio）的识别：
// veo 名字启发式命中；family_map 显式覆盖双向生效（含把 veo 名字反声明为 text）。
func TestVideoFamilyHeuristicAndOverride(t *testing.T) {
	setupVideoFamilyConfig(t,
		[]string{"veo-3.0-generate-preview", "clip-model", "veo-named-text", "gemini-3.6-flash"},
		nil,
		map[string]string{
			"clip-model":     config.FamilyNameVideo, // 名字无 veo，显式声明为视频
			"veo-named-text": config.FamilyNameText,  // 名字含 veo，显式纠正为文本
		},
	)

	if got := FamilyFor("veo-3.0-generate-preview"); got != FamilyVideo {
		t.Errorf("FamilyFor(veo-3.0-generate-preview) = %v, want FamilyVideo", got)
	}
	if got := FamilyFor("clip-model"); got != FamilyVideo {
		t.Errorf("显式声明 vedio 未生效: FamilyFor(clip-model) = %v", got)
	}
	if got := FamilyFor("veo-named-text"); got != FamilyText {
		t.Errorf("显式声明 text 应覆盖 veo 启发式: FamilyFor(veo-named-text) = %v", got)
	}
	if got := FamilyFor("gemini-3.6-flash"); got != FamilyText {
		t.Errorf("FamilyFor(gemini-3.6-flash) = %v, want FamilyText", got)
	}
}

// TestResolveVideoModelNormalizationContract 统一规范化契约：
// 基础名 / 别名 / 变体 ID 走同一套解析规则，结果确定且与家族判定一致。
func TestResolveVideoModelNormalizationContract(t *testing.T) {
	setupVideoFamilyConfig(t,
		[]string{"veo-3.0-generate-preview", "gemini-3.6-flash"},
		map[string]string{"my-video": "veo-3.0-generate-preview"},
		nil,
	)

	// 基础名 → 视频家族 + 视频策略（Family 与 Strategy 不允许分裂）
	r := ResolveModel("veo-3.0-generate-preview", nil)
	if r.Family != FamilyVideo {
		t.Fatalf("Family = %v, want FamilyVideo", r.Family)
	}
	if _, ok := r.Strategy.(*VideoStrategy); !ok {
		t.Fatalf("Strategy = %T, want *VideoStrategy", r.Strategy)
	}

	// 变体前缀：非文本家族强制清空变体标志（前缀剥离但语义不生效）
	r = ResolveModel("抗截断-veo-3.0-generate-preview", nil)
	if r.ActualModel != "veo-3.0-generate-preview" || r.Family != FamilyVideo {
		t.Fatalf("抗截断-veo 解析错误: %+v", r)
	}
	if r.IsAntiTruncate || r.IsFake {
		t.Fatalf("非文本家族不得携带变体标志: IsAntiTruncate=%v IsFake=%v", r.IsAntiTruncate, r.IsFake)
	}

	// 组合前缀：两种书写顺序结果确定且等价（每种前缀至多剥离一次）
	a := ResolveModel("假非流-抗截断-veo-3.0-generate-preview", nil)
	b := ResolveModel("抗截断-假非流-veo-3.0-generate-preview", nil)
	if a.ActualModel != "veo-3.0-generate-preview" || b.ActualModel != a.ActualModel {
		t.Fatalf("组合前缀解析应等价: a=%q b=%q", a.ActualModel, b.ActualModel)
	}
	if a.Family != FamilyVideo || b.Family != FamilyVideo {
		t.Fatalf("组合前缀应归视频家族: a=%v b=%v", a.Family, b.Family)
	}

	// 别名 → 视频家族；前缀 + 别名（变体 ID 与别名组合）同样可解析
	r = ResolveModel("my-video", nil)
	if r.ActualModel != "veo-3.0-generate-preview" || r.Family != FamilyVideo {
		t.Fatalf("别名指向视频模型解析错误: %+v", r)
	}
	r = ResolveModel("抗截断-my-video", nil)
	if r.ActualModel != "veo-3.0-generate-preview" || r.Family != FamilyVideo {
		t.Fatalf("前缀 + 别名解析错误: %+v", r)
	}

	// GCP 路径前缀
	r = ResolveModel("models/veo-3.0-generate-preview", nil)
	if r.ActualModel != "veo-3.0-generate-preview" || r.Family != FamilyVideo {
		t.Fatalf("GCP 路径前缀解析错误: %+v", r)
	}

	// 回归锚点：文本模型的变体语义不受本次改动影响
	tr := ResolveModel("抗截断-gemini-3.6-flash", nil)
	if tr.Family != FamilyText || !tr.IsAntiTruncate || tr.ActualModel != "gemini-3.6-flash" {
		t.Fatalf("文本变体解析被破坏: %+v", tr)
	}
}

// TestVideoStrategyRejects 视频策略是"能力未实现"的拒绝占位：
// 任何误入 pipeline 的调用在 Validate 阶段即被拒绝。
func TestVideoStrategyRejects(t *testing.T) {
	vs := &VideoStrategy{model: "veo-3.0-generate-preview"}
	if vs.Family() != FamilyVideo {
		t.Errorf("Family() = %v, want FamilyVideo", vs.Family())
	}
	if vs.FamilyStreamMode() != StreamModeForbidden {
		t.Errorf("FamilyStreamMode() = %v, want StreamModeForbidden", vs.FamilyStreamMode())
	}
	err := vs.Validate(&GeminiRequest{})
	if err == nil || !strings.Contains(err.Error(), "尚未实现") {
		t.Errorf("Validate 应返回「视频未实现」错误, got %v", err)
	}
	if vs.IsValidChunk(&GeminiChunk{}) || vs.IsValidResponse(&GeminiResponse{}) {
		t.Error("视频策略不应参与竞速胜出判定")
	}
	if v := vs.BuildVariables("veo-3.0-generate-preview", &GeminiRequest{}, nil); v == nil {
		t.Error("BuildVariables 防御性实现不应返回 nil")
	}
	// 路由器对视频模型返回视频策略
	if _, ok := SharedModelFamilyRouter().For("veo-3.0-generate-preview").(*VideoStrategy); !ok {
		t.Error("Router.For 未返回 VideoStrategy")
	}
}
