package transform

import "testing"

func TestCleanFinishReasonUnspecified(t *testing.T) {
	cases := []struct {
		name string
		resp *GeminiResponse
		want string
	}{
		{
			name: "nil response",
			resp: nil,
			want: "",
		},
		{
			name: "all unspecified cleared",
			resp: &GeminiResponse{Candidates: []*Candidate{
				{FinishReason: FinishReasonUnspecified},
				{FinishReason: FinishReasonUnspecified},
			}},
			want: "",
		},
		{
			name: "first real finish reason returned",
			resp: &GeminiResponse{Candidates: []*Candidate{
				{FinishReason: FinishReasonUnspecified},
				{FinishReason: "STOP"},
				{FinishReason: "MAX_TOKENS"},
			}},
			want: "STOP",
		},
		{
			name: "unspecified mutated to empty",
			resp: &GeminiResponse{Candidates: []*Candidate{
				{FinishReason: FinishReasonUnspecified},
				{FinishReason: "STOP"},
			}},
			want: "STOP",
		},
		{
			name: "empty candidate list",
			resp: &GeminiResponse{Candidates: []*Candidate{}},
			want: "",
		},
		{
			name: "nil candidate skipped",
			resp: &GeminiResponse{Candidates: []*Candidate{nil, {FinishReason: "SAFETY"}}},
			want: "SAFETY",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CleanFinishReasonUnspecified(c.resp)
			if got != c.want {
				t.Errorf("CleanFinishReasonUnspecified=%q, want %q", got, c.want)
			}
		})
	}

	// 就地变异断言：UNSPECIFIED 必须被清空为 ""。
	resp := &GeminiResponse{Candidates: []*Candidate{
		{FinishReason: FinishReasonUnspecified},
		{FinishReason: "STOP"},
	}}
	CleanFinishReasonUnspecified(resp)
	if resp.Candidates[0].FinishReason != "" {
		t.Errorf("UNSPECIFIED 候选应被清空为 \"\", got %q", resp.Candidates[0].FinishReason)
	}
	if resp.Candidates[1].FinishReason != "STOP" {
		t.Errorf("真实 finishReason 不应被改动, got %q", resp.Candidates[1].FinishReason)
	}
}

func TestIsSafetyFinishReason(t *testing.T) {
	for _, fr := range []string{"SAFETY", "RECITATION", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "IMAGE_SAFETY"} {
		if !IsSafetyFinishReason(fr) {
			t.Errorf("IsSafetyFinishReason(%q) 应为 true", fr)
		}
	}
	for _, fr := range []string{" recitation ", "prohibited_content"} {
		if !IsSafetyFinishReason(fr) {
			t.Errorf("IsSafetyFinishReason(%q) 大小写/空白归一后应为 true", fr)
		}
	}
	for _, fr := range []string{"", "STOP", "OTHER", "MAX_TOKENS", "TOOL_CALLS", "UNKNOWN_XYZ"} {
		if IsSafetyFinishReason(fr) {
			t.Errorf("IsSafetyFinishReason(%q) 应为 false", fr)
		}
	}
}

func TestIsBlockReason(t *testing.T) {
	for _, br := range []string{"SAFETY", "BLOCKED_REASON_PROHIBITED_CONTENT", "BLOCKED_REASON_IMAGE_SAFETY", "BLOCKED_REASON_OTHER", "IMAGE_SAFETY", "OTHER", "BLOCKLIST", "SPII"} {
		if !IsBlockReason(br) {
			t.Errorf("IsBlockReason(%q) 应为 true", br)
		}
	}
	for _, br := range []string{"", "BLOCKED_REASON_UNSPECIFIED", "blocked_reason_unspecified", " BLOCKED_REASON_UNSPECIFIED "} {
		if IsBlockReason(br) {
			t.Errorf("IsBlockReason(%q) 应为 false", br)
		}
	}
}

func TestIsSafetyResponse(t *testing.T) {
	cases := []struct {
		name string
		resp *GeminiResponse
		want bool
	}{
		{
			name: "promptFeedback blockReason 拦截",
			resp: &GeminiResponse{PromptFeedback: &PromptFeedback{BlockReason: "BLOCKED_REASON_IMAGE_SAFETY"}},
			want: true,
		},
		{
			name: "候选 finishReason RECITATION",
			resp: &GeminiResponse{Candidates: []*Candidate{{FinishReason: "RECITATION"}}},
			want: true,
		},
		{
			name: "无 pf 无 candidates",
			resp: &GeminiResponse{},
			want: false,
		},
		{
			name: "nil 响应",
			resp: nil,
			want: false,
		},
		{
			name: "STOP + 有内容",
			resp: &GeminiResponse{Candidates: []*Candidate{{FinishReason: "STOP", Content: &Content{Parts: []Part{{Text: "hi"}}}}}},
			want: false,
		},
		{
			name: "pf blockReason UNSPECIFIED 不算拦截",
			resp: &GeminiResponse{PromptFeedback: &PromptFeedback{BlockReason: "BLOCKED_REASON_UNSPECIFIED"}},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSafetyResponse(c.resp); got != c.want {
				t.Errorf("IsSafetyResponse=%v, want %v", got, c.want)
			}
		})
	}
}

// TestTruncationLabel 上游主动异常终止的标注映射。
// 只做标注、不改判成败：客户端本就能从 finishReason 看到同样信息，此处供统计与展示层使用。
func TestTruncationLabel(t *testing.T) {
	cases := []struct {
		name string
		fr   string
		want string
	}{
		{name: "输出上限截断", fr: "MAX_TOKENS", want: "输出上限截断"},
		{name: "工具调用畸形", fr: "MALFORMED_FUNCTION_CALL", want: "工具调用畸形"},
		// 归一化口径与 IsSafetyFinishReason 一致。
		{name: "小写归一", fr: "max_tokens", want: "输出上限截断"},
		{name: "首尾空白归一", fr: "  MALFORMED_FUNCTION_CALL  ", want: "工具调用畸形"},
		// 非截断：正常收尾、其它原因完成、安全拦截、未指定的空值、未知枚举均不标注。
		{name: "正常收尾不标注", fr: "STOP", want: ""},
		{name: "OTHER 不标注", fr: "OTHER", want: ""},
		{name: "安全拦截不标注", fr: "SAFETY", want: ""},
		{name: "UNSPECIFIED 不标注", fr: FinishReasonUnspecified, want: ""},
		{name: "空串不标注", fr: "", want: ""},
		{name: "未知枚举不标注", fr: "SOMETHING_NEW", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TruncationLabel(c.fr); got != c.want {
				t.Fatalf("TruncationLabel(%q) = %q, want %q", c.fr, got, c.want)
			}
		})
	}
}
