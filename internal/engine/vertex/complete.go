package vertex

import (
	"context"
	"sort"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
)

type candidateResult struct {
	proxyURI string
	resp     *transform.GeminiResponse
	err      error
}

func (c *VertexAIClient) CompleteChat(ctx context.Context, model string, req *transform.GeminiRequest, strategy transform.ModelStrategy) (*transform.GeminiResponse, error) {
	if strategy == nil {
		strategy = transform.SharedModelFamilyRouter().For(model)
	}
	// 预取池自停解除点，每请求通知一次（重复通知由池内按 reqID 合并）。
	c.pool.NoteRequest(RequestIDFromContext(ctx))
	run := func(ctx context.Context, proxyURI string) (*transform.GeminiResponse, error) {
		return c.runSingleCandidate(ctx, model, req, proxyURI, strategy)
	}
	// 非流式无部分交付（不标 Truncated），中途断流只是候选失败，由窗口引擎补位承接；
	// 补位、发射预算与 FailFast 门禁均由 RunRace 内部自治。
	resp, err := RunRace(ctx, c.nodePool(), c.cfg, run,
		WithWinningCheck(func(resp *transform.GeminiResponse) bool {
			return candidateFinishTyped(resp) == "STOP" && strategy.IsValidResponse(resp)
		}), WithCollectedFinalizer(func(results []raceResult[*transform.GeminiResponse]) (*transform.GeminiResponse, string, error) {
			cr := make([]candidateResult, len(results))
			for i, r := range results {
				cr[i] = candidateResult{proxyURI: r.uri, resp: r.val, err: r.err}
			}
			return pickBestResult(cr, strategy)
		}), WithLatencyLabel[*transform.GeminiResponse]("总耗时"))
	if err != nil {
		return nil, NormalizeError(err)
	}
	return resp, nil
}

// runSingleCandidate 执行单候选单次尝试（L1 透传层非流式版）：
// 并行建联 + 取 token（见 prepareCandidate）+ 单次 attempt，原样上报真实错误。
// runSingleCandidate 同样走 executeStreamingAttempt（上游侧恒为流式），
// 故非流式路径也能采集上游首字节；胜出判定由 CompleteChat 的 WithWinningCheck 给出，
// 此处只负责把本候选的首字节记入请求级采集器（CAS 保证只有胜出者生效）。
func (c *VertexAIClient) runSingleCandidate(ctx context.Context, model string, req *transform.GeminiRequest, proxyURI string, strategy transform.ModelStrategy) (*transform.GeminiResponse, error) {
	var chunks []*transform.GeminiChunk
	var validChunkCount int
	reqTiming := TimingFromContext(ctx)
	candTiming := NewUpstreamTiming()
	attemptCtx := WithUpstreamTiming(ctx, candTiming)

	sess, tok, err := c.prepareCandidate(attemptCtx, proxyURI)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	attemptErr := withRTFirstTryCompensation(attemptCtx, c.rtCompensationDelay(), func() error {
		return c.executeStreamingAttempt(attemptCtx, sess, model, req, tok, func(chunk *transform.GeminiChunk) bool {
			if chunk == nil {
				return true
			}
			chunks = append(chunks, chunk)
			if strategy.IsValidChunk(chunk) {
				validChunkCount++
				reqTiming.MarkWinner(candTiming)
			}
			return true
		}, strategy)
	})
	if attemptErr != nil {
		return nil, NormalizeError(attemptErr)
	}
	if validChunkCount == 0 {
		return nil, NewEmptyResponseError("Upstream returned no valid content", nil)
	}

	result := collectChunksToParseResultTyped(chunks)
	resp, buildErr := c.buildCompleteResponseTyped(result)
	if buildErr != nil {
		return nil, buildErr
	}

	// 发往上游的 payload 恒由三家族 BuildVariables 经 BuildSafetySettingsTyped 注入 4×OFF
	// （无视 req.SafetySettings），故原"req.SafetySettings==nil 且上游返回 SAFETY 时以 4×OFF 重试"
	// 分支即便命中，重试载荷也与首次逐字节一致、必得相同结果，属确定无效的重复请求；已删除。
	return resp, nil
}

// pickBestResult 从候选结果中选出最佳者，并返回其所属节点 URI
// （URI 供竞速统计把"真正成功的那个节点"记入胜出，其余候选计为不成功）。
func pickBestResult(results []candidateResult, strategy transform.ModelStrategy) (*transform.GeminiResponse, string, error) {
	if len(results) == 0 {
		return nil, "", NewEmptyResponseError("no viable candidate results", nil)
	}
	sort.Slice(results, func(i, j int) bool {
		fi := candidateFinishTyped(results[i].resp)
		fj := candidateFinishTyped(results[j].resp)
		if fi == "MAX_TOKENS" && fj != "MAX_TOKENS" {
			return true
		}
		if fj == "MAX_TOKENS" && fi != "MAX_TOKENS" {
			return false
		}
		return responseContentLengthTyped(results[i].resp) > responseContentLengthTyped(results[j].resp)
	})
	for _, r := range results {
		if isSafetyBlockedResponse(r.resp) {
			continue
		}
		if strategy != nil {
			if strategy.IsValidResponse(r.resp) {
				return r.resp, r.proxyURI, nil
			}
		} else if hasViableResponseTyped(r.resp) {
			return r.resp, r.proxyURI, nil
		}
	}
	// 所有候选均无有效内容时：若任一候选被安全审查拦截（finishReason=SAFETY/RECITATION 等
	// 或 BlockReason 拦截），返回 safety 错误（携带真实拦截原因）而非退化为 502/500 内部错误，
	// 避免网关误判为服务故障。
	for _, r := range results {
		if isSafetyBlockedResponse(r.resp) {
			reason := "SAFETY"
			if r.resp.PromptFeedback != nil && transform.IsBlockReason(r.resp.PromptFeedback.BlockReason) {
				reason = strings.ToUpper(strings.TrimSpace(r.resp.PromptFeedback.BlockReason))
			} else if fr := strings.ToUpper(strings.TrimSpace(candidateFinishTyped(r.resp))); transform.IsSafetyFinishReason(fr) {
				reason = fr
			}
			return nil, "", NewSafetyError("Blocked by safety filter", reason, nil)
		}
	}
	return nil, "", NewEmptyResponseError("no viable candidate results", nil)
}

func isSafetyBlockedResponse(resp *transform.GeminiResponse) bool {
	return transform.IsSafetyResponse(resp)
}

// firstNonNilCandidateTyped 取首个非 nil 候选。typed 解码路径与 collectChunks
// 均会过滤 nil，但本组函数位于竞速热路径（WithWinningCheck / pickBestResult），
// legacy 回退或畸形帧一旦漏网即全进程 panic，与 transform.firstCandidateTyped
// 保持同等防御（纵深防御，非活跃 bug）。
func firstNonNilCandidateTyped(resp *transform.GeminiResponse) *transform.Candidate {
	if resp == nil {
		return nil
	}
	for _, c := range resp.Candidates {
		if c != nil {
			return c
		}
	}
	return nil
}

func candidateFinishTyped(resp *transform.GeminiResponse) string {
	c := firstNonNilCandidateTyped(resp)
	if c == nil {
		return ""
	}
	return c.FinishReason
}

func hasViableResponseTyped(resp *transform.GeminiResponse) bool {
	c := firstNonNilCandidateTyped(resp)
	if c == nil || c.Content == nil {
		return false
	}
	return len(c.Content.Parts) > 0
}

func responseContentLengthTyped(resp *transform.GeminiResponse) int {
	c := firstNonNilCandidateTyped(resp)
	if c == nil || c.Content == nil {
		return 0
	}
	total := 0
	for _, p := range c.Content.Parts {
		total += len(p.Text)
	}
	return total
}
