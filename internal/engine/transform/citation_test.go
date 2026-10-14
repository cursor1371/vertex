package transform

import (
	"strings"
	"testing"
)

func TestConvertCitationsToMarkdown(t *testing.T) {
	gm := map[string]any{
		"groundingChunks": []any{
			map[string]any{
				"web": map[string]any{
					"uri":   "https://example.com/weather",
					"title": "北京天气预报",
				},
			},
			map[string]any{
				"web": map[string]any{
					"uri":   "https://example.com/time",
					"title": "北京时间",
				},
			},
		},
	}

	inputText := "当前北京时间为：上午 11:46 左右[cite: 0f151430-2]。天气状况：多云转阴[cite: 0f151430-1]。"

	got := ConvertCitationsToMarkdown(inputText, gm)

	// 完整相等断言：此前只断言 URL 包含，没抓到 label 被双重包裹的 [[1]](url) 回归。
	want := "当前北京时间为：上午 11:46 左右[2](https://example.com/time \"北京时间\")。" +
		"天气状况：多云转阴[1](https://example.com/weather \"北京天气预报\")。"
	if got != want {
		t.Errorf("ConvertCitationsToMarkdown mismatch.\nGot:  %s\nWant: %s", got, want)
	}
	if strings.Contains(got, "[[") {
		t.Errorf("引用不应输出双层方括号 [[n]]，got: %s", got)
	}
}

// TestConvertCitationsToMarkdown_NoTitle 验证无标题引用输出 [n](url)（不带 "title" 段），
// 以及纯数字 cite id 的 1-based 索引路径。
func TestConvertCitationsToMarkdown_NoTitle(t *testing.T) {
	gm := map[string]any{
		"groundingChunks": []any{
			map[string]any{
				"web": map[string]any{"uri": "https://example.com/a"},
			},
		},
	}

	// 兜底路径（索引越界）沿用原始 id 作 label，仅回退到第 1 个 reference 的 URI。
	got := ConvertCitationsToMarkdown("见[cite: 1]与[cite: 9]。", gm)
	want := "见[1](https://example.com/a)与[9](https://example.com/a)。"
	if got != want {
		t.Errorf("无标题引用 mismatch.\nGot:  %s\nWant: %s", got, want)
	}
	if strings.Contains(got, "[[") {
		t.Errorf("引用不应输出双层方括号 [[n]]，got: %s", got)
	}
}
