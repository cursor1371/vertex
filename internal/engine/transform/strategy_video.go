package transform

import (
	"fmt"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
)

// VideoStrategy 是视频生成家族（vedio）的策略占位实现。
//
// 后端视频生成能力尚未实现：上游 Veo 系走 long-running 协议（predictLongRunning /
// operations 轮询 / 视频文件获取），与现有「单次 batchGraphql 调用 + 即时响应」架构正交，
// 端到端适配拆为后续独立 Issue（见 Issue #4 P1 结论）。
//
// 本策略存在的意义是**结构性隔离**：让视频模型有明确的家族归属与策略对象，
// 使 API 分发层可以显式拒绝，而不是因为"不认识"而落入文本家族；
// 策略自身在 Validate 阶段即报错，任何误入 pipeline 的调用都不会到达上游。
type VideoStrategy struct {
	model string
}

// Family 返回视频家族。
func (s *VideoStrategy) Family() ModelFamily { return FamilyVideo }

// FamilyStreamMode 视频家族无流式语义；即使被调用也不走既有的流式分支。
func (s *VideoStrategy) FamilyStreamMode() StreamMode { return StreamModeForbidden }

// Enhancement 类方法对视频家族不适用，全部保持零改写。

// Enhance 不注入任何模态/参数。
func (s *VideoStrategy) Enhance(_ *GeminiRequest, _ config.ConfigProvider) {}

// Validate 直接拒绝：视频生成 API 尚未实现。
//
// 这是 pipeline 内的第二道保险——第一道是 API 分发层的显式家族拒绝
// （见 internal/api 的 videoUnsupportedError），正常路径不会走到这里。
func (s *VideoStrategy) Validate(_ *GeminiRequest) error {
	return fmt.Errorf("视频生成 API 尚未实现（family=vedio），暂不支持调用 "+
		"%s (video generation not implemented)", s.model)
}

// Prepare 不执行任何出口清洗。
func (s *VideoStrategy) Prepare(_ *GeminiRequest) {}

// BuildVariables 防御性实现：正常路径不可达（Validate 已拒绝）。
// 返回仅含模型名的空载荷，避免调用方拿到 nil 后解引用。
func (s *VideoStrategy) BuildVariables(model string, _ *GeminiRequest, _ config.ConfigProvider) *GeminiVariables {
	return &GeminiVariables{Model: config.ResolveModelName(model), GeminiRequest: &GeminiRequest{}}
}

// CalculateIdleTimeouts 返回保守的文本家族同款超时（不可达路径，仅防御）。
func (s *VideoStrategy) CalculateIdleTimeouts(baseSeconds int) (preTimeout, postTimeout time.Duration) {
	postTimeout = max(time.Duration(baseSeconds)*time.Second, 10*time.Second)
	preTimeout = max(time.Duration(baseSeconds*2)*time.Second, 20*time.Second)
	return preTimeout, postTimeout
}

// IsValidChunk 视频家族不参与竞速胜出判定。
func (s *VideoStrategy) IsValidChunk(_ *GeminiChunk) bool { return false }

// IsValidResponse 视频家族不参与非流式对冲胜出判定。
func (s *VideoStrategy) IsValidResponse(_ *GeminiResponse) bool { return false }
