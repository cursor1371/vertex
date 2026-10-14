package transform

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestForcedToolInjection_SystemInstructionFallback 记录并锁定控制指令注入的已知副作用：
// 注入的 user 控制回合使 contentsHasUser 恒为真，因此 systemInstruction 不再降级为首条 user 消息。
// 关闭档必须保持既有降级行为不变。
func TestForcedToolInjection_SystemInstructionFallback(t *testing.T) {
	const model = "gemini-3.7-flash"

	sysOnly := func() *GeminiRequest {
		return &GeminiRequest{
			SystemInstruction: &Content{Parts: []Part{{Text: "系统指令"}}},
		}
	}

	t.Run("关闭档保持既有降级行为", func(t *testing.T) {
		out := SharedModelFamilyRouter().For(model).BuildVariables(model, sysOnly(), forcedTestCfg(config.ForcedModeOff))

		if out.SystemInstruction != nil {
			t.Fatalf("关闭档应把 systemInstruction 降级为首条 user 消息: %+v", out.SystemInstruction)
		}
		if len(out.Contents) != 1 || out.Contents[0].Role != "user" {
			t.Fatalf("关闭档应产出单条 user 回合: %+v", out.Contents)
		}
		if out.Contents[0].Parts[0].Text != "系统指令" {
			t.Fatalf("降级内容不符: %+v", out.Contents[0].Parts)
		}
	})

	t.Run("强制档保留顶层 systemInstruction", func(t *testing.T) {
		req := sysOnly()
		req.ForcedToolName = testForcedName
		req.ForcedToolMode = config.ForcedModeRequired
		out := SharedModelFamilyRouter().For(model).BuildVariables(model, req, forcedTestCfg(config.ForcedModeRequired))

		if out.SystemInstruction == nil {
			t.Fatal("强制档注入了 user 控制回合，systemInstruction 应保留为顶层字段而非降级")
		}
		if out.SystemInstruction.Parts[0].Text != "系统指令" {
			t.Fatalf("systemInstruction 内容被改动: %+v", out.SystemInstruction.Parts)
		}
		if len(out.Contents) != 1 || out.Contents[0].Role != "user" {
			t.Fatalf("强制档应产出单条 user 控制回合: %+v", out.Contents)
		}
	})
}
