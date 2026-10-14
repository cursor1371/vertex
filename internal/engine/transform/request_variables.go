package transform

import (
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// 本文件是"强类型请求 → 上游 variables"的构建层。
// 输入是已增强的 GeminiRequest，通过各家族 Strategy 分发构建对应的强类型 variables 结构。

// BuildGeminiVariablesTyped 由强类型请求按模型家族 Strategy 构建发往上游的强类型 variables 结构体。
func BuildGeminiVariablesTyped(model string, req *GeminiRequest, cfg config.ConfigProvider) *GeminiVariables {
	router := SharedModelFamilyRouter()
	strategy := router.For(model)
	return strategy.BuildVariables(model, req, cfg)
}

// packParallelToolResponses 专门处理 FunctionResponse 的打包：
// 相邻且仅包含 FunctionResponse 的多个 Content turn，强制打包平滑合并在同一个 Content (role="user") 中，
// 满足 Vertex AI 上游对于多工具调用 (Parallel Tool Calls) 需打包在同一个 turn 的强要求。
func packParallelToolResponses(contents []Content) []Content {
	if len(contents) == 0 {
		return contents
	}
	merged := make([]Content, 0, len(contents))
	for _, c := range contents {
		currIsFR := partsIsOnlyFunctionResponsesTyped(c.Parts)
		if !currIsFR {
			merged = append(merged, c)
			continue
		}
		if len(merged) > 0 {
			prev := &merged[len(merged)-1]
			if partsIsOnlyFunctionResponsesTyped(prev.Parts) {
				// Parts 与请求侧底层数组共享，禁止 append 原地扩容，必须新建切片承接。
				mergedParts := make([]Part, 0, len(prev.Parts)+len(c.Parts))
				mergedParts = append(mergedParts, prev.Parts...)
				mergedParts = append(mergedParts, c.Parts...)
				prev.Parts = mergedParts
				continue
			}
		}
		cCopy := c
		cCopy.Role = "user"
		merged = append(merged, cCopy)
	}
	return merged
}

// mergeContiguousTextRoles 专门处理普通非 FunctionResponse 消息：
// 仅对相邻的普通同 Role（user->user 或 model->model）消息做 Part 级平滑合并。
// 任何包含 FunctionResponse 的 Content turn 保持独立隔离，不与普通文本合并。
func mergeContiguousTextRoles(contents []Content) []Content {
	if len(contents) == 0 {
		return contents
	}
	merged := make([]Content, 0, len(contents))
	for _, c := range contents {
		if len(merged) == 0 {
			merged = append(merged, c)
			continue
		}
		prev := &merged[len(merged)-1]
		if c.Role != prev.Role {
			merged = append(merged, c)
			continue
		}
		if partsContainFunctionResponseTyped(prev.Parts) || partsContainFunctionResponseTyped(c.Parts) {
			merged = append(merged, c)
			continue
		}
		// Parts 与请求侧底层数组共享，禁止 append 原地扩容，必须新建切片承接。
		mergedParts := make([]Part, 0, len(prev.Parts)+len(c.Parts))
		mergedParts = append(mergedParts, prev.Parts...)
		mergedParts = append(mergedParts, c.Parts...)
		prev.Parts = mergedParts
	}
	return merged
}

// mergeContiguousRolesTyped 组合 packParallelToolResponses 与 mergeContiguousTextRoles。
func mergeContiguousRolesTyped(contents []Content) []Content {
	if len(contents) == 0 {
		return contents
	}
	packed := packParallelToolResponses(contents)
	return mergeContiguousTextRoles(packed)
}

// sanitizeContentRolesTyped 将 contents 中 Role 为空或纯空白的 Content
// 兜底为 "user"，满足 Vertex AI 上游对 role 字段的非空强要求。
// 仅补全空值，不改动已有的合法 role（user/model）。
//
// 纯函数契约：竞速的每个候选 goroutine 并行调用 BuildVariables，而本函数最初
// 收到的正是共享的 req.Contents——必须先浅拷贝 Content 结构体切片再写 Role，
// 否则会对请求侧底层数组产生并发写（data race）。拷贝后 len==cap，调用方随后
// 的 append 也必然扩容到新数组，不会写穿共享底层数组。
func sanitizeContentRolesTyped(contents []Content) []Content {
	out := make([]Content, len(contents))
	copy(out, contents)
	for i := range out {
		if strings.TrimSpace(out[i].Role) == "" {
			out[i].Role = "user"
		}
	}
	return out
}

// filterEmptyContentsTyped 丢弃无任何有效字段的 part，并丢弃清洗后 parts 为空的 content。
func filterEmptyContentsTyped(contents []Content) []Content {
	var filtered []Content
	for i := range contents {
		parts := make([]Part, 0, len(contents[i].Parts))
		for _, p := range contents[i].Parts {
			if partEmptyTyped(p) {
				continue
			}
			parts = append(parts, p)
		}
		if len(parts) == 0 {
			continue
		}
		c := contents[i]
		c.Parts = parts
		filtered = append(filtered, c)
	}
	return filtered
}

// partEmptyTyped 判断 part 是否不含任何有效内容字段。
func partEmptyTyped(p Part) bool {
	if p.Text != "" || p.Thought {
		return false
	}
	if p.InlineData != nil || p.FileData != nil {
		return false
	}
	if p.FunctionCall != nil || p.FunctionResponse != nil {
		return false
	}
	if p.ExecutableCode != nil || p.CodeExecutionResult != nil {
		return false
	}
	return p.VideoMetadata == nil && p.MediaResolution == ""
}

// extractTextTyped 汇总 Content 中所有纯文本 part 的文本。
func extractTextTyped(c *Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// contentsHasUser 判断 contents 中是否存在 user 回合。
func contentsHasUser(contents []Content) bool {
	for _, c := range contents {
		if c.Role == "user" {
			return true
		}
	}
	return false
}

// endsWithModelTurnTyped 判断对话历史是否以模型回合（model）结尾。
func endsWithModelTurnTyped(contents []Content) bool {
	if len(contents) == 0 {
		return false
	}
	role := contents[len(contents)-1].Role
	return role == "model" || role == "assistant"
}

// partsContainFunctionResponseTyped 判断 parts 中是否存在含 functionResponse 的 part。
func partsContainFunctionResponseTyped(parts []Part) bool {
	for _, p := range parts {
		if p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// partsIsOnlyFunctionResponsesTyped 判断 parts 是否非空且所有 part 均为 functionResponse。
func partsIsOnlyFunctionResponsesTyped(parts []Part) bool {
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if p.FunctionResponse == nil {
			return false
		}
	}
	return true
}

// prepareNativeTools 把 Tools 中各 FunctionDeclaration 的 Parameters
// 转换为私有 GraphQL 端点 (google_cloud_aiplatform_ui_Schema) 认可的原生 Map-style Schema，
// 包含将 type 转换为全大写 (OBJECT/STRING/NUMBER 等) 及 properties 转为 key/value 数组。
// 转换完成后过滤空工具兜底，防止边界场景泄漏空 Tool 至上游。
func prepareNativeTools(tools []Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if IsToolEmpty(t) {
			continue
		}
		toolCopy := t
		if len(t.FunctionDeclarations) > 0 {
			decls := make([]FunctionDeclaration, len(t.FunctionDeclarations))
			for j, decl := range t.FunctionDeclarations {
				declCopy := decl
				if declCopy.Parameters != nil {
					cleaned := cleanFunctionParameters(declCopy.Parameters)
					declCopy.Parameters = toNativeSchema(cleaned)
				}
				decls[j] = declCopy
			}
			toolCopy.FunctionDeclarations = decls
		}
		out = append(out, toolCopy)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// prepareNativeToolConfig 规范化 ToolConfig，确保 functionCallingConfig.mode 为 Vertex AI 上游 GraphQL 支持的值。
// 如果客户端传入非标准 mode（如 "VALIDATED" / "validated"），将其归一化为 "AUTO"。
//
// 纯函数契约：竞速候选并行调用，tc 经 req.ToolConfig 与请求侧共享，
// 写 Mode 必须 copy-on-write，禁止写穿共享指针（AllowedFunctionNames 下游仅读取，浅拷贝共享）。
func prepareNativeToolConfig(tc *ToolConfig) *ToolConfig {
	if tc == nil || tc.FunctionCallingConfig == nil {
		return tc
	}
	fcc := *tc.FunctionCallingConfig
	mode := strings.ToUpper(strings.TrimSpace(fcc.Mode))
	switch mode {
	case "AUTO", "NONE", "ANY":
		fcc.Mode = mode
	default:
		// 上游仅支持 AUTO, NONE, ANY；非法/未知 mode 兜底为 AUTO
		fcc.Mode = "AUTO"
	}
	out := *tc
	out.FunctionCallingConfig = &fcc
	return &out
}

// prepareNativeGenerationConfig 规范化 GenerationConfig 中的枚举字段（MediaResolution, ThinkingLevel, ResponseModalities）。
//
// 纯函数契约：竞速候选并行调用，gc 经 req.GenerationConfig 与请求侧共享，
// 任何字段写入都必须落在私有副本上（ResponseModalities 本就新建切片，赋值也须写入副本）。
func prepareNativeGenerationConfig(gc *GenerationConfig) *GenerationConfig {
	if gc == nil {
		return nil
	}
	out := *gc
	if gc.ThinkingConfig != nil {
		tcc := *gc.ThinkingConfig
		out.ThinkingConfig = &tcc
	}

	// 1. 媒体分辨率 (MediaResolution) 处理
	if out.MediaResolution != "" {
		trimmed := strings.ToUpper(strings.TrimSpace(out.MediaResolution))
		switch trimmed {
		case "LOW":
			out.MediaResolution = "MEDIA_RESOLUTION_LOW"
		case "MEDIUM":
			out.MediaResolution = "MEDIA_RESOLUTION_MEDIUM"
		case "HIGH":
			out.MediaResolution = "MEDIA_RESOLUTION_HIGH"
		case "MEDIA_RESOLUTION_LOW", "MEDIA_RESOLUTION_MEDIUM", "MEDIA_RESOLUTION_HIGH", "MEDIA_RESOLUTION_UNSPECIFIED":
			out.MediaResolution = trimmed
		default:
			out.MediaResolution = trimmed
		}
	}

	// 2. 思考配置 (ThinkingConfig) 处理
	if out.ThinkingConfig != nil && out.ThinkingConfig.ThinkingLevel != "" {
		level := strings.ToUpper(strings.TrimSpace(out.ThinkingConfig.ThinkingLevel))
		switch level {
		case "LOW", "MEDIUM", "HIGH", "NONE", "MINIMAL", "THINKING_LEVEL_UNSPECIFIED":
			out.ThinkingConfig.ThinkingLevel = level
		default:
			out.ThinkingConfig.ThinkingLevel = "MEDIUM"
		}
	}

	// 3. 响应模态 (ResponseModalities) 处理
	if len(out.ResponseModalities) > 0 {
		modalities := make([]string, len(out.ResponseModalities))
		for i, m := range out.ResponseModalities {
			modalities[i] = strings.ToUpper(strings.TrimSpace(m))
		}
		out.ResponseModalities = modalities
	}

	return &out
}
