package transform

import "testing"

func TestStreamIntegrityGuard_Complete(t *testing.T) {
	var g StreamIntegrityGuard
	g.Observe(&GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{{Text: "正文"}}},
	}}})
	g.Observe(&GeminiChunk{Candidates: []*Candidate{{
		Index: 0, FinishReason: "STOP",
		Content: &Content{Role: "model", Parts: []Part{}},
	}}})
	got := g.Finalize()
	if got.Outcome != IntegrityComplete {
		t.Errorf("Outcome = %q, want complete", got.Outcome)
	}
	if !got.StreamDone || !got.HasContent {
		t.Errorf("StreamDone/HasContent 不符: %+v", got)
	}
}

func TestStreamIntegrityGuard_ThoughtOnlyIsEmpty(t *testing.T) {
	// 只有思维链却声称正常完成：空回复（思维链不是可见正文）。
	var g StreamIntegrityGuard
	g.Observe(&GeminiChunk{Candidates: []*Candidate{{
		Index: 0, FinishReason: "STOP",
		Content: &Content{Role: "model", Parts: []Part{{Text: "思考中", Thought: true}}},
	}}})
	if got := g.Finalize(); got.Outcome != IntegrityEmpty {
		t.Errorf("只思考声称完成应为 empty，实得 %q", got.Outcome)
	}
}

func TestStreamIntegrityGuard_NoFinishIsIncomplete(t *testing.T) {
	var g StreamIntegrityGuard
	g.Observe(&GeminiChunk{Candidates: []*Candidate{{
		Index:   0,
		Content: &Content{Role: "model", Parts: []Part{{Text: "半截"}}},
	}}})
	got := g.Finalize()
	if got.Outcome != IntegrityIncomplete {
		t.Errorf("无终帧应为 incomplete，实得 %q", got.Outcome)
	}
}

func TestStreamIntegrityGuard_BadToolArgsIncomplete(t *testing.T) {
	var g StreamIntegrityGuard
	g.Observe(&GeminiChunk{Candidates: []*Candidate{{
		Index: 0, FinishReason: "STOP",
		Content: &Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{Name: "f", Args: `{"a":`}},
		}},
	}}})
	if got := g.Finalize(); got.Outcome != IntegrityIncomplete {
		t.Errorf("工具参数非法应为 incomplete，实得 %q", got.Outcome)
	}
}

func TestStreamIntegrityGuard_LengthAndFilter(t *testing.T) {
	var g StreamIntegrityGuard
	g.MarkSafety("SAFETY")
	if got := g.Finalize(); got.Outcome != IntegrityFilter {
		t.Errorf("MarkSafety 应为 content_filter，实得 %q", got.Outcome)
	}

	var g2 StreamIntegrityGuard
	g2.Observe(&GeminiChunk{Candidates: []*Candidate{{
		Index: 0, FinishReason: "MAX_TOKENS",
		Content: &Content{Role: "model", Parts: []Part{{Text: "撞上限"}}},
	}}})
	if got := g2.Finalize(); got.Outcome != IntegrityLength {
		t.Errorf("MAX_TOKENS 应为 length，实得 %q", got.Outcome)
	}
}

func TestDroppedParamsFor(t *testing.T) {
	tests := []struct {
		model string
		gc    *GenerationConfig
		want  []string
	}{
		{"gemini-3.7-flash", &GenerationConfig{Temperature: f64ptr(1), FrequencyPenalty: f64ptr(0.5), TopK: f64ptr(32)},
			[]string{"temperature", "top_k", "frequency_penalty"}},
		{"gemini-3.5-flash", &GenerationConfig{TopK: f64ptr(32), PresencePenalty: f64ptr(0.5)},
			[]string{"top_k", "presence_penalty"}},
		{"gemini-3.1-pro-preview", &GenerationConfig{TopK: f64ptr(32), FrequencyPenalty: f64ptr(0.5)},
			[]string{"top_k"}},
		{"gemini-2.5-flash", &GenerationConfig{TopK: f64ptr(32), FrequencyPenalty: f64ptr(0.5)},
			nil}, // 2.5-flash 文档未声明不支持 → 不剥离
		{"unknown-model-9.9", &GenerationConfig{Temperature: f64ptr(1), TopK: f64ptr(1)},
			[]string{"temperature"}}, // 未知模型沿用既有保守温度策略；topK/penalty 新字段从宽不剥离
	}
	for _, tt := range tests {
		req := &GeminiRequest{GenerationConfig: tt.gc}
		got := DroppedParamsFor(tt.model, req)
		if len(got) != len(tt.want) {
			t.Errorf("%s: DroppedParamsFor = %v, want %v", tt.model, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: DroppedParamsFor[%d] = %q, want %q", tt.model, i, got[i], tt.want[i])
			}
		}
	}
}

func TestOpenAIToolCallRegistry_StableAcrossFrames(t *testing.T) {
	r := NewOpenAIToolCallRegistry()
	id1, idx1 := r.Resolve("get_weather")
	id2, idx2 := r.Resolve("get_weather")
	if id1 != id2 || idx1 != idx2 {
		t.Fatalf("同一函数名跨帧必须稳定: (%s,%d) vs (%s,%d)", id1, idx1, id2, idx2)
	}
	id3, idx3 := r.Resolve("other_tool")
	if id3 == id1 || idx3 == idx1 {
		t.Fatalf("不同函数名应各自分配: (%s,%d) vs (%s,%d)", id3, idx3, id1, idx1)
	}
}

func TestGeminiToolCalls_CarriesIndex(t *testing.T) {
	r := NewOpenAIToolCallRegistry()
	c := &Candidate{Content: &Content{Parts: []Part{
		{FunctionCall: &FunctionCall{Name: "get_weather", Args: map[string]any{"city": "上海"}}},
	}}}
	calls := geminiToolCalls(c, r.Resolve)
	if len(calls) != 1 {
		t.Fatalf("应产出 1 个 tool_call: %+v", calls)
	}
	if calls[0].Index == nil || *calls[0].Index != 0 {
		t.Errorf("tool_call 应携带 index=0: %+v", calls[0].Index)
	}
	// 下一帧同一调用：ID 与 index 复用，客户端按序拼接不出重复调用。
	calls2 := geminiToolCalls(&Candidate{Content: &Content{Parts: []Part{
		{FunctionCall: &FunctionCall{Name: "get_weather", Args: map[string]any{"city": "北京"}}},
	}}}, r.Resolve)
	if calls2[0].ID != calls[0].ID || *calls2[0].Index != *calls[0].Index {
		t.Fatalf("跨帧 ID/index 应稳定: %+v vs %+v", calls[0], calls2[0])
	}
}
