package transform

import (
	"strings"
	"testing"
)

// feedAll 依次喂入各段，返回 (累积正文, 全程合法)。
func feedAll(t *testing.T, d *forcedContentDecoder, chunks ...string) (string, bool) {
	t.Helper()
	ok := true
	for _, c := range chunks {
		if !d.Feed(c) {
			ok = false
		}
	}
	return d.Content(), ok
}

func TestForcedContentDecoder_CompleteObject(t *testing.T) {
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"完整正文"}`)
	if !ok || got != "完整正文" {
		t.Fatalf("完整对象应解出正文: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_IncrementalFragments(t *testing.T) {
	// 分片式：JSON 未闭合时也应产出已解码前缀，且各段拼接无重复无缺失。
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"第一`, `段，第二`, `段"}`)
	if !ok {
		t.Fatalf("分片不应触发非法: ok=%v", ok)
	}
	if got != "第一段，第二段" {
		t.Fatalf("增量累积不符: %q", got)
	}
}

func TestForcedContentDecoder_Escapes(t *testing.T) {
	d := &forcedContentDecoder{}
	// \" \\ \/ \n \t 与字面反斜杠组合
	got, ok := feedAll(t, d, `{"content":"a\"b\\c\/d\ne\tf"}`)
	if !ok {
		t.Fatalf("转义序列不应触发非法: ok=%v", ok)
	}
	want := "a\"b\\c/d\ne\tf"
	if got != want {
		t.Fatalf("转义还原不符: got=%q want=%q", got, want)
	}
}

func TestForcedContentDecoder_UnicodeEscape(t *testing.T) {
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"\u4e2d\u6587"}`)
	if !ok || got != "中文" {
		t.Fatalf("\\uXXXX 还原不符: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_UnicodeEscapeSplitAcrossFrames(t *testing.T) {
	// \uXXXX 的十六进制位被切成三帧：必须凑齐四位才产出，不得泄漏半截。
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"\u`, `4e`, `2d"}`)
	if !ok {
		t.Fatalf("跨帧 \\u 序列不应触发非法: ok=%v", ok)
	}
	if got != "中" {
		t.Fatalf("跨帧 \\u 还原不符: %q", got)
	}
}

func TestForcedContentDecoder_SurrogatePair(t *testing.T) {
	// 😀 = \uD83D\uDE00（UTF-16 代理对）
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"\uD83D\uDE00!"}`)
	if !ok || got != "😀!" {
		t.Fatalf("代理对还原不符: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_SurrogatePairSplitAcrossFrames(t *testing.T) {
	// 代理对在高位与低位之间切帧：高位暂存，下帧配对后产出完整字符。
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"\uD83D`, `\uDE00"}`)
	if !ok || got != "😀" {
		t.Fatalf("跨帧代理对还原不符: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_LoneLowSurrogateInvalid(t *testing.T) {
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"\uDE00"}`)
	if ok {
		t.Fatal("孤立低位代理项应触发非法")
	}
	if got != "" {
		t.Fatalf("非法态不得产出正文: %q", got)
	}
}

func TestForcedContentDecoder_MultibyteUTF8SplitAcrossFrames(t *testing.T) {
	// 原生 UTF-8 多字节序列在字节中间切帧："中" 的三字节被切开。
	full := "中"
	enc := `{"content":"` + full + `"}`
	idx := strings.Index(enc, full)
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, enc[:idx+2], enc[idx+2:]) // 中 的首字节 + 余下字节
	if !ok {
		t.Fatalf("跨帧多字节序列不应触发非法: ok=%v", ok)
	}
	if got != full {
		t.Fatalf("跨帧多字节还原不符: got=%q want=%q", got, full)
	}
}

func TestForcedContentDecoder_ContentNotFirstKey(t *testing.T) {
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"other":123,"flag":true,"content":"正文"}`)
	if !ok || got != "正文" {
		t.Fatalf("content 在后置键时应解出: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_NestedContentKeyIgnored(t *testing.T) {
	// 嵌套对象里的同名 content 键不算正文；根级 content 才是。
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"wrap":{"content":"嵌套"},"content":"根级"}`)
	if !ok || got != "根级" {
		t.Fatalf("嵌套 content 不应被采信: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_DuplicateContentInvalid(t *testing.T) {
	d := &forcedContentDecoder{}
	_, ok := feedAll(t, d, `{"content":"a","content":"b"}`)
	if ok {
		t.Fatal("content 重复出现应触发非法（契约破坏，交由兜底）")
	}
}

func TestForcedContentDecoder_FencedInputInvalid(t *testing.T) {
	// Markdown 围栏包裹：根不是对象 → 非法，回退 extractForcedContent 的正则路径。
	d := &forcedContentDecoder{}
	_, ok := feedAll(t, d, "```json\n{\"content\":\"围栏内\"}\n```")
	if ok {
		t.Fatal("围栏包裹应触发非法")
	}
}

func TestForcedContentDecoder_SnapshotReset(t *testing.T) {
	// 快照式：完整对象替换前帧，解码器 Reset 后重新累积，正文为前缀扩展。
	d := &forcedContentDecoder{}
	if _, ok := feedAll(t, d, `{"content":"第一段"}`); !ok {
		t.Fatal("首帧快照不应非法")
	}
	d.Reset()
	got, ok := feedAll(t, d, `{"content":"第一段，第二段"}`)
	if !ok || got != "第一段，第二段" {
		t.Fatalf("快照替换后正文不符: got=%q ok=%v", got, ok)
	}
}

func TestForcedContentDecoder_TruncatedEscapeHeldBack(t *testing.T) {
	// 尾部是半个转义（\），该字符暂存，不产出残缺字节。
	d := &forcedContentDecoder{}
	got, ok := feedAll(t, d, `{"content":"ab\`)
	if !ok || got != "ab" {
		t.Fatalf("悬挂转义应暂存: got=%q ok=%v", got, ok)
	}
	if !d.Feed(`n"}`) {
		t.Fatal("补齐后不应非法")
	}
	if got := d.Content(); got != "ab\n" {
		t.Fatalf("补齐后正文不符: %q", got)
	}
}

// TestForcedToolUnwrapper_DecoderFallsBackToFencedArgs 解码器非法（围栏包裹）时，
// 回退 extractForcedContent 的正则兜底，正文不丢。
func TestForcedToolUnwrapper_DecoderFallsBackToFencedArgs(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	fenced := "```json\n{\"content\":\"围栏内正文\"}\n```"
	chunk := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, fenced)}},
	}}}
	u.Rewrite(chunk)
	got := partTexts(chunk.Candidates[0].Content.Parts)
	if len(got) != 1 || got[0] != "围栏内正文" {
		t.Fatalf("围栏 args 应走正则兜底: %v", got)
	}
}

// TestForcedToolUnwrapper_MapSnapshotThroughDecoder 完整快照（清洗层转成 map）
// 经 jsonx 序列化后仍走解码器/解析路径，且第二帧只发增量。
func TestForcedToolUnwrapper_MapSnapshotThroughDecoder(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)
	f1 := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, map[string]any{"content": "A"})}},
	}}}
	u.Rewrite(f1)
	if got := partTexts(f1.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "A" {
		t.Fatalf("map 快照首帧应发 A: %v", got)
	}
	f2 := &GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{forcedCall(testForcedName, map[string]any{"content": "AB"})}},
	}}}
	u.Rewrite(f2)
	if got := partTexts(f2.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "B" {
		t.Fatalf("map 快照第二帧应只发增量 B: %v", got)
	}
}
