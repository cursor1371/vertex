package transform

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// 本文件守护 BuildVariables 的"纯函数"契约：
// 竞速引擎（race_engine.go）对同一 *GeminiRequest 并行调用每个候选的
// buildTypedRequestPayload → BuildGeminiVariablesTyped，BuildVariables 若有任何
// 共享内存写（Role 兜底、Parts merge append、FunctionCall.ID 清理、ToolConfig.Mode /
// GenerationConfig / ThinkingConfig 归一化）都会构成 data race 且互相污染 payload。
// 必须以 `go test -race` 运行才有守护效力。

// buildSharedRaceRequest 构造一个覆盖全部历史共享写触发点的请求：
//   - Role 为空的 Content → sanitizeContentRolesTyped 的 Role 兜底写
//   - 相邻同 role 文本回合 + 相邻 FunctionResponse 回合 → 两个 merge 函数的 Parts append
//   - FunctionCall part → ApplyPart 清理临时 ID
//   - 小写 toolConfig.mode → prepareNativeToolConfig 归一化
//   - 小写 mediaResolution / responseModalities / 越界采样参数 → prepareNativeGenerationConfig
//     与 applyTextSamplingParams 的注入/clamp 写
//   - ThinkingConfig 小写档位 → NormalizeThinkingConfig（image 家族路径）
func buildSharedRaceRequest() *GeminiRequest {
	mx := 8192
	temp := 3.5
	topP := 1.5
	return &GeminiRequest{
		Contents: []Content{
			// Role 为空：触发 Role 兜底
			{Role: "", Parts: []Part{{Text: "hi"}}},
			// 相邻同 role 文本：触发 mergeContiguousTextRoles 的 Parts 合并
			{Role: "user", Parts: []Part{{Text: "part-a"}}},
			{Role: "user", Parts: []Part{{Text: "part-b"}}},
			// FunctionResponse 相邻回合：触发 packParallelToolResponses 的 Parts 合并
			{Role: "", Parts: []Part{{FunctionResponse: &FunctionResponse{Name: "t", Response: map[string]any{"r": "1"}}}}},
			{Role: "", Parts: []Part{{FunctionResponse: &FunctionResponse{Name: "t", Response: map[string]any{"r": "2"}}}}},
			// model 回合带 FunctionCall（含临时 ID 与明文签名）：触发 ApplyPart
			{Role: "model", Parts: []Part{
				{FunctionCall: &FunctionCall{Name: "get_weather", Args: map[string]any{"city": "x"}, ID: "temp-id-1"}},
				{Thought: true, ThoughtSignature: "plain_sig"},
			}},
		},
		Tools: []Tool{{FunctionDeclarations: []FunctionDeclaration{{
			Name:       "client_tool",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		}}}},
		ToolConfig: &ToolConfig{FunctionCallingConfig: &FunctionCallingConfig{Mode: "validated"}},
		GenerationConfig: &GenerationConfig{
			MaxOutputTokens:    &mx,
			Temperature:        &temp,
			TopP:               &topP,
			MediaResolution:    "low",
			ResponseModalities: []string{"text"},
			ThinkingConfig:     &ThinkingConfig{ThinkingLevel: "high"},
		},
	}
}

// TestBuildVariables_ConcurrentSharedRequest 守护并发安全：
// 多 goroutine 并行对同一请求执行三家族 BuildVariables，`-race` 下无告警即通过；
// 结束后校验请求对象逐字节未被修改（纯函数性）。
func TestBuildVariables_ConcurrentSharedRequest(t *testing.T) {
	req := buildSharedRaceRequest()
	before, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal before: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = BuildGeminiVariablesTyped("gemini-3.7-flash", req, &dummyConfig{})
				_ = BuildGeminiVariablesTyped("gemini-3.1-flash-image", req, &dummyConfig{})
				_ = BuildGeminiVariablesTyped("gemini-2.5-flash-tts", req, &dummyConfig{})
			}
		}()
	}
	wg.Wait()

	after, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("BuildVariables 修改了共享请求对象（竞速候选会互相污染 payload）:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestBuildVariables_ConcurrentForcedToolName 补齐合成工具注入路径的并发守护：
// ForcedToolName 由 handler 层一次性固化，竞速候选并行调用 BuildVariables 时
// 各自复制追加（req.Tools 只读）。`-race` 下无告警 + 请求对象逐字节不变。
func TestBuildVariables_ConcurrentForcedToolName(t *testing.T) {
	req := buildSharedRaceRequest()
	req.ForcedToolName = testForcedName
	req.ForcedToolMode = config.ForcedModeRequired
	before, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal before: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = BuildGeminiVariablesTyped("gemini-3.7-flash", req, &dummyConfig{})
			}
		}()
	}
	wg.Wait()

	after, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("注入路径下 BuildVariables 修改了共享请求对象:\nbefore=%s\nafter=%s", before, after)
	}

	// 单次调用产物必须同时包含客户端工具与合成工具（共存，非替换）。
	vars := BuildGeminiVariablesTyped("gemini-3.7-flash", req, &dummyConfig{})
	foundClient, foundForced := false, false
	for _, tl := range vars.Tools {
		for _, d := range tl.FunctionDeclarations {
			switch d.Name {
			case "client_tool":
				foundClient = true
			case testForcedName:
				foundForced = true
			}
		}
	}
	if !foundClient || !foundForced {
		t.Fatalf("注入产物应同时含客户端工具与合成工具: %+v", vars.Tools)
	}
}

// TestNormalizeThinkingConfig_PureFunction 直接守护 NormalizeThinkingConfig 不改输入：
// 其输入经 ImageStrategy.BuildVariables 的 orig.ThinkingConfig 共享指针在候选内并行可达。
func TestNormalizeThinkingConfig_PureFunction(t *testing.T) {
	for _, model := range []string{"gemini-3.7-flash", "gemini-2.5-flash", "gemini-3.1-flash-image"} {
		in := &ThinkingConfig{ThinkingLevel: "high", ThinkingBudget: nil}
		_ = NormalizeThinkingConfig(in, model)
		if in.ThinkingLevel != "high" || in.ThinkingBudget != nil {
			t.Fatalf("model=%s: NormalizeThinkingConfig 修改了输入: %+v", model, in)
		}
		in2 := &ThinkingConfig{ThinkingBudget: intPtr(1024)}
		_ = NormalizeThinkingConfig(in2, model)
		if in2.ThinkingBudget == nil || *in2.ThinkingBudget != 1024 || in2.ThinkingLevel != "" {
			t.Fatalf("model=%s: NormalizeThinkingConfig 修改了输入: %+v", model, in2)
		}
	}
}
