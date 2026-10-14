package vertex

import (
	"context"
	"encoding/json"
	"log"
	"strconv"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
)

// CountTokens 统计给定 contents 在指定模型下的 token 数（纯本地离线估算）。
//
// 不再发起 HTTP BatchGraphQL 请求，改为根据字符类型和媒体 Part 估算：
//   - ASCII 字符：0.25 token/字符（len(ascii) / 4）
//   - 非 ASCII 字符（中文/Emoji/日韩文等）：1.5 token/字符（nonAsciiCount * 3 / 2）
//   - 媒体/图片 Part（inlineData / fileData）：固定 1024 token
//
// 返回估算值，0 表示空 contents。
func (c *VertexAIClient) CountTokens(_ context.Context, model string, contents []transform.Content) int {
	result := estimateTokensTyped(contents)
	if c.cfg.DebugMode() {
		log.Printf("[Vertex] [CountTokens] 离线估算: 模型=%s, tokens=%d", model, result)
	}
	return result
}

// EstimatePartsTokens 对 parts 做本地 token 估算（导出给 api 层统计输出用量）。
// 与 CountTokens 使用同一套折算规则，保证输入/输出两侧口径一致。
func EstimatePartsTokens(parts []transform.Part) int {
	return estimatePartsTokensTyped(parts)
}

// EstimateRequestTokens 估算一次请求的输入 token：contents + systemInstruction + tools。
// 匿名端点不回 usageMetadata，输入侧只能本地折算；遗漏 systemInstruction/tools 会系统性低估。
func EstimateRequestTokens(req *transform.GeminiRequest) int {
	if req == nil {
		return 0
	}
	total := estimateTokensTyped(req.Contents)
	if req.SystemInstruction != nil {
		total += estimatePartsTokensTyped(req.SystemInstruction.Parts)
	}
	for _, t := range req.Tools {
		total += estimateToolTokens(t)
	}
	return total
}

// estimateToolTokens 估算单个工具声明的 token 开销（函数名/描述/参数 schema 序列化）。
func estimateToolTokens(t transform.Tool) int {
	total := 0
	for _, fd := range t.FunctionDeclarations {
		total += estimateTextTokens(fd.Name)
		total += estimateTextTokens(fd.Description)
		total += estimateJSONTokens(fd.Parameters)
	}
	// 内置工具（googleSearch / codeExecution 等）无显式声明体，按固定开销估算。
	if t.GoogleSearch != nil || t.GoogleSearchRetrieval != nil || t.CodeExecution != nil ||
		t.Retrieval != nil || t.URLContext != nil || t.ComputerUse != nil ||
		t.MCPTool != nil || t.FileSearch != nil || t.GoogleMaps != nil {
		total += 32
	}
	return total
}

// estimateTokensTyped 对强类型 contents 进行纯本地 token 估算（零 map 中转）。
func estimateTokensTyped(contents []transform.Content) int {
	total := 0
	for _, cm := range contents {
		total += estimatePartsTokensTyped(cm.Parts)
	}
	return total
}

// estimatePartsTokensTyped 统计单个 content 内 parts 的 token 估算。
// 覆盖文本、内联/文件媒体，以及工具调用、工具结果、可执行代码等结构化 part，
// 避免"纯 functionCall 响应"被折算成 0 输出 token。
func estimatePartsTokensTyped(parts []transform.Part) int {
	total := 0
	for _, p := range parts {
		if p.Text != "" {
			total += estimateTextTokens(p.Text)
		}
		if p.InlineData != nil {
			total += 1024
		} else if p.FileData != nil {
			total += 1024
		}
		if p.FunctionCall != nil {
			total += estimateTextTokens(p.FunctionCall.Name)
			total += estimateJSONTokens(p.FunctionCall.Args)
		}
		if p.FunctionResponse != nil {
			total += estimateTextTokens(p.FunctionResponse.Name)
			total += estimateJSONTokens(p.FunctionResponse.Response)
		}
		if p.ExecutableCode != nil {
			total += estimateTextTokens(p.ExecutableCode.Code)
			total += estimateTextTokens(p.ExecutableCode.CodeLanguage)
		}
		if p.CodeExecutionResult != nil {
			total += estimateTextTokens(p.CodeExecutionResult.Output)
		}
	}
	return total
}

// estimateJSONTokens 估算任意值序列化后的 token 数；nil 返回 0，字符串直接按文本折算。
func estimateJSONTokens(v any) int {
	if v == nil {
		return 0
	}
	if s, ok := v.(string); ok {
		return estimateTextTokens(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateTextTokens(string(b))
}

func estimateTextTokens(text string) int {
	ascii := 0
	nonAscii := 0
	for _, r := range text {
		if r <= 127 {
			ascii++
		} else {
			nonAscii++
		}
	}
	// ASCII: 0.25 token/char, 即每 4 字符 1 token
	// 非 ASCII: 1.5 token/char, 即每字符 1.5 token (nonAscii + nonAscii/2)
	return ascii/4 + nonAscii + nonAscii/2
}

// coerceTokenCount 把 totalTokens（数字或数字字符串）转 int。
func coerceTokenCount(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if x, err := strconv.Atoi(n); err == nil {
			return x
		}
	}
	return 0
}
