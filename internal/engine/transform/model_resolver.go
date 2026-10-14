package transform

import (
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// StreamMode 标识模型家族支持的流式传输模式。
type StreamMode string

const (
	StreamModeTrue      StreamMode = "true_stream"      // 原生真流式 (Text 家族)
	StreamModeAggregate StreamMode = "aggregate_stream" // 聚合单包 SSE (Image 家族降级保护)
	StreamModeForbidden StreamMode = "forbidden"        // 禁用流式 / Unary 专用 (Audio 家族)
)

// ResolvedModel 存储 ModelResolver 解析后的模型元数据与家族策略绑定。
type ResolvedModel struct {
	RawModel       string        // 原始请求模型名 (例: "models/假非流-gemini-image-alias")
	ActualModel    string        // 剥离归一后的 Gemini 真正模型名 (例: "gemini-3.1-flash-image")
	IsFake         bool          // 是否带有假非流修饰（客户端流式、代理聚合成单帧返回）
	IsAntiTruncate bool          // 是否带有抗截断修饰（非流式入口改走真流式竞速引擎再聚合）
	Family         ModelFamily   // FamilyText / FamilyImage / FamilyAudio / FamilyVideo
	Strategy       ModelStrategy // 绑定的家族策略
}

// trimGeminiPathPrefix 清理 GCP 规范前缀 (models/ 与 publishers/*/models/)，不区分大小写。
func trimGeminiPathPrefix(model string) string {
	m := strings.TrimSpace(model)
	lower := strings.ToLower(m)
	if strings.HasPrefix(lower, "models/") {
		return m[len("models/"):]
	}
	if idx := strings.Index(lower, "/models/"); idx != -1 {
		return m[idx+len("/models/"):]
	}
	return m
}

// stripPrefix 从前缀列表中剥离命中的首个前缀。
func stripPrefix(model string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(model, p) {
			return model[len(p):], true
		}
	}
	return model, false
}

// stripVariantPrefixes 循环剥离变体前缀，每种至多一次。
// 支持组合形态（假非流-抗截断-m 与 抗截断-假非流-m 均可解析）；
// 组合仅存在于解析层，/models 清单不暴露组合 variant。
func stripVariantPrefixes(model string, fakePrefixes, antiTruncatePrefixes []string) (string, bool, bool) {
	m := model
	var isFake, isAnti bool
	for {
		changed := false
		if !isFake {
			if s, ok := stripPrefix(m, fakePrefixes); ok {
				m, isFake, changed = s, true, true
			}
		}
		if !isAnti {
			if s, ok := stripPrefix(m, antiTruncatePrefixes); ok {
				m, isAnti, changed = s, true, true
			}
		}
		if !changed {
			return m, isFake, isAnti
		}
	}
}

// ResolveModel 统一解析模型名称，剥离 GCP 前缀与变体前缀，查询别名，并绑定家族与 Strategy。
func ResolveModel(rawModel string, cfg config.ConfigProvider) *ResolvedModel {
	m := trimGeminiPathPrefix(rawModel)

	var isFake, isAnti bool
	if cfg != nil {
		m, isFake, isAnti = stripVariantPrefixes(m, cfg.FakePrefixes(), cfg.AntiTruncatePrefixes())
		m = cfg.ResolveModelName(m)
	} else {
		m, isFake, isAnti = stripVariantPrefixes(m, config.FakePrefixes(), config.AntiTruncatePrefixes())
		m = config.ResolveModelName(m)
	}

	family := FamilyFor(m)
	// 变体修饰仅对文本家族有意义：图像家族本就降级为聚合流，音频家族为 unary。
	if family != FamilyText {
		isFake = false
		isAnti = false
	}
	router := SharedModelFamilyRouter()
	strategy := router.For(m)

	return &ResolvedModel{
		RawModel:       rawModel,
		ActualModel:    m,
		IsFake:         isFake,
		IsAntiTruncate: isAnti,
		Family:         family,
		Strategy:       strategy,
	}
}
