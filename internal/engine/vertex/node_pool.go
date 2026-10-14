package vertex

import (
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
)

// NodePool 是竞速引擎对出口节点池的最小消费契约（生产实现 *exitpool.Manager）。
// 类型位引用 []exitpool.Node 为 AGENTS.md 铁律 #10 记录的显式豁免（R3 下合法）：
// 候选结构体跨域只读传递。
type NodePool interface {
	// SelectForParallel 严格选点：按健康分层排序，429 冷却中的 Tier 2 节点被跳过。
	SelectForParallel(k int, debugMode bool) []exitpool.Node
	// SelectForParallelRelaxed 宽松选点：忽略冷却期（非常时期按优先级强行补位），
	// Disabled 节点仍被排除。窗口引擎在严格通道供给不足时以此兜底。
	SelectForParallelRelaxed(k int, debugMode bool) []exitpool.Node
	NodeName(rawURI string) string
	RecordTest(uri string, ok bool, ms float64, errStr string)
	// SelectForTokenFetch 是 rT 抓取专用选点（见 recaptcha.NodeSource）：把对 rT 差/慢的
	// 节点排后，不影响普通请求选点。
	SelectForTokenFetch(k int, debugMode bool) []exitpool.Node
	// TokenFetchRank 返回节点当前的 rT 降权层（见 recaptcha.NodeSource），仅用于展示。
	TokenFetchRank(uri string) int
	// RecordTokenFetch 上报一次 rT token 抓取结果与耗时（见 recaptcha.NodeSource）：
	// 只更新 rT 健康度（rT 选点降权/冷却），不参与普通请求选点、也不禁用节点。
	RecordTokenFetch(uri string, outcome exitpool.TokenFetchOutcome, ms int64, errStr string)
	// RecordRateLimit 429 短时冷却：只避让与独立计次，不算失败、不降权、不禁用
	//（429 是上游正常竞争限流，节点本身没有故障）。
	RecordRateLimit(uri string, seconds int)
	// RecordSlowAvoid 流式空闲超时（慢/僵死节点）避让：除冷却外还写亚健康降权，
	// 与 429 的"正常竞争"语义不同。
	RecordSlowAvoid(uri string, seconds int)
	BatchUpdateNodesDisabled(uris []string, disabled bool)
	// ReportProbe 是请求路径的**统一上报告口**：把失败交给节点池，由它按类别归因
	// 并按**策略表**决定动作（不禁用 / 单次禁用 / 禁用后进入自动探测）。
	//
	// 之所以收敛到单一入口：此前 auth 走 DisableByHealth 一次即禁、permission 落到默认
	// 分支只记账，两套规则与池内策略表并存会互相漂移（403 就曾因此永不被摘出）。
	// 显式类别（Class 非空）优先于文本分类——引擎已从 VertexError.Kind 得知更可靠的信息。
	// 返回本次是否真的摘出了节点（引擎不消费，仅供调用方与测试断言）。
	ReportProbe(uri string, r exitpool.ProbeResult) bool
	IncInFlight(uri string)
	DecInFlight(uri string)
}

// nopNodePool 是节点池未注入时的防御性空实现：选点返回空、记账全部无操作，
// 引擎自然走 ActiveNodeURI 单节点降级路径，与直连/锁定模式语义一致。
type nopNodePool struct{}

func (nopNodePool) SelectForParallel(int, bool) []exitpool.Node                        { return nil }
func (nopNodePool) SelectForParallelRelaxed(int, bool) []exitpool.Node                 { return nil }
func (nopNodePool) NodeName(string) string                                             { return "" }
func (nopNodePool) RecordTest(string, bool, float64, string)                           {}
func (nopNodePool) SelectForTokenFetch(int, bool) []exitpool.Node                      { return nil }
func (nopNodePool) TokenFetchRank(string) int                                          { return 0 }
func (nopNodePool) RecordTokenFetch(string, exitpool.TokenFetchOutcome, int64, string) {}
func (nopNodePool) RecordRateLimit(string, int)                                        {}
func (nopNodePool) RecordSlowAvoid(string, int)                                        {}
func (nopNodePool) BatchUpdateNodesDisabled([]string, bool)                            {}
func (nopNodePool) ReportProbe(string, exitpool.ProbeResult) bool                      { return false }
func (nopNodePool) IncInFlight(string)                                                 {}
func (nopNodePool) DecInFlight(string)                                                 {}
