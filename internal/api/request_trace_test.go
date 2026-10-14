package api

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestResponseModeOf 覆盖回应形态三态：客户端实际接收形态只存在于客户端侧
// （上游侧恒为流式，见 request_trace.go 的类型注释）。
func TestResponseModeOf(t *testing.T) {
	cases := []struct {
		name         string
		reqStreaming bool
		mode         transform.StreamMode
		want         ResponseMode
	}{
		{name: "非流式端点恒为单次", reqStreaming: false, mode: transform.StreamModeTrue, want: ResponseSingle},
		{name: "非流式端点忽略聚合模式", reqStreaming: false, mode: transform.StreamModeAggregate, want: ResponseSingle},
		{name: "流式真流式=增量", reqStreaming: true, mode: transform.StreamModeTrue, want: ResponseIncrement},
		{name: "流式聚合=聚合", reqStreaming: true, mode: transform.StreamModeAggregate, want: ResponseAggregate},
		// StreamModeForbidden（音频）在流式端点无对应分支、落入真流式路径（既有行为）。
		{name: "流式禁用态按增量计", reqStreaming: true, mode: transform.StreamModeForbidden, want: ResponseIncrement},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResponseModeOf(c.reqStreaming, c.mode); got != c.want {
				t.Fatalf("ResponseModeOf(%v, %v) = %q, want %q", c.reqStreaming, c.mode, got, c.want)
			}
		})
	}
}

// TestAntiTruncateEnabled 抗截断链路判定必须与 core_pipeline_text.go 的分支条件同源。
func TestAntiTruncateEnabled(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	anti := transform.ResolveModel("抗截断-gemini-3.7-flash", cfg)
	plain := transform.ResolveModel("gemini-3.7-flash", cfg)
	forced := &transform.GeminiRequest{ForcedToolName: "vertex_emit_x"}

	cases := []struct {
		name     string
		resolved *transform.ResolvedModel
		req      *transform.GeminiRequest
		want     bool
	}{
		{name: "nil resolved", resolved: nil, req: forced, want: false},
		{name: "nil req", resolved: plain, req: nil, want: false},
		{name: "普通请求", resolved: plain, req: &transform.GeminiRequest{}, want: false},
		{name: "抗截断变体", resolved: anti, req: &transform.GeminiRequest{}, want: true},
		{name: "强制工具调用", resolved: plain, req: forced, want: true},
		{name: "抗截断+强制工具调用", resolved: anti, req: forced, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AntiTruncateEnabled(c.resolved, c.req); got != c.want {
				t.Fatalf("AntiTruncateEnabled = %v, want %v", got, c.want)
			}
		})
	}
}

// TestResolveStreamMode 流模式判定：假非流变体与全局聚合开关都覆盖到聚合，
// 图像家族本身即为聚合，文本家族为真流式。
func TestResolveStreamMode(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	text := transform.ResolveModel("gemini-3.7-flash", cfg)
	fake := transform.ResolveModel("假非流-gemini-3.7-flash", cfg)
	image := transform.ResolveModel("gemini-3.1-flash-image", cfg)

	cases := []struct {
		name      string
		resolved  *transform.ResolvedModel
		aggregate bool
		want      transform.StreamMode
	}{
		{name: "nil resolved 保守聚合", resolved: nil, want: transform.StreamModeAggregate},
		{name: "文本真流式", resolved: text, want: transform.StreamModeTrue},
		{name: "文本+全局聚合开关", resolved: text, aggregate: true, want: transform.StreamModeAggregate},
		{name: "假非流变体", resolved: fake, want: transform.StreamModeAggregate},
		{name: "图像家族聚合", resolved: image, want: transform.StreamModeAggregate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveStreamMode(c.resolved, c.aggregate); got != c.want {
				t.Fatalf("ResolveStreamMode = %v, want %v", got, c.want)
			}
		})
	}
}
