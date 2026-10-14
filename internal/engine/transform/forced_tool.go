package transform

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/jsonx"
)

// ForcedToolNamePrefix 是合成工具名的固定前缀。
//
// 必须是稳定前缀：顶点层清洗函数（cleanTypedPart / cleanPart）拿不到请求上下文
// （工具名每请求随机），只能靠前缀识别"这是合成工具"。前缀与随机名的拼接方式不可随意变更。
const ForcedToolNamePrefix = "vertex_emit_"

// forcedContentKey 是合成工具的入参字段名，也是回程解包取正文的字段。
const forcedContentKey = "content"

// forcedToolNameAttempts 是随机工具名的冲突重试上限（96 位随机量，实际不会触及）。
const forcedToolNameAttempts = 100

// forcedToolDescription 是合成工具的描述——模型行为的唯一杠杆。
// 与 forcedControlText 共同约束模型：正文必须完整落在 content，调用之外不得输出内容。
const forcedToolDescription = "把你要对用户说的全部内容原样、完整、逐字地放进 content 字段。" +
	"不要总结、不要省略、不要改写、不要加任何前后缀或额外说明。" +
	"content 字段的内容会直接展示给用户，调用之外不要输出任何内容。"

// forcedControlText 生成追加在对话尾部的控制指令正文。
func forcedControlText(name string) string {
	return "请只通过工具 " + name + " 回复：把你需要对用户说的全部内容完整放进它的 content 参数，" +
		"不要在工具调用之外输出任何文字。"
}

// CollectToolNames 收集请求中已声明的全部函数名，供合成工具名避让。
func CollectToolNames(tools []Tool) map[string]bool {
	names := map[string]bool{}
	for _, t := range tools {
		for _, d := range t.FunctionDeclarations {
			if n := strings.TrimSpace(d.Name); n != "" {
				names[n] = true
			}
		}
	}
	return names
}

// GenerateForcedToolName 生成唯一合成工具名：固定前缀 + 96 位随机量（24 位十六进制）。
// 与已有工具名冲突则重试，上限 forcedToolNameAttempts 次。
func GenerateForcedToolName(existing map[string]bool) (string, error) {
	for range forcedToolNameAttempts {
		b := make([]byte, 12) // 96 位 = 12 字节 = 24 位十六进制
		if _, err := cryptorand.Read(b); err != nil {
			return "", fmt.Errorf("generate forced tool name: %w", err)
		}
		name := ForcedToolNamePrefix + hex.EncodeToString(b)
		if !existing[name] {
			return name, nil
		}
	}
	return "", fmt.Errorf("generate forced tool name: exhausted %d attempts", forcedToolNameAttempts)
}

// IsForcedToolName 判定函数名是否为合成工具（前缀匹配）。
// 顶点层清洗函数靠它识别合成工具；名字每请求随机，那里拿不到请求上下文，故只能用前缀。
func IsForcedToolName(name string) bool {
	return strings.HasPrefix(name, ForcedToolNamePrefix)
}

// BuildForcedTool 构造合成工具声明。
// parameters 保持标准 JSON Schema 形态，由 prepareNativeTools → toNativeSchema
// 统一转换为上游要求的原生 Map-style（type 大写化、properties 转 key/value 数组）。
func BuildForcedTool(name string) Tool {
	return Tool{FunctionDeclarations: []FunctionDeclaration{{
		Name:        name,
		Description: forcedToolDescription,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				forcedContentKey: map[string]any{"type": "string"},
			},
			"required": []string{forcedContentKey},
		},
	}}}
}

// BuildControlMessage 构造追加在对话尾部的控制指令回合（user 角色）。
func BuildControlMessage(name string) Content {
	return Content{Role: "user", Parts: []Part{{Text: forcedControlText(name)}}}
}

// ForcedToolConfig 构造强制调用配置。
//
// 刻意不填 allowedFunctionNames：mode=ANY 的语义是"必须调用某个已声明函数"，
// 客户端无自带工具时合成工具是唯一候选，即已锁定；填该字段依赖上游支持，未验证前不引入。
func ForcedToolConfig() *ToolConfig {
	return &ToolConfig{FunctionCallingConfig: &FunctionCallingConfig{Mode: "ANY"}}
}

// ForcedModeForAntiTruncate 是 抗截断- 前缀变体的强制工具判定。
//
// 前缀是客户端对单次请求的显式意图：即使全局 forced_tool_call_mode 为关闭，
// 也按强制档装配合成工具传输（否则 抗截断- 在流式端点是空转前缀）。
// 但全部结构性闸门保留——不支持自定义工具的模型、含内置工具（googleSearch 等，
// 由上游直接消费、与强制调用语义冲突）、客户端显式 toolConfig.mode=NONE，
// 任一命中仍一律关闭（优雅降级为仅竞速链路改道，不注入）。
func ForcedModeForAntiTruncate(model string, req *GeminiRequest) string {
	if req == nil {
		return config.ForcedModeOff
	}
	if !TextSpecFor(model).SupportsTools {
		return config.ForcedModeOff
	}
	if hasBuiltinTool(req.Tools) {
		return config.ForcedModeOff
	}
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil &&
		strings.EqualFold(strings.TrimSpace(req.ToolConfig.FunctionCallingConfig.Mode), "NONE") {
		return config.ForcedModeOff
	}
	return config.ForcedModeRequired
}

// hasBuiltinTool 判定请求是否含内置工具标记（区别于 functionDeclarations）。
// 内置工具由上游直接消费、不产生 functionCall，与强制调用模式语义冲突，故整请求跳过。
func hasBuiltinTool(tools []Tool) bool {
	for _, t := range tools {
		if t.GoogleSearch != nil || t.GoogleSearchRetrieval != nil || t.GoogleMaps != nil ||
			t.CodeExecution != nil || t.Retrieval != nil || t.URLContext != nil ||
			t.ComputerUse != nil || t.MCPTool != nil || t.FileSearch != nil {
			return true
		}
	}
	return false
}

// ForcedModeFor 判定本次请求的强制工具调用模式，返回 config.ForcedModeOff/Prefer/Required。
// 全部条件满足才启用，任一不满足一律返回 关闭：
//   - 全局模式非"关闭"；
//   - 模型支持自定义 FunctionDeclarations（当前全部规格为 true，属前瞻性防御）；
//   - 模型命中白名单（白名单为空表示不限制）；
//   - 请求不含内置工具（googleSearch/googleMaps/codeExecution 等）；
//   - 客户端未显式关闭函数调用（toolConfig.mode == NONE 时尊重其意图）。
//
// 注意：客户端已声明的 functionDeclarations 不再导致跳过——合成工具与之共存，
// 两者均出现在工具列表中，模型可先调用真实工具（原样透传）再调用合成工具（解包为文本）。
func ForcedModeFor(model string, req *GeminiRequest, cfg config.ConfigProvider) string {
	if cfg == nil || req == nil {
		return config.ForcedModeOff
	}
	mode := cfg.ForcedToolCallMode()
	// 只认两个启用档；关闭/空/非法值一律视为关闭。
	// 不直接信任 provider 返回值：Load() 虽已归一化，但 ConfigProvider 是接口，
	// 其他实现（含测试替身）可能给出未归一化的值，此处必须自证。
	if mode != config.ForcedModePrefer && mode != config.ForcedModeRequired {
		return config.ForcedModeOff
	}
	if !TextSpecFor(model).SupportsTools {
		return config.ForcedModeOff
	}
	if list := cfg.ForcedToolCallModels(); len(list) > 0 {
		hit := false
		for _, m := range list {
			if m == model {
				hit = true
				break
			}
		}
		if !hit {
			return config.ForcedModeOff
		}
	}
	if hasBuiltinTool(req.Tools) {
		return config.ForcedModeOff
	}
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil &&
		strings.EqualFold(strings.TrimSpace(req.ToolConfig.FunctionCallingConfig.Mode), "NONE") {
		return config.ForcedModeOff
	}
	return mode
}

// ---- 回程解包 ----

// forcedContentRegex 是合成工具 args 的兜底扫描正则：抓 "content" 后的首个字符串字面量。
// 覆盖 args 以字符串形式下发（可能被围栏包裹或截断）导致 JSON 解析失败的场景。
//
//nolint:gochecknoglobals // 合成工具 args 的兜底扫描正则
var forcedContentRegex = regexp.MustCompile(`"content"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// forcedArgsText 把 functionCall.args 归一为待解析文本。
// 对象/数组/标量一律 JSON 序列化；字符串直接使用（其内容可能是被围栏包裹或截断的 JSON）。
func forcedArgsText(args any) string {
	switch a := args.(type) {
	case nil:
		return ""
	case string:
		return a
	default:
		b, err := jsonx.Marshal(a)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// extractForcedContent 从合成工具的 args 原文中取回答正文。
// 优先结构化解析（content 为 string 直取，非 string 则 JSON 序列化）；
// 失败则退化为正则扫描 content 的字符串字面量，并以 JSON 反转义还原。
func extractForcedContent(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		if v, ok := obj[forcedContentKey]; ok {
			switch s := v.(type) {
			case string:
				return s, true
			case nil:
				return "", false
			default:
				if b, err := jsonx.Marshal(s); err == nil {
					return string(b), true
				}
			}
		}
	}
	m := forcedContentRegex.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	var s string
	// 用 JSON 反转义而非 strconv.Unquote：JSON 允许 \/ 等 Go 字面量不接受的转义。
	if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &s); err == nil {
		return s, true
	}
	return m[1], true
}

// isPlainTextPart 判定 part 是否为"普通文本"——契约要求不得与合成调用并存的正文载体。
// thought 是思维链、真实 functionCall/functionResponse 是客户端的工具协议、媒体是独立载荷，
// 均不属此列，必须原样保留。
func isPlainTextPart(p Part) bool {
	return p.Text != "" && !p.Thought &&
		p.FunctionCall == nil && p.FunctionResponse == nil &&
		p.InlineData == nil && p.FileData == nil &&
		p.ExecutableCode == nil && p.CodeExecutionResult == nil
}

// forcedSplit 是一次 parts 拆解的结果。
type forcedSplit struct {
	kept  []Part // 保留部分；首个合成调用的位置留空占位（下标 slot）
	slot  int    // 首个合成调用在 kept 中的占位下标；found 为 false 时无意义
	plain string // 收集到的普通文本（按原顺序拼接）
	// argsTexts 是本帧内各合成调用的 args 原文，按出现顺序；
	// 交由 forcedArgsAccumulator 按"快照覆盖 / 分片拼接"规则合并。
	argsTexts []string
	found     bool
}

// forcedArgsAccumulator 累积合成工具调用的 args 原文。
//
// 上游对 tool-call args 存在两种下发形态，必须区别对待，否则正文会被截断：
//   - 快照式：每帧都是"到目前为止的完整对象"，后者覆盖前者。盲目拼接会得到非法 JSON，
//     正则兜底只命中第一段 → 客户端只看到前半。
//   - 分片式：每帧是 JSON 片段，需按序拼接。
//
// 判别依据：新文本自身能否独立解析为完整 JSON 对象——能则视为快照（替换），否则视为分片（追加）。
// 这依赖顶点层 cleanTypedPart 对字符串形式 args 保留原文：解析失败的 args 一律原样
// 透传（现对所有工具统一放行，不再按合成工具前缀区分），否则分片在该层就被清成 {}
// 而永远拼不出完整 JSON。
type forcedArgsAccumulator struct {
	buf string
}

// Add 追加一段 args 原文，返回累积后的完整原文。
func (a *forcedArgsAccumulator) Add(next string) string {
	if next == "" {
		return a.buf
	}
	if isCompleteJSONObject(next) {
		a.buf = next // 快照：后者覆盖前者
		return a.buf
	}
	a.buf += next // 分片：按序拼接
	return a.buf
}

// Text 返回当前累积的 args 原文。
func (a *forcedArgsAccumulator) Text() string { return a.buf }

// isCompleteJSONObject 判定文本能否独立解析为 JSON 对象。
func isCompleteJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '{' {
		return false
	}
	var obj map[string]any
	return json.Unmarshal([]byte(s), &obj) == nil
}

// splitForcedParts 拆解候选的 parts：剥离合成调用（保留其位置作为回填槽）、
// 收集普通文本、原样保留 thought 与真实 functionCall/functionResponse 等。
// 同帧出现多个合成调用时，仅首个保留回填槽，其余被消费但不占位。
func splitForcedParts(parts []Part, name string) forcedSplit {
	sp := forcedSplit{kept: make([]Part, 0, len(parts))}
	var plain strings.Builder
	for i := range parts {
		p := parts[i]
		if p.FunctionCall != nil && p.FunctionCall.Name == name {
			sp.argsTexts = append(sp.argsTexts, forcedArgsText(p.FunctionCall.Args))
			if !sp.found {
				sp.found = true
				sp.slot = len(sp.kept)
				sp.kept = append(sp.kept, Part{}) // 占位：由调用方回填正文或删除
			}
			continue
		}
		if isPlainTextPart(p) {
			plain.WriteString(p.Text)
			continue
		}
		sp.kept = append(sp.kept, p)
	}
	sp.plain = plain.String()
	return sp
}

// isTerminalFinish 判定是否为真实终帧（非空且非 UNSPECIFIED）。
func isTerminalFinish(fr string) bool {
	return fr != "" && fr != FinishReasonUnspecified
}

// UnwrapForcedToolCall 就地消费响应中的合成工具调用，替换为纯文本 part（保持原位置）。
// name 为空表示本请求未启用强制工具调用，直接返回，零改写。
//
// 内容冲突策略：合成调用存在时丢弃同一候选内的普通文本（单一真源），
// thought 与真实 functionCall/functionResponse 原样保留。
// 解包失败时回退交付普通文本——宁可交付契约外文本，也不交付空响应。
func UnwrapForcedToolCall(resp *GeminiResponse, name string) {
	if resp == nil || name == "" {
		return
	}
	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		sp := splitForcedParts(cand.Content.Parts, name)
		if !sp.found {
			continue // 未出现合成调用：原样保留
		}
		// 同一候选可能带上多个合成调用 part（上游按快照或分片增量下发），
		// 必须按累加器规则合并，盲目拼接会把正文截断成前半。
		var acc forcedArgsAccumulator
		for _, t := range sp.argsTexts {
			acc.Add(t)
		}
		if text, ok := extractForcedContent(acc.Text()); ok {
			sp.kept[sp.slot] = Part{Text: text}
			if sp.plain != "" {
				log.Printf("[ForcedTool] 合成调用与普通文本并存，按单一真源丢弃普通文本 %d 字符", len(sp.plain))
			}
			cand.Content.Parts = sp.kept
			continue
		}
		kept := append(sp.kept[:sp.slot], sp.kept[sp.slot+1:]...)
		if sp.plain != "" {
			log.Printf("[ForcedTool] 合成调用 args 无法解析，回退普通文本 %d 字符", len(sp.plain))
			kept = append(kept, Part{Text: sp.plain})
		} else {
			log.Printf("[ForcedTool] 合成调用 args 无法解析且无普通文本可回退")
		}
		cand.Content.Parts = kept
	}
}

// ForcedToolUnwrapper 在流式路径上把合成工具调用帧改写为纯文本帧。
//
// 硬约束：
//   - 就地改写 chunk 而非新建对象：FinishReason / UsageMetadata / SafetyRatings 等元数据必须保留，
//     否则收尾会误判为 ErrStreamIncomplete、token 计数会从上游权威值退化为本地估算；
//   - 永不丢弃帧（空 parts 由上层规范化为 []Part{}）；
//   - emitted 保证正文只发出一次：args 帧已发后，终结帧不得重复发。
//
// 状态按 candidate index 分桶，避免多候选互相污染。
//
// 普通文本策略随模式而异：
//   - 强制档（bufferPlain=true）：正文必经 tool call，普通文本属契约外输出，先缓冲；
//     合成调用出现即丢弃（单一真源），终结帧仍无合成调用则补发；
//   - 优先档（bufferPlain=false）：普通文本立即透传以保留流式体验，
//     合成调用出现时另行发出正文（不回溯删除已发文本，流式下不可行）。
type ForcedToolUnwrapper struct {
	name        string
	bufferPlain bool
	args        map[int]*forcedArgsAccumulator
	// dec 按候选分桶的字符级增量解码器：分片式下发时在 JSON 未闭合前就能产出正文，
	// 与 args 累加器并存——解码器非法（围栏包裹等）时回退累加器 + extractForcedContent。
	dec   map[int]*forcedContentDecoder
	plain map[int]*strings.Builder
	// emittedText 记录每个候选已发出的正文（而非布尔闩锁）：
	// 上游快照式增量下发时，后续帧相对已发内容是前缀扩展，只应补发增量；
	// 用布尔闩锁会在首帧就锁死，导致正文只剩前半。
	emittedText map[int]string
}

// NewForcedToolUnwrapper 构造流式解包器；name 为空（未启用）时返回 nil，各方法对 nil 接收者安全。
func NewForcedToolUnwrapper(name string, bufferPlain bool) *ForcedToolUnwrapper {
	if name == "" {
		return nil
	}
	return &ForcedToolUnwrapper{
		name:        name,
		bufferPlain: bufferPlain,
		args:        map[int]*forcedArgsAccumulator{},
		dec:         map[int]*forcedContentDecoder{},
		plain:       map[int]*strings.Builder{},
		emittedText: map[int]string{},
	}
}

// argsAccFor 取（或创建）指定候选的 args 累加器。
func (u *ForcedToolUnwrapper) argsAccFor(idx int) *forcedArgsAccumulator {
	if a, ok := u.args[idx]; ok {
		return a
	}
	a := &forcedArgsAccumulator{}
	u.args[idx] = a
	return a
}

// tryEmitArgs 在 args 已可解析时把正文的**新增部分**追加进 out。
//
// 只补发增量：上游快照式下发时，第二帧的正文是"第一段+第二段"，
// 已发过第一段则此处只发"第二段"；若整段重发会让客户端看到重复内容。
func (u *ForcedToolUnwrapper) tryEmitArgs(idx int, out []Part) []Part {
	text, ok := u.resolvedForcedText(idx)
	if !ok {
		return out
	}
	prev := u.emittedText[idx]
	if text == prev {
		return out
	}
	delta := text
	if strings.HasPrefix(text, prev) {
		delta = text[len(prev):] // 前缀扩展：只发增量
	} else {
		// 非前缀扩展（上游改写或换了内容）：整段重发会把已发内容重复一遍，
		// 但静默丢弃会让正文缺一截——选择重发完整内容，宁可重复不可缺失。
		log.Printf("[ForcedTool] 合成调用正文非前缀扩展（已发 %d 字符，新 %d 字符），按整段重发", len(prev), len(text))
	}
	u.emittedText[idx] = text
	if pb, ok := u.plain[idx]; ok && pb.Len() > 0 {
		log.Printf("[ForcedTool] 合成调用与普通文本并存，按单一真源丢弃普通文本 %d 字符", pb.Len())
		pb.Reset()
	}
	if delta == "" {
		return out
	}
	return append(out, Part{Text: delta})
}

// resolvedForcedText 取候选当前已可交付的合成正文全量文本。
//
// 优先增量解码器：分片式下发在 JSON 未闭合前就能拿到已解码前缀，这是流式
// 体验的关键。解码器未命中（尚未见到 content 键）或已非法（围栏包裹、畸形
// 转义等）时回退累加器 + extractForcedContent——后者要求 JSON 可整体解析
// 或 content 字面量已闭合，分片中途会拿不到内容。
func (u *ForcedToolUnwrapper) resolvedForcedText(idx int) (string, bool) {
	if d := u.dec[idx]; d != nil && !d.invalid && d.found {
		return d.Content(), true
	}
	acc, ok := u.args[idx]
	if !ok {
		return "", false
	}
	return extractForcedContent(acc.Text())
}

// feedArgsDecoder 把本帧 args 文本喂给指定候选的增量解码器。
// 快照式帧（自身即完整 JSON 对象，内容替换前帧）先复位解码器——复位同时
// 解除 closed/invalid 状态（快照是新的一段合法输入），与 forcedArgsAccumulator
// 的"后者覆盖前者"口径保持一致。
func (u *ForcedToolUnwrapper) feedArgsDecoder(idx int, text string) {
	if text == "" {
		return
	}
	d, ok := u.dec[idx]
	if !ok {
		d = &forcedContentDecoder{}
		u.dec[idx] = d
	}
	if isCompleteJSONObject(text) {
		d.Reset()
	} else if d.invalid || d.closed {
		return // 解码器已不可用：后续帧交给 extractForcedContent 兜底
	}
	d.Feed(text)
}

// Rewrite 就地改写单个 chunk。对 nil 接收者与 nil chunk 均安全。
func (u *ForcedToolUnwrapper) Rewrite(chunk *GeminiChunk) {
	if u == nil || chunk == nil {
		return
	}
	for _, cand := range chunk.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		idx := cand.Index
		sp := splitForcedParts(cand.Content.Parts, u.name)
		out := sp.kept
		if sp.found {
			acc := u.argsAccFor(idx)
			for _, t := range sp.argsTexts {
				acc.Add(t)
				u.feedArgsDecoder(idx, t)
			}
			// 摘掉占位：流式下正文以增量形式追加在末尾，原位无意义。
			out = append(out[:sp.slot], out[sp.slot+1:]...)
		}
		if sp.plain != "" {
			if u.bufferPlain {
				builderFor(u.plain, idx).WriteString(sp.plain)
			} else {
				out = append(out, Part{Text: sp.plain})
			}
		}
		out = u.tryEmitArgs(idx, out)
		// 终结帧兜底：真实 finishReason 到达仍无正文时，补发已缓冲的普通文本。
		if isTerminalFinish(cand.FinishReason) {
			if _, emitted := u.args[idx]; !emitted {
				if b, ok := u.plain[idx]; ok && b.Len() > 0 {
					out = append(out, Part{Text: b.String()})
					u.emittedText[idx] = b.String()
					b.Reset()
				}
			}
		}
		cand.Content.Parts = out
	}
}

// Flush 在流异常结束（始终未收到真实 finishReason）时兜底补发已缓冲的普通文本。
// 返回需要下发的合成 chunk；无内容可补发时返回 nil。
func (u *ForcedToolUnwrapper) Flush() *GeminiChunk {
	if u == nil {
		return nil
	}
	var cands []*Candidate
	for idx, b := range u.plain {
		if b.Len() == 0 || u.emittedText[idx] != "" {
			continue
		}
		text := b.String()
		u.emittedText[idx] = text
		b.Reset()
		cands = append(cands, &Candidate{
			Index:   idx,
			Content: &Content{Role: "model", Parts: []Part{{Text: text}}},
		})
	}
	if len(cands) == 0 {
		return nil
	}
	// map 迭代无序，按候选下标排序保证输出确定。
	sort.Slice(cands, func(i, j int) bool { return cands[i].Index < cands[j].Index })
	return &GeminiChunk{Candidates: cands}
}

// builderFor 取（或创建）指定候选的累积缓冲。
func builderFor(m map[int]*strings.Builder, idx int) *strings.Builder {
	if b, ok := m[idx]; ok {
		return b
	}
	b := &strings.Builder{}
	m[idx] = b
	return b
}
