package transform

import "strings"

const FinishReasonUnspecified = "FINISH_REASON_UNSPECIFIED"

// CleanFinishReasonUnspecified 就地清洗 typed 候选的 FINISH_REASON_UNSPECIFIED
// （protobuf 默认值，不代表真实结束），返回首个真实 finishReason。
// 返回 "" 表示所有候选均为 UNSPECIFIED（流未结束）。
func CleanFinishReasonUnspecified(resp *GeminiResponse) string {
	if resp == nil {
		return ""
	}
	var realFR string
	for _, cand := range resp.Candidates {
		if cand == nil {
			continue
		}
		if cand.FinishReason == FinishReasonUnspecified {
			cand.FinishReason = ""
		} else if cand.FinishReason != "" && realFR == "" {
			realFR = cand.FinishReason
		}
	}
	return realFR
}

// SafetyFinishReasons 是 Gemini finishReason 维度的"输出被拦截"枚举集合。
// 覆盖：SAFETY（安全审查）、RECITATION（版权/抄袭）、PROHIBITED_CONTENT（违禁内容）、
// SPII（隐私信息）、BLOCKLIST（黑名单）、IMAGE_SAFETY（生图安全）。
// 注意：不含 OTHER（表示"其它原因完成"，非拦截，属正常结束）。
var SafetyFinishReasons = map[string]struct{}{ //nolint:gochecknoglobals
	"SAFETY":             {},
	"RECITATION":         {},
	"PROHIBITED_CONTENT": {},
	"SPII":               {},
	"BLOCKLIST":          {},
	"IMAGE_SAFETY":       {},
}

// IsSafetyFinishReason 判定 finishReason 是否为安全/内容拦截类结束原因。
// 语义：大写化 + 去首尾空白后查 SafetyFinishReasons；空串/未知值返回 false。
func IsSafetyFinishReason(fr string) bool {
	fr = strings.ToUpper(strings.TrimSpace(fr))
	_, ok := SafetyFinishReasons[fr]
	return ok
}

// IsBlockReason 判定 blockReason 是否表示"提示被拦截"。
// 语义：非空且非 BLOCKED_REASON_UNSPECIFIED 即视为拦截（BlockReason 枚举除 UNSPECIFIED
// 外全部表示拦截，含 SAFETY/PROHIBITED_CONTENT/BLOCKLIST/IMAGE_SAFETY/SPII/OTHER；
// 逐一枚举会漏枚举上游新增值，故采用排除法而非白名单）。
func IsBlockReason(br string) bool {
	br = strings.ToUpper(strings.TrimSpace(br))
	return br != "" && br != "BLOCKED_REASON_UNSPECIFIED"
}

// IsSafetyResponse 判定一个 Gemini 响应整体是否为安全/内容拦截响应：
// ① PromptFeedback.BlockReason 为拦截（IsBlockReason）；
// ② 任一候选 FinishReason 为拦截类（IsSafetyFinishReason）。
// 二者满足其一即为 true；resp 为 nil 返回 false。
func IsSafetyResponse(resp *GeminiResponse) bool {
	if resp == nil {
		return false
	}
	if resp.PromptFeedback != nil && IsBlockReason(resp.PromptFeedback.BlockReason) {
		return true
	}
	for _, cand := range resp.Candidates {
		if cand != nil && IsSafetyFinishReason(cand.FinishReason) {
			return true
		}
	}
	return false
}

// 上游主动终止（非安全拦截、亦非正常收尾）的 finishReason 枚举。
// 二者都是上游明确给出的信号，此前均被当作"正常结束"处理：
//   - MAX_TOKENS：撞输出上限被切断；
//   - MALFORMED_FUNCTION_CALL：产出的工具调用畸形（参数不合 schema 等）。
const (
	FinishReasonMaxTokens             = "MAX_TOKENS"
	FinishReasonMalformedFunctionCall = "MALFORMED_FUNCTION_CALL"
)

// TruncationLabels 是 finishReason → 截断标注的映射。
// 仅收录"上游明确表示被截断/异常终止"的枚举；其余（STOP/OTHER/安全类/未知值）不标注。
var TruncationLabels = map[string]string{ //nolint:gochecknoglobals
	FinishReasonMaxTokens:             "输出上限截断",
	FinishReasonMalformedFunctionCall: "工具调用畸形",
}

// TruncationLabel 返回 finishReason 对应的截断标注；空串表示非截断（正常结束或未知）。
//
// 只做标注、不改动交付内容：客户端本就能从 finishReason 看到同样的信息，
// 本函数用于把该事实落到统计与展示层，供运维观察。
// 归一化口径与 IsSafetyFinishReason 一致（大写化 + 去首尾空白）。
func TruncationLabel(fr string) string {
	return TruncationLabels[strings.ToUpper(strings.TrimSpace(fr))]
}
