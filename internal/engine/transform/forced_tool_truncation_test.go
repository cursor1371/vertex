package transform

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// TestUnwrapForcedToolCall_GrowingSnapshots 复现"输出一半即止"：
// 上游对 tool-call args 采用快照式下发（每帧都是"到目前为止的完整对象"，后者覆盖前者）时，
// 同一候选会带上多个 functionCall part。若把它们的 args 原文盲目拼接，
// 得到的是非法 JSON，正则兜底只会命中第一段——正文被截断成前半。
func TestUnwrapForcedToolCall_GrowingSnapshots(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: map[string]any{"content": "第一段"}}},
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: map[string]any{"content": "第一段，第二段"}}},
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: map[string]any{"content": "第一段，第二段，第三段"}}},
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)

	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "第一段，第二段，第三段" {
		t.Fatalf("快照式 args 应取最后一份完整快照, 实际: %+v", parts)
	}
}

// TestUnwrapForcedToolCall_FragmentArgs 分片式下发（每帧是 JSON 片段，需按序拼接）时，
// 拼接后应还原完整正文。
func TestUnwrapForcedToolCall_FragmentArgs(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: `{"content":"分片`}},
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: `前半，分片后半"}`}},
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)

	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "分片前半，分片后半" {
		t.Fatalf("分片式 args 应按序拼接还原, 实际: %+v", parts)
	}
}

// TestForcedToolUnwrapper_GrowingSnapshotAcrossFrames 流式路径下快照式增量：
// 第二次快照相对首次是前缀扩展，客户端应收到其增量（而非被 emitted 闩锁吞掉）。
func TestForcedToolUnwrapper_GrowingSnapshotAcrossFrames(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)

	f1 := &GeminiChunk{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: map[string]any{"content": "第一段"}}},
		}},
	}}}
	u.Rewrite(f1)
	if got := partTexts(f1.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "第一段" {
		t.Fatalf("首帧应发出第一段, 实际: %v", got)
	}

	f2 := &GeminiChunk{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: map[string]any{"content": "第一段，第二段"}}},
		}},
	}}}
	u.Rewrite(f2)
	got := partTexts(f2.Candidates[0].Content.Parts)
	if len(got) != 1 || got[0] != "，第二段" {
		t.Fatalf("第二帧快照应只发出增量「，第二段」, 实际: %v", got)
	}

	f3 := &GeminiChunk{Candidates: []*Candidate{{
		Index:        0,
		Content:      &Content{Role: "model", Parts: []Part{}},
		FinishReason: "STOP",
	}}}
	u.Rewrite(f3)
	if got := partTexts(f3.Candidates[0].Content.Parts); len(got) != 0 {
		t.Fatalf("终结帧不得重复发出, 实际: %v", got)
	}
}

// TestForcedToolUnwrapper_FragmentAcrossFrames 流式分片：增量解码逐帧产出
// 已完整解码的字符（首帧 "上"、次帧补 "半，下半"），拼接完整且无重复。
func TestForcedToolUnwrapper_FragmentAcrossFrames(t *testing.T) {
	u := NewForcedToolUnwrapper(testForcedName, true)

	f1 := &GeminiChunk{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: `{"content":"上`}},
		}},
	}}}
	u.Rewrite(f1)
	if got := partTexts(f1.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "上" {
		t.Fatalf("首帧应增量产出 上, 实际: %v", got)
	}

	f2 := &GeminiChunk{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: `半，下半"}`}},
		}},
	}}}
	u.Rewrite(f2)
	if got := partTexts(f2.Candidates[0].Content.Parts); len(got) != 1 || got[0] != "半，下半" {
		t.Fatalf("次帧应只补增量 半，下半, 实际: %v", got)
	}
}

// TestForcedToolInjection_RequiredModeStillUnwraps 回归：既有单帧完整 args 路径不受影响。
func TestForcedToolInjection_RequiredModeStillUnwraps(t *testing.T) {
	resp := &GeminiResponse{Candidates: []*Candidate{{
		Index: 0,
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: testForcedName, Args: map[string]any{"content": "完整正文"}}},
		}},
	}}}
	UnwrapForcedToolCall(resp, testForcedName)
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].Text != "完整正文" {
		t.Fatalf("单帧完整 args 应原样解包, 实际: %+v", parts)
	}
	_ = config.ForcedModeRequired
}
