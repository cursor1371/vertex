package api

import "time"

// supportedGenerationMethods 返回模型条目声明的支持方法（本代理统一支持这三种）。
func supportedGenerationMethods() []any {
	return []any{"generateContent", "streamGenerateContent", "countTokens"}
}

// geminiModelInfo 构造单个 Gemini 模型规范条目（模型清单与单模型详情共用）。
// SillyTavern 等下游客户端依赖 supportedGenerationMethods 字段过滤可用模型，禁止瘦身。
func geminiModelInfo(name string) map[string]any {
	return map[string]any{
		"name":                       "models/" + name,
		"version":                    name,
		"displayName":                name,
		"description":                "Vertex AI Studio anonymous model",
		"inputTokenLimit":            1048576,
		"outputTokenLimit":           65536,
		"supportedGenerationMethods": supportedGenerationMethods(),
	}
}

// modelsListCreated 是模型清单条目的 created 时间戳（进程启动时刻确定，保持幂等）。
// 模型清单是静态配置，无真实创建时间语义。
var modelsListCreated = time.Now().Unix() //nolint:gochecknoglobals

// openaiModelEntry 构造单个 OpenAI 模型规范条目（混响清单与单模型详情共用）。
// OpenAI 客户端（new-api 等）按 id 字段取模型名。
func openaiModelEntry(name string) map[string]any {
	return map[string]any{
		"id":       name,
		"object":   "model",
		"created":  modelsListCreated,
		"owned_by": "vertex-ai-proxy",
	}
}
