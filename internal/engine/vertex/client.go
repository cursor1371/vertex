package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
)

const (
	anonBaseURL      = "https://cloudconsole-pa.clients6.google.com"
	batchGraphqlPath = "/v3/entityServices/AiplatformEntityService/schemas/AIPLATFORM_GRAPHQL:batchGraphql"
	anonAPIKey       = "AIzaSyCI-zsRP85UVOi0DjtiCwWBwQ1djDy741g"
)

// AnonAPIKey 返回匿名端点 API key，供目录拉取等非生成类匿名调用复用
// （避免 key 在 vertex / scripts 更新模型.bat / catalog 多处各硬编码一份）。
func AnonAPIKey() string { return anonAPIKey }

// AnonBatchGraphqlURL 返回匿名 batchGraphql 端点基址（不含 key 查询参数）。
func AnonBatchGraphqlURL() string { return anonBaseURL + batchGraphqlPath }

// RequestIDKey 是 context 中存储 reqID 的键类型。
type RequestIDKey struct{}

// RequestIDFromContext 取请求上下文里的 request-id（无则空串）。
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(RequestIDKey{}).(string); ok {
		return v
	}
	return ""
}

type VertexAIClient struct {
	net  *transport.NetworkClient
	pool *recaptcha.TokenPool
	cfg  config.ConfigProvider
	// nodes 是出口节点池消费端口（候选选取与健康态记账）；零值时由方法内防御归一。
	nodes NodePool

	// ownsPool 标记 pool 是否由本客户端在构造时自建：自建的由 Close 负责关闭，
	// 外部注入的（main.go 显式装配 / 测试注入 mock）所有权归调用方。
	ownsPool bool

	// batchURL 非空时覆盖 batchGraphql 端点（测试注入用），空串走动态计算。
	batchURL string
}

// NewVertexAIClient 装配客户端：pool 为 nil 时按 cfg+nodePool 自建 reCAPTCHA 池，
// nodePool 为 nil 时归一为 nopNodePool（引擎走单节点降级路径）。
func NewVertexAIClient(cfg config.ConfigProvider, net *transport.NetworkClient,
	pool *recaptcha.TokenPool, nodePool NodePool,
) *VertexAIClient {
	ownsPool := pool == nil
	if ownsPool {
		pool = recaptcha.NewTokenPool(net, cfg, nodePool)
	}
	if nodePool == nil {
		nodePool = nopNodePool{}
	}
	return &VertexAIClient{
		net:      net,
		pool:     pool,
		cfg:      cfg,
		nodes:    nodePool,
		ownsPool: ownsPool,
	}
}

// Close 释放客户端自建的 reCAPTCHA 池（预取协程等后台资源）。幂等；外部注入的
// 池不受影响，由其所有者关闭。生产装配（main.go）自持 pool 并显式 Close，
// 此处主要服务测试与未来"客户端全权持有"的装配形态。
func (c *VertexAIClient) Close() {
	if c != nil && c.ownsPool {
		c.pool.Close()
	}
}

// nodePool 返回节点池消费端口；零值客户端（测试直构 struct 字面量）防御归一为 nopNodePool。
func (c *VertexAIClient) nodePool() NodePool {
	if c.nodes == nil {
		return nopNodePool{}
	}
	return c.nodes
}

// rtCompensationDelay 返回 rT 第一发补偿的静置间隔（rt_compensation_ms，默认 500ms，
// 0=立即重发）。cfg 缺失（测试直构零值客户端）时兜底 500ms 保持旧行为。
func (c *VertexAIClient) rtCompensationDelay() time.Duration {
	ms := 500
	if c.cfg != nil {
		ms = c.cfg.RTCompensationMs()
	}
	if ms < 0 {
		ms = 500
	}
	return time.Duration(ms) * time.Millisecond
}

// prepareCandidate 并行执行候选启动前置准备：出口建联与 rT 抓取互不依赖
// （rT 抓取内部自建临时会话，从不复用候选连接），关键路径由 T建联+T取rT
// 收敛为 max(T建联, T取rT)。裁决语义见 joinPrepared。
//
// 取消语义无条件最优先：join 后若 ctx 已取消（含取消发生在两任务执行期间），
// 无论建联/抓取结果如何一律丢弃并上浮取消错误——防止请求死亡时的资源类错误
// 被误判为 infra/FailFast 扭曲竞速终局；失败路径上已建成的会话就地关闭，
// 临时 sing-box 实例绝不泄漏。
func (c *VertexAIClient) prepareCandidate(ctx context.Context, proxyURI string) (*transport.Session, string, *VertexError) {
	sess, tok, sessErr, tokErr := runPrepareTasks(
		func() (*transport.Session, error) {
			return c.net.CreateSession(sessionTimeoutFromContext(ctx, 180), proxyURI, RequestIDFromContext(ctx))
		},
		func() (string, error) {
			return c.pool.GetTokenShared(ctx)
		},
	)

	if err := ctx.Err(); err != nil {
		if sess != nil {
			sess.Close()
		}
		return nil, "", NewContextError(err)
	}
	return joinPrepared(sess, sessErr, tok, tokErr)
}

// runPrepareTasks 并发调度两组互不依赖的前置任务并等待双双完成。
// 只负责并发编排与汇合，不做任何错误取舍（裁决见 joinPrepared）；
// 独立成函数以便以受控桩做确定性的并行性测试。
func runPrepareTasks(
	sessFn func() (*transport.Session, error),
	tokFn func() (string, error),
) (sess *transport.Session, tok string, sessErr error, tokErr error) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sess, sessErr = sessFn()
	}()
	go func() {
		defer wg.Done()
		tok, tokErr = tokFn()
	}()
	wg.Wait()
	return
}

// joinPrepared 对并行完成的结果做错误裁决（纯函数，矩阵可穷举单测）：
//  1. rT 失败或为空 → RecaptchaUnavailableError（infra：流式下触发 FailFast 尽早终局；
//     双失败时同样由 rT 错误胜出，省去无谓候选消耗）；
//  2. 仅会话失败 → InternalError（internal：仅该候选出局，其余候选照常竞争）；
//  3. 全部成功 → 会话与 token 原样移交调用方（Close 责任随之转移）。
//
// 任一失败路径上已建成的会话在返回前负责关闭。
func joinPrepared(sess *transport.Session, sessErr error, tok string, tokErr error) (*transport.Session, string, *VertexError) {
	if tokErr != nil || tok == "" {
		if sess != nil {
			sess.Close()
		}
		return nil, "", NewRecaptchaUnavailableError("Could not fetch recaptcha token", tokErr)
	}
	if sessErr != nil {
		return nil, "", NewInternalError("create session: "+sessErr.Error(), nil)
	}
	return sess, tok, nil
}

// getBatchGraphqlURL 每次动态读取配置密钥（cfg 经 SIGHUP 缓存失效热重载），
// 未配置时回落匿名 key；batchURL 被测试注入覆盖时原样返回。
func (c *VertexAIClient) getBatchGraphqlURL() string {
	if c.batchURL != "" {
		return c.batchURL
	}
	key := c.cfg.VertexAPIKey()
	if key == "" {
		key = anonAPIKey
	}
	return anonBaseURL + batchGraphqlPath + "?key=" + key + "&prettyPrint=false"
}

type ParseResultTyped struct {
	Candidates     []*transform.Candidate
	PromptFeedback *transform.PromptFeedback
	UsageMetadata  *transform.UsageMetadata
	ModelVersion   string
	ResponseID     string
	CreateTime     string
	HasError       bool
	ErrorMessage   string
}

func (c *VertexAIClient) buildCompleteResponseTyped(r *ParseResultTyped) (*transform.GeminiResponse, error) {
	if r.HasError {
		return nil, NewInternalError("upstream parse error: "+r.ErrorMessage, nil)
	}
	resp := &transform.GeminiResponse{
		Candidates:     r.Candidates,
		PromptFeedback: r.PromptFeedback,
		UsageMetadata:  r.UsageMetadata,
		ModelVersion:   r.ModelVersion,
		ResponseID:     r.ResponseID,
		CreateTime:     r.CreateTime,
	}
	if len(resp.Candidates) == 0 && resp.PromptFeedback == nil {
		return nil, NewEmptyResponseError("Upstream returned empty response (no content)", nil)
	}
	return resp, nil
}

func collectChunksToParseResultTyped(chunks []*transform.GeminiChunk) *ParseResultTyped {
	s := &ParseResultTyped{}
	candsMap := map[int]*transform.Candidate{}

	for _, chunk := range chunks {
		if chunk == nil {
			continue
		}
		for _, cand := range chunk.Candidates {
			if cand == nil {
				continue
			}
			idx := cand.Index
			existing, ok := candsMap[idx]
			if !ok {
				cCopy := *cand
				if cCopy.Content == nil {
					cCopy.Content = &transform.Content{Role: "model"}
				}
				candsMap[idx] = &cCopy
			} else {
				overlayCandidateTyped(existing, cand)
				if cand.Content != nil && len(cand.Content.Parts) > 0 {
					if existing.Content == nil {
						existing.Content = &transform.Content{Role: "model"}
					}
					existing.Content.Parts = append(existing.Content.Parts, cand.Content.Parts...)
				}
			}
		}
		if chunk.PromptFeedback != nil && s.PromptFeedback == nil {
			s.PromptFeedback = chunk.PromptFeedback
		}
		if chunk.UsageMetadata != nil {
			s.UsageMetadata = chunk.UsageMetadata
		}
		if chunk.ModelVersion != "" {
			s.ModelVersion = chunk.ModelVersion
		}
		if chunk.ResponseID != "" {
			s.ResponseID = chunk.ResponseID
		}
		if chunk.CreateTime != "" {
			s.CreateTime = chunk.CreateTime
		}
	}

	var idxs []int
	for idx := range candsMap {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)

	for _, idx := range idxs {
		c := candsMap[idx]
		if c.Content != nil && len(c.Content.Parts) > 0 {
			c.Content.Parts = mergeFunctionCallPartsTyped(mergeStreamPartsTyped(c.Content.Parts))
		}
		s.Candidates = append(s.Candidates, c)
	}
	return s
}

// fcSlot 是同一次工具调用的跨帧累积槽。
type fcSlot struct {
	name string
	id   string
	buf  string // args 原文累积（快照覆盖 / 分片追加，同 forcedArgsAccumulator 判别）
	out  int    // 本槽在输出切片中的落点（开槽时占位，flush 原地覆写，保住原始位置）
}

// mergeFunctionCallPartsTyped 把同候选内被上游分片下发的普通工具调用归并为单个
// functionCall part。聚合阶段此前只做 Parts 追加，一个分 3 帧下发的调用会变成
// 3 个 args 各为半截原文的独立 functionCall。
//
// 槽生命周期（FunctionCall.ID 无代码依赖、不可靠，仅作辅助身份）：
//   - 完整 JSON（map 或可独立解析的字符串）：快照。与当前开槽同身份 → 覆盖槽并立即
//     收口（同一调用的快照重发）；无开槽但与上一输出 part 同身份且相邻 → 覆盖该 part
//     （连续快照重发）；否则独立成 part（并行调用的第二发由此与第一发隔离）。
//   - 不完整分片：带 name/ID 时——当前槽 buf 已凑齐完整 JSON 视为新调用宣告开新槽，
//     身份一致且未凑齐视为同调用续帧（上游可能逐帧重复 name），身份不同开新槽；
//     不带 name/ID 一律续接当前槽（分片续帧省略 name 是上游既有形态）。
//   - 中间的 text/thought part 不打断续接：同一调用的分片可与正文交错到达。
//
// 快照/分片判别与 forcedArgsAccumulator 一致（isCompleteJSONObject），保证两条
// 路径对同一形态的裁决相同。归并产物始终是新值（copy-on-write），不写穿与 chunk
// 共享的 *FunctionCall 指针（见 signature.go 的纯函数契约）。
// 已知取舍：同名两次调用均以无 name 分片宣告时无法区分，按续接合并——与合成工具
// 累加器的"宁可拼接"取舍一致。
func mergeFunctionCallPartsTyped(parts []transform.Part) []transform.Part {
	var cur *fcSlot
	merged := make([]transform.Part, 0, len(parts))

	// newSlot 在 merged 中先占位再累积：同一调用的分片可与正文交错，flush 必须
	// 原地覆写占位 part，否则归并产物会被追加到末尾、打乱 part 顺序。
	newSlot := func(name, id, buf string) {
		cur = &fcSlot{name: name, id: id, buf: buf, out: len(merged)}
		merged = append(merged, transform.Part{})
	}
	flushWith := func(args any) {
		if cur == nil {
			return
		}
		merged[cur.out] = transform.Part{FunctionCall: &transform.FunctionCall{Name: cur.name, ID: cur.id, Args: args}} //nolint:exhaustruct
		cur = nil
	}
	flush := func() {
		if cur == nil {
			return
		}
		if obj, err := parseJSONObjectTyped(cur.buf); err == nil {
			flushWith(obj)
		} else {
			flushWith(cur.buf) // 拼接后仍非完整 JSON（上游中途截断）：保留原文，不伪造 {}
		}
	}

	for _, p := range parts {
		if p.FunctionCall == nil {
			merged = append(merged, p)
			continue
		}
		fc := p.FunctionCall
		switch a := fc.Args.(type) {
		case map[string]any:
			// 已解析的完整对象：同身份快照覆盖当前槽，否则独立成 part。
			if cur != nil && fc.Name == cur.name && fc.ID == cur.id {
				flushWith(a)
				continue
			}
			flush()
			merged = append(merged, transform.Part{FunctionCall: &transform.FunctionCall{Name: fc.Name, ID: fc.ID, Args: a}}) //nolint:exhaustruct
		case nil:
			// 无 args：完整空调用。
			flush()
			merged = append(merged, transform.Part{FunctionCall: &transform.FunctionCall{Name: fc.Name, ID: fc.ID, Args: map[string]any{}}}) //nolint:exhaustruct
		case string:
			if isCompleteJSONObjectText(a) {
				// 字符串形式的完整快照：落为解析对象（与 map 分支产物形态一致）。
				if cur != nil && fc.Name == cur.name && fc.ID == cur.id {
					obj, _ := parseJSONObjectTyped(a)
					flushWith(obj)
					continue
				}
				// 相邻重复快照（无开槽）：同身份视为同一调用重发，覆盖之。
				if n := len(merged); n > 0 {
					if prev := merged[n-1].FunctionCall; prev != nil && prev.Name == fc.Name && prev.ID == fc.ID {
						if obj, err := parseJSONObjectTyped(a); err == nil {
							prev.Args = obj
						} else {
							prev.Args = a
						}
						continue
					}
				}
				flush()
				args := any(a)
				if obj, err := parseJSONObjectTyped(a); err == nil {
					args = obj
				}
				merged = append(merged, transform.Part{FunctionCall: &transform.FunctionCall{Name: fc.Name, ID: fc.ID, Args: args}}) //nolint:exhaustruct
				continue
			}
			if a == "" {
				// 空字符串不携带内容，仅可能是空调用宣告（正常已被清洗为 {}，防御性兜底）。
				flush()
				merged = append(merged, transform.Part{FunctionCall: &transform.FunctionCall{Name: fc.Name, ID: fc.ID, Args: map[string]any{}}}) //nolint:exhaustruct
				continue
			}
			// 不完整分片：判槽。
			sameSlot := cur != nil
			if sameSlot && (fc.Name != "" || fc.ID != "") {
				switch {
				case fc.Name == cur.name && fc.ID == cur.id && !isCompleteJSONObjectText(cur.buf):
					// 同身份且未凑齐：同调用续帧（上游可能逐帧重复 name）。
				default:
					// 槽已凑齐完整 JSON（前一次调用已完成）或身份不同：新调用宣告。
					sameSlot = false
				}
			}
			if !sameSlot {
				flush()
				newSlot(fc.Name, fc.ID, a)
			} else {
				// 与 forcedArgsAccumulator 同判别：分片一律追加（完整快照已在上方分流）。
				cur.buf += a
			}
		default:
			// 其他形态（数组等）原样保留。
			flush()
			merged = append(merged, p)
		}
	}
	flush()
	return merged
}

// overlayCandidateTyped 把后续帧的候选元数据覆盖到已聚合的候选上。
//
// 此前 existing 分支只更新 FinishReason 与 Content.Parts，而 grounding/citation/
// tokenCount 等几乎只在收尾帧出现——收尾帧的候选必然命中 existing 分支，元数据
// 被整体丢弃。覆盖规则按字段类型分三类：
//   - any/slice（可判 nil）：后帧非空即整体 replace（SafetyRatings 每帧下发完整
//     集合，append 会 N 倍重复）；
//   - 值类型（TokenCount/AvgLogprobs）：后帧非 0 才覆盖（int/float64 无法区分
//     "未出现"与"合法零值"；0.0 是 avgLogprobs 的合法值，此规则会漏掉"终帧明确
//     回 0"，但比首帧遗留脏值安全）。
func overlayCandidateTyped(existing *transform.Candidate, next *transform.Candidate) {
	if next.FinishReason != "" {
		existing.FinishReason = next.FinishReason
	}
	if next.FinishMessage != nil {
		existing.FinishMessage = next.FinishMessage
	}
	if len(next.SafetyRatings) > 0 {
		existing.SafetyRatings = next.SafetyRatings
	}
	if next.CitationMetadata != nil {
		existing.CitationMetadata = next.CitationMetadata
	}
	if next.GroundingMetadata != nil {
		existing.GroundingMetadata = next.GroundingMetadata
	}
	if next.LogprobsResult != nil {
		existing.LogprobsResult = next.LogprobsResult
	}
	if next.TokenCount != 0 {
		existing.TokenCount = next.TokenCount
	}
	if next.AvgLogprobs != 0 {
		existing.AvgLogprobs = next.AvgLogprobs
	}
}

// parseJSONObjectTyped 把文本解析为 JSON 对象（仅对象形态），失败返回错误。
// 与 stream_scanner.go 的 parseJSONObject（[]byte、返回 nil）职责不同：本函数
// 需要区分"解析失败保留原文"与"解析成功空对象 {}"。
func parseJSONObjectTyped(s string) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// isCompleteJSONObjectText 判定文本能否独立解析为 JSON 对象，与 transform 包
// forcedArgsAccumulator 的同名判别（isCompleteJSONObject）语义一致，保证两条路径
// 对同一 args 形态的快照/分片裁决相同。
func isCompleteJSONObjectText(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '{' {
		return false
	}
	var obj map[string]any
	return json.Unmarshal([]byte(s), &obj) == nil
}

func mergeStreamPartsTyped(parts []transform.Part) []transform.Part {
	if len(parts) == 0 {
		return parts
	}
	merged := make([]transform.Part, 0, len(parts))
	var current *transform.Part

	for _, p := range parts {
		if p.Text == "" {
			merged = append(merged, p)
			current = nil
			continue
		}
		if current != nil && current.Thought == p.Thought && current.Text != "" {
			current.Text += p.Text
			if p.ThoughtSignature != "" && current.ThoughtSignature == "" {
				current.ThoughtSignature = p.ThoughtSignature
			}
		} else {
			pCopy := p
			merged = append(merged, pCopy)
			current = &merged[len(merged)-1]
		}
	}
	return merged
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asVertexError(err error) *VertexError {
	var ve *VertexError
	if errors.As(err, &ve) {
		return ve
	}
	return nil
}

// NormalizeError 将任意 error 统一归一化为 *VertexError。
//
// 1. 若 err 已是 *VertexError，直接返回避免双重包装；
// 2. 若包含 context.Canceled / context.DeadlineExceeded，包装为保留 cause 的 ContextError；
// 3. 若为 net.Error，包装为 502 NetworkError；
// 4. 其他未知错误包装为 500 InternalError。
func NormalizeError(err error) *VertexError {
	if err == nil {
		return nil
	}
	var ve *VertexError
	if errors.As(err, &ve) {
		return ve
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewContextError(err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return NewNetworkError(err)
	}

	return NewInternalError(err.Error(), err)
}

// classifyNetworkError 将网络原生 error 统一包装为 *VertexError（内部复用 NormalizeError）。
func classifyNetworkError(err error) *VertexError {
	return NormalizeError(err)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck
	case <-t.C:
		return nil
	}
}

func mapToGeminiChunk(m map[string]any) *transform.GeminiChunk {
	if m == nil {
		return &transform.GeminiChunk{}
	}
	b, err := jsonx.Marshal(m)
	if err != nil {
		return &transform.GeminiChunk{}
	}
	var chunk transform.GeminiChunk
	if err := json.Unmarshal(b, &chunk); err != nil {
		return &transform.GeminiChunk{}
	}
	return &chunk
}
