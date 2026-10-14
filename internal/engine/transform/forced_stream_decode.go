package transform

import (
	"strings"
	"unicode/utf8"
)

// 本文件实现合成工具 args 的字符级增量解码。
//
// 上游对 functionCall.args 存在分片式下发形态（每帧是 JSON 片段，拼接后才完整）。
// 此时 extractForcedContent 在流中途拿不到任何正文：整段 JSON 未闭合时结构化解析
// 失败，正则兜底又要求 content 字符串已闭合。结果客户端看到"卡住后一大段涌出"，
// 而非逐字流式——这正是抗截断传输的体验目标。
//
// forcedContentDecoder 逐字符保留 JSON 语法状态，在 content 字符串尚未闭合时就能
// 产出已完整解码的字符（设计参照 vertex-streaming-anti-truncation 的 ContentDecoder）。
// 关键约束：
//   - 永不累积原始 args 文本：跨帧只保留语法状态与已解码正文，内存随正文而非输入增长；
//   - 不产出残缺字符：未闭合的 \uXXXX、半个 UTF-8 序列、孤立 UTF-16 代理项一律暂存，
//     下帧补齐后才进入正文，客户端不会看到乱码；
//   - 只认根对象 content 键的首个字符串值：嵌套同名键、重复 content 视为契约破坏，
//     转入 invalid 态交由 extractForcedContent 兜底。

// fdMode 标记解码器当前的扫描位置。
type fdMode uint8

const (
	fdModeNormal  fdMode = iota // 结构位置：期待值 / 键 / 分隔符
	fdModeString                // 字符串内部
	fdModeLiteral               // true / false / null 字面量
	fdModeNumber                // 数字
)

// fdStrKind 标记当前字符串的用途。
type fdStrKind uint8

const (
	fdStrNone    fdStrKind = iota
	fdStrKey               // 对象键：只保留短前缀用于匹配 content
	fdStrContent           // 合成正文：逐字符产出
	fdStrValue             // 其余字符串值：跳过
)

// fdState 是一层容器的结构状态。
type fdState uint8

const (
	fdKeyOrEnd        fdState = iota // 对象：期待键或 '}'
	fdColon                          // 对象：键已读，期待 ':'
	fdObjectValue                    // 对象：冒号后期待值（不允许直接 '}'）
	fdArrayValueOrEnd                // 数组：期待值或 ']'
	fdCommaOrEnd                     // 容器：值已读，期待 ',' 或收括
)

// fdFrame 是一层嵌套容器的语法状态。
type fdFrame struct {
	open  byte // '{' 或 '['
	state fdState
	key   string // 最近闭合的对象键（截断保存，用于匹配 content）
}

const (
	// fdMaxDepth 是嵌套容器深度上限：合成工具的 schema 只有单层对象，
	// 超深嵌套只可能是畸形输入，及时止损转兜底。
	fdMaxDepth = 64
	// fdKeyMax 是对象键的保留长度：只需匹配 "content"，超长键截断保存。
	fdKeyMax = 32
)

// forcedContentDecoder 是合成工具 args 的流式 JSON 扫描器。
// 零值即可用；Feed 按帧（上游 chunk 内的 args 文本）调用。
type forcedContentDecoder struct {
	stack []fdFrame

	mode    fdMode
	strKind fdStrKind

	escape bool
	inHex  bool // 正在收集 \uXXXX 的四个十六进制位
	hex    rune // 已累积的数值
	hexLen int  // 已累积的十六进制位数
	high   rune // 待配对的 UTF-16 高位代理项（0 表示无）
	lit    []byte

	key     strings.Builder // 当前对象键（截断至 fdKeyMax）
	pending []byte          // 跨帧暂存的未完整 UTF-8 序列

	// content 是已解码出的正文（跨帧累积），可随时读取。
	content strings.Builder

	found    bool // 已进入根对象 content 字符串
	closed   bool // content 字符串已闭合：后续输入不再产出
	complete bool // 根对象已闭合
	invalid  bool // 输入破坏语法或契约：交由 extractForcedContent 兜底
}

// Feed 喂入一段 args 文本。返回 false 表示本段或此前输入已破坏 JSON 语法，
// 解码器从此进入 invalid 态，调用方应改走 extractForcedContent 兜底路径。
func (d *forcedContentDecoder) Feed(text string) bool {
	if d.invalid || d.closed {
		return !d.invalid
	}
	for i := 0; i < len(text); i++ {
		switch d.mode {
		case fdModeString:
			d.feedString(text[i])
		case fdModeLiteral:
			d.feedLiteral(text[i])
		case fdModeNumber:
			d.feedNumber(text[i])
		default:
			d.feedNormal(text[i])
		}
		if d.invalid {
			return false
		}
	}
	return true
}

// Content 返回已解码出的正文前缀（可能尚未闭合）。
func (d *forcedContentDecoder) Content() string { return d.content.String() }

// Invalid 报告解码器是否已进入非法态。
func (d *forcedContentDecoder) Invalid() bool { return d.invalid }

// Reset 复位全部状态。快照式下发（新帧自身即完整对象、内容替换旧帧）时调用，
// 与 forcedArgsAccumulator 的"后者覆盖前者"口径保持一致。
func (d *forcedContentDecoder) Reset() {
	*d = forcedContentDecoder{}
}

func (d *forcedContentDecoder) fail() { d.invalid = true }

// feedNormal 处理结构位置的字符。
func (d *forcedContentDecoder) feedNormal(ch byte) {
	switch ch {
	case ' ', '\t', '\n', '\r':
		return
	}
	if len(d.stack) == 0 {
		if d.complete {
			// 根对象闭合后仍有输入：帧边界错乱（非快照语境下的多余内容）。
			d.fail()
			return
		}
		if ch != '{' {
			// 根不是对象（围栏包裹 / 数组 / 标量）：交由兜底路径处理。
			d.fail()
			return
		}
		d.stack = append(d.stack, fdFrame{open: '{', state: fdKeyOrEnd})
		return
	}
	fr := &d.stack[len(d.stack)-1]
	switch fr.state {
	case fdKeyOrEnd:
		switch ch {
		case '"':
			d.mode, d.strKind = fdModeString, fdStrKey
			d.key.Reset()
		case '}':
			d.popFrame()
		default:
			d.fail()
		}
	case fdColon:
		if ch != ':' {
			d.fail()
			return
		}
		fr.state = fdObjectValue
	case fdObjectValue:
		d.startValue(ch, fr.key == forcedContentKey && len(d.stack) == 1)
	case fdArrayValueOrEnd:
		if ch == ']' {
			d.popFrame()
			return
		}
		d.startValue(ch, false)
	case fdCommaOrEnd:
		switch ch {
		case ',':
			if fr.open == '{' {
				fr.state = fdKeyOrEnd
			} else {
				fr.state = fdArrayValueOrEnd
			}
		case '}', ']':
			d.popFrame()
		default:
			d.fail()
		}
	}
}

// startValue 在结构位置开始解析一个值。isContent 标记该值位置是根对象的 content 键。
func (d *forcedContentDecoder) startValue(ch byte, isContent bool) {
	if isContent {
		if d.found || ch != '"' {
			// content 重复出现或非字符串：契约破坏，交由兜底路径。
			d.fail()
			return
		}
		d.found = true
		d.mode, d.strKind = fdModeString, fdStrContent
		return
	}
	switch {
	case ch == '"':
		d.mode, d.strKind = fdModeString, fdStrValue
	case ch == '{' || ch == '[':
		if len(d.stack) >= fdMaxDepth {
			d.fail()
			return
		}
		st := fdKeyOrEnd
		if ch == '[' {
			st = fdArrayValueOrEnd
		}
		d.stack = append(d.stack, fdFrame{open: ch, state: st})
	case ch == 't' || ch == 'f' || ch == 'n':
		d.mode = fdModeLiteral
		d.lit = append(d.lit[:0], ch)
	case ch == '-' || (ch >= '0' && ch <= '9'):
		d.mode = fdModeNumber
	default:
		d.fail()
	}
}

// feedString 处理字符串内部字符（含转义序列）。
func (d *forcedContentDecoder) feedString(ch byte) {
	if d.inHex {
		v := fdHexVal(ch)
		if v < 0 {
			d.fail()
			return
		}
		d.hex = d.hex<<4 | rune(v)
		d.hexLen++
		if d.hexLen == 4 {
			d.inHex = false
			d.emitCodeUnit(d.hex)
		}
		return
	}
	if d.escape {
		d.escape = false
		switch ch {
		case 'u':
			d.inHex, d.hex, d.hexLen = true, 0, 0
		case '"':
			d.emitByte('"')
		case '\\':
			d.emitByte('\\')
		case '/':
			d.emitByte('/')
		case 'b':
			d.emitByte('\b')
		case 'f':
			d.emitByte('\f')
		case 'n':
			d.emitByte('\n')
		case 'r':
			d.emitByte('\r')
		case 't':
			d.emitByte('\t')
		default:
			d.fail()
		}
		return
	}
	switch ch {
	case '\\':
		d.escape = true
	case '"':
		switch d.strKind {
		case fdStrKey:
			fr := &d.stack[len(d.stack)-1]
			fr.key = d.key.String()
			fr.state = fdColon
		case fdStrContent:
			// 闭合时仍有未完成的 UTF-8 序列，说明上游在字节中间切帧且收尾非法。
			if len(d.pending) > 0 {
				d.fail()
				return
			}
			d.closed = true
			d.afterValue()
		case fdStrValue:
			d.afterValue()
		}
		d.mode, d.strKind = fdModeNormal, fdStrNone
	default:
		switch d.strKind {
		case fdStrKey:
			if d.key.Len() < fdKeyMax {
				d.key.WriteByte(ch)
			}
		case fdStrContent:
			d.emitByte(ch)
		}
	}
}

// feedLiteral 处理 true / false / null，按前缀增量校验。
func (d *forcedContentDecoder) feedLiteral(ch byte) {
	d.lit = append(d.lit, ch)
	word := string(d.lit)
	if !strings.HasPrefix("true", word) && !strings.HasPrefix("false", word) && !strings.HasPrefix("null", word) {
		d.fail()
		return
	}
	if word == "true" || word == "false" || word == "null" {
		d.mode = fdModeNormal
		d.afterValue()
	}
}

// feedNumber 累积数字字符；数字文法不在此校验，非法序列最终由整体解析兜底。
func (d *forcedContentDecoder) feedNumber(ch byte) {
	switch {
	case ch >= '0' && ch <= '9', ch == '.', ch == 'e', ch == 'E', ch == '+', ch == '-':
		return
	default:
		d.mode = fdModeNormal
		d.afterValue()
		d.feedNormal(ch) // 数字结束，当前字符交回结构层
	}
}

// afterValue 在值（字面量/数字）结束后推进容器状态。
func (d *forcedContentDecoder) afterValue() {
	if len(d.stack) == 0 {
		d.complete = true
		return
	}
	d.stack[len(d.stack)-1].state = fdCommaOrEnd
}

// popFrame 收拢一层容器，并让外层进入"值已读"状态。
func (d *forcedContentDecoder) popFrame() {
	d.stack = d.stack[:len(d.stack)-1]
	if len(d.stack) == 0 {
		d.complete = true
		return
	}
	d.stack[len(d.stack)-1].state = fdCommaOrEnd
}

// emitCodeUnit 处理 \uXXXX 解出的单个 UTF-16 码元：代理项需配对后才产出。
func (d *forcedContentDecoder) emitCodeUnit(r rune) {
	if d.strKind != fdStrContent {
		return
	}
	switch {
	case r >= 0xD800 && r <= 0xDBFF: // 高位代理项：等低位配对
		if d.high != 0 {
			d.fail() // 连续两个高位代理项：非法序列
			return
		}
		d.high = r
	case r >= 0xDC00 && r <= 0xDFFF: // 低位代理项
		if d.high == 0 {
			d.fail() // 孤立低位代理项
			return
		}
		d.content.WriteRune(((d.high-0xD800)<<10 | (r - 0xDC00)) + 0x10000)
		d.high = 0
	default:
		if d.high != 0 {
			d.fail() // 高位代理项未获配对
			return
		}
		d.content.WriteRune(r)
	}
}

// emitByte 产出 content 字符串内的一个原始字节。
// 多字节 UTF-8 序列可能被上游在字节中间切帧，凑齐完整序列才写入正文。
func (d *forcedContentDecoder) emitByte(b byte) {
	if d.strKind != fdStrContent {
		return
	}
	d.pending = append(d.pending, b)
	if !utf8.FullRune(d.pending) {
		return
	}
	r, size := utf8.DecodeRune(d.pending)
	if r == utf8.RuneError && size <= 1 {
		d.fail() // 非法 UTF-8 编码
		return
	}
	d.content.WriteRune(r)
	d.pending = d.pending[:0]
}

// fdHexVal 解析单个十六进制字符，非法返回 -1。
func fdHexVal(ch byte) int {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0')
	case ch >= 'a' && ch <= 'f':
		return int(ch-'a') + 10
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10
	default:
		return -1
	}
}
