package transform

import (
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestGenerateForcedToolName 覆盖名字形态与唯一性。
// 冲突重试耗尽路径不可达（96 位随机量），不做测试。
func TestGenerateForcedToolName(t *testing.T) {
	name, err := GenerateForcedToolName(nil)
	if err != nil {
		t.Fatalf("GenerateForcedToolName 返回错误: %v", err)
	}
	if !strings.HasPrefix(name, ForcedToolNamePrefix) {
		t.Fatalf("名字缺少前缀: %q", name)
	}
	if got := len(name) - len(ForcedToolNamePrefix); got != 24 {
		t.Fatalf("随机段长度应为 24 位十六进制，实际 %d (%q)", got, name)
	}
	if name != strings.ToLower(name) {
		t.Fatalf("随机段应为小写十六进制: %q", name)
	}
	if !IsForcedToolName(name) {
		t.Fatalf("IsForcedToolName 未识别合成工具名: %q", name)
	}

	// 连续两次不得相同，且不与已有名冲突。
	existing := map[string]bool{name: true}
	second, err := GenerateForcedToolName(existing)
	if err != nil {
		t.Fatalf("GenerateForcedToolName 第二次返回错误: %v", err)
	}
	if second == name {
		t.Fatalf("两次生成的名字重复: %q", name)
	}
	if existing[second] {
		t.Fatalf("生成的名字与已有名冲突: %q", second)
	}

	if IsForcedToolName("search") {
		t.Fatal("普通工具名被误判为合成工具")
	}
}

func TestCollectToolNames(t *testing.T) {
	tools := []Tool{
		{FunctionDeclarations: []FunctionDeclaration{{Name: "a"}, {Name: " b "}, {Name: ""}}},
		{GoogleSearch: map[string]any{}},
		{FunctionDeclarations: []FunctionDeclaration{{Name: "c"}, {Name: "a"}}},
	}
	got := CollectToolNames(tools)
	for _, want := range []string{"a", "b", "c"} {
		if !got[want] {
			t.Fatalf("未收集到函数名 %q，实际 %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("应收集 3 个函数名，实际 %d (%v)", len(got), got)
	}
}

func TestBuildForcedTool_SchemaShape(t *testing.T) {
	tool := BuildForcedTool("vertex_emit_test")
	if len(tool.FunctionDeclarations) != 1 {
		t.Fatalf("应声明 1 个函数，实际 %d", len(tool.FunctionDeclarations))
	}
	decl := tool.FunctionDeclarations[0]
	if decl.Name != "vertex_emit_test" {
		t.Fatalf("函数名不符: %q", decl.Name)
	}
	if !strings.Contains(decl.Description, "content") {
		t.Fatalf("描述应要求把回答放入 content: %q", decl.Description)
	}
	params, ok := decl.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("parameters 应为 map[string]any，实际 %T", decl.Parameters)
	}
	if params["type"] != "object" {
		t.Fatalf("parameters.type 应为 object: %v", params["type"])
	}
	required, ok := params["required"].([]string)
	if !ok || len(required) != 1 || required[0] != forcedContentKey {
		t.Fatalf("required 应为 [content]: %v", params["required"])
	}
}

// TestBuildForcedTool_ThroughNativeConversion 锁定"合成工具必须先于 prepareNativeTools 入列"这一约束：
// 只有经过 toNativeSchema，type 才会大写化、properties 才会转成 [{key,value}] 数组。
func TestBuildForcedTool_ThroughNativeConversion(t *testing.T) {
	tools := prepareNativeTools([]Tool{BuildForcedTool("vertex_emit_test")})
	if len(tools) != 1 || len(tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("prepareNativeTools 丢弃了合成工具: %+v", tools)
	}
	params, ok := tools[0].FunctionDeclarations[0].Parameters.(map[string]any)
	if !ok {
		t.Fatalf("转换后 parameters 应为 map[string]any，实际 %T", tools[0].FunctionDeclarations[0].Parameters)
	}
	if params["type"] != "OBJECT" {
		t.Fatalf("type 应大写化为 OBJECT: %v", params["type"])
	}
	props, ok := params["properties"].([]any)
	if !ok {
		t.Fatalf("properties 应转为 []any，实际 %T", params["properties"])
	}
	if len(props) != 1 {
		t.Fatalf("properties 应有 1 项，实际 %d", len(props))
	}
	entry, ok := props[0].(map[string]any)
	if !ok || entry["key"] != forcedContentKey {
		t.Fatalf("properties 项应为 {key:content,value:...}: %v", props[0])
	}
}

func TestBuildControlMessage(t *testing.T) {
	msg := BuildControlMessage("vertex_emit_test")
	if msg.Role != "user" {
		t.Fatalf("控制指令应为 user 角色: %q", msg.Role)
	}
	if len(msg.Parts) != 1 || !strings.Contains(msg.Parts[0].Text, "vertex_emit_test") {
		t.Fatalf("控制指令应点名合成工具: %+v", msg.Parts)
	}
}

func TestForcedToolConfig_ModeOnly(t *testing.T) {
	tc := ForcedToolConfig()
	if tc == nil || tc.FunctionCallingConfig == nil {
		t.Fatal("ForcedToolConfig 返回空")
	}
	if tc.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("mode 应为 ANY: %q", tc.FunctionCallingConfig.Mode)
	}
	if tc.FunctionCallingConfig.AllowedFunctionNames != nil {
		t.Fatalf("allowedFunctionNames 应保持 nil（依赖 omitempty 省略）: %v",
			tc.FunctionCallingConfig.AllowedFunctionNames)
	}
}

func TestForcedModeFor(t *testing.T) {
	userText := &GeminiRequest{Contents: []Content{{Role: "user", Parts: []Part{{Text: "hi"}}}}}
	withFuncDecl := &GeminiRequest{Tools: []Tool{{FunctionDeclarations: []FunctionDeclaration{{Name: "real"}}}}}
	withSearch := &GeminiRequest{Tools: []Tool{{GoogleSearch: map[string]any{}}}}
	withMaps := &GeminiRequest{Tools: []Tool{{GoogleMaps: map[string]any{}}}}
	withCodeExec := &GeminiRequest{Tools: []Tool{{CodeExecution: map[string]any{}}}}
	cfgNone := &GeminiRequest{ToolConfig: &ToolConfig{
		FunctionCallingConfig: &FunctionCallingConfig{Mode: "NONE"}}}
	cfgLowerNone := &GeminiRequest{ToolConfig: &ToolConfig{
		FunctionCallingConfig: &FunctionCallingConfig{Mode: "none"}}}
	cfgAuto := &GeminiRequest{ToolConfig: &ToolConfig{
		FunctionCallingConfig: &FunctionCallingConfig{Mode: "AUTO"}}}

	cases := []struct {
		name   string
		model  string
		req    *GeminiRequest
		mode   string
		models []string
		cfgNil bool
		want   string
	}{
		{name: "cfg 为 nil", req: userText, cfgNil: true, want: config.ForcedModeOff},
		{name: "req 为 nil", req: nil, mode: config.ForcedModeRequired, want: config.ForcedModeOff},
		{name: "模式关闭", req: userText, mode: config.ForcedModeOff, want: config.ForcedModeOff},
		{name: "模式为空", req: userText, mode: "", want: config.ForcedModeOff},
		{name: "模式非法", req: userText, mode: "bogus", want: config.ForcedModeOff},
		{name: "优先档", req: userText, mode: config.ForcedModePrefer, want: config.ForcedModePrefer},
		{name: "强制档", req: userText, mode: config.ForcedModeRequired, want: config.ForcedModeRequired},
		{name: "白名单为空=不限制", req: userText, mode: config.ForcedModeRequired, models: []string{}, want: config.ForcedModeRequired},
		{name: "白名单命中", req: userText, mode: config.ForcedModeRequired, models: []string{"gemini-3.7-flash"}, want: config.ForcedModeRequired},
		{name: "白名单未命中", req: userText, mode: config.ForcedModeRequired, models: []string{"other-model"}, want: config.ForcedModeOff},
		{name: "客户端真实工具=共存不跳过", req: withFuncDecl, mode: config.ForcedModeRequired, want: config.ForcedModeRequired},
		{name: "内置 googleSearch=跳过", req: withSearch, mode: config.ForcedModeRequired, want: config.ForcedModeOff},
		{name: "内置 googleMaps=跳过", req: withMaps, mode: config.ForcedModeRequired, want: config.ForcedModeOff},
		{name: "内置 codeExecution=跳过", req: withCodeExec, mode: config.ForcedModeRequired, want: config.ForcedModeOff},
		{name: "客户端显式 NONE=尊重", req: cfgNone, mode: config.ForcedModeRequired, want: config.ForcedModeOff},
		{name: "客户端 NONE 小写=尊重", req: cfgLowerNone, mode: config.ForcedModeRequired, want: config.ForcedModeOff},
		{name: "客户端 AUTO=不阻止", req: cfgAuto, mode: config.ForcedModeRequired, want: config.ForcedModeRequired},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cfg config.ConfigProvider
			if !c.cfgNil {
				cfg = &dummyConfig{forcedMode: c.mode, forcedModels: c.models}
			}
			got := ForcedModeFor("gemini-3.7-flash", c.req, cfg)
			if got != c.want {
				t.Fatalf("ForcedModeFor = %q, 期望 %q", got, c.want)
			}
		})
	}
}
