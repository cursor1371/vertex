package api

import (
	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
)

// videoUnsupportedError 是视频家族（vedio）的分发层硬闸。
//
// 背景：Veo 系上游走 long-running 协议，后端视频生成能力尚未实现（Issue #4 P1 结论）。
// 视频模型的正确行为是"可发现、不可调用"——上游目录与管理面板可见，但任何生成请求
// 必须在触达上游之前被明确拒绝，绝不能因为"不认识"而落入文本/图像/语音 pipeline。
//
// 拒绝对 resolved 家族为 FamilyVideo 的请求返回 400；其余家族返回 nil（不干预）。
// 调用点：两个 Gemini 生成 handler 在解析模型后立即调用（第一道闸，覆盖非流式、
// 聚合流式与真流式全部路径），两处 family switch 内的 FamilyVideo case 是第二道闸
// （防御未来重构把前置检查挪走）。
func videoUnsupportedError(resolved *transform.ResolvedModel) *vertex.VertexError {
	if resolved == nil || resolved.Family != transform.FamilyVideo {
		return nil
	}
	return vertex.NewInvalidArgumentError(
		"模型 "+resolved.ActualModel+" 属视频家族(vedio)，后端视频生成能力尚未实现，暂不支持调用 "+
			"(video generation is not implemented)", nil)
}
