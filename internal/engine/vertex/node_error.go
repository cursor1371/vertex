package vertex

import (
	"errors"
	"log"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// applyNodeFailure 是节点池健康态的旁路消费者：吃透传错误，改节点池健康态，
// 不触碰错误内容、不影响竞速裁决。以 VertexError 为输入，不返回任何值。
//
// 归因规则（按顺序）：
//   - uri == ""（直连/前置代理）：无操作——直连没有可归属的出口节点，前置代理身份在建连时
//     round-robin 轮换且 Session 不携带实际被轮询到的前置身份，误禁用会破坏前置池容错；
//   - 流式包间空闲超时（ErrStreamIdleTimeout）：15 秒短时避让，避免下一轮反复选中僵死节点
//     （慢/僵死是劣化信号，除冷却外还写亚健康降权，不摘出）；
//   - Kind == "infra"（rT 子系统耗尽）：无操作——全局故障，不归因节点；
//   - Kind == "ratelimit"：30 秒歇息（429 是上游正常竞争，不算节点故障）；
//   - Kind == "auth" / "permission"：**显式带上类别**上报，由节点池按策略决定动作
//     （默认策略为"禁用后恢复探测"：首次即摘出并进入自动探测）；
//   - 其余（含 Kind == "network"）：只上报原始错误文本，类别交给共享分类器判定——
//     "network" 这个粗分类里也可能夹着 DNS/业务超时，文本分类比它更准；
//     认不出来的落 other，而 other 默认不禁用（保守方向：宁可漏摘也不错摘）。
func applyNodeFailure(p NodePool, uri string, err error) {
	if p == nil || uri == "" || err == nil {
		return
	}
	ve := asVertexError(err)
	if ve == nil {
		p.ReportProbe(uri, exitpool.ProbeResult{
			OK: false, ErrStr: err.Error(), Source: nodestore.ProbeSourceRequest,
		})
		return
	}
	switch {
	case errors.Is(err, ErrStreamIdleTimeout) ||
		(ve.Kind == "network" && strings.Contains(ve.Message, "idle timeout")):
		log.Printf("[Racing] 节点 %s 触发流式包间空闲超时，进入 15 秒短时避让", p.NodeName(uri))
		// 慢/僵死是节点劣化信号：除冷却外还写亚健康降权（与 429 的正常竞争不同）。
		p.RecordSlowAvoid(uri, 15)
	case ve.Kind == "infra":
		return
	case ve.Kind == "ratelimit":
		log.Printf("[Racing] 节点 %s 触发 429 API 限制，进入 30 秒短时歇息", p.NodeName(uri))
		p.RecordRateLimit(uri, 30)
	case ve.Kind == "auth", ve.Kind == "permission":
		// 认证/权限失败交统一入口：是否摘出、是否进入自动探测由**策略表**决定
		//（此前 auth 硬编码一次即禁、permission 只记账，两套规则会漂移）。
		// 引擎从 Kind 得知的类别比文本分类可靠，故显式给出。
		class := nodestore.FailureAuth
		if ve.Kind == "permission" {
			class = nodestore.FailurePermission
		}
		log.Printf("[Racing] 节点 %s 触发 %s（上游状态 %s），按策略处理", p.NodeName(uri), class, ve.Status)
		p.ReportProbe(uri, exitpool.ProbeResult{
			OK: false, ErrStr: err.Error(), Class: class, Source: nodestore.ProbeSourceRequest,
		})
	default:
		p.ReportProbe(uri, exitpool.ProbeResult{
			OK: false, ErrStr: err.Error(), Source: nodestore.ProbeSourceRequest,
		})
	}
}
