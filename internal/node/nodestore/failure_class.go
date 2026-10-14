package nodestore

import (
	"regexp"
	"strconv"
	"strings"
)

// 本文件是失败归因的共享内核：把一次失败的错误文本归到固定的 FailureClass，
// 供两个节点池按**处理策略**（见 failure_policy.go）决定动作、供故障记录聚合、
// 供管理面板按类别统计与展开明细。纯函数、无持久化语义（与 health_classify.go 同层）。
//
// 本文件只负责"这是什么故障"，**不负责"该怎么办"**——动作由 PolicySet 决定，
// 因为动作是可配置的（同一类别可以被配成不禁用/单次禁用/禁用后恢复探测）。
// 唯一的例外是 GuardsAgainstGlobalFault：它描述"这类失败是否可能是全局性的"，
// 属于类别自身的属性，与用户选择的动作无关。
//
// 分类顺序即优先级，且**顺序本身是契约**：
//   - unsupported/parse → DNS → 连接级网络 → 超时 → 状态码 → 枚举 → other；
//   - "dial timeout after 15s" 必须先命中连接级网络（含 "dial"）而不是超时类，
//     否则会把已实测通过的节点网络故障路径改坏（评审第 5 条）。

// FailureClass 是失败归因类别（同时作为故障记录的聚合维度与面板展示分类）。
type FailureClass string

const (
	// FailureNodeNetwork 节点级连通性故障：拨号失败 / 拒连 / 连接重置 / TLS 握手失败 /
	// 网络不可达 / 连接级 i/o 超时 / Winsock 的 wsarecv 等"对端无响应"。
	// 默认策略为"禁用后恢复探测"；该类也是全局故障保护的重点对象。
	FailureNodeNetwork FailureClass = "node_network"

	// FailureDNS DNS 解析失败（no such host / NXDOMAIN / lookup 失败）。按评审要求
	// 「只记录、不触发自动禁用」：DNS 走本地或公共解析链路，单节点解析失败不足以判定
	// 节点本身失效，而全局 DNS 故障若参与禁用会把整池节点逐个误禁。
	FailureDNS FailureClass = "dns"

	// FailureAuth 认证 / 密钥失效（401 / UNAUTHENTICATED / API key 无效）。
	// 请求路径（applyNodeFailure）识别到 Kind=="auth" 时显式给出。
	// 默认策略与 permission 相同："禁用后恢复探测"——密钥失效不会自愈，
	// 但只要恢复通道完整（探测成功即回来），一次即禁对节点池的伤害可控。
	FailureAuth FailureClass = "auth"

	// FailurePermission 权限 / 策略拒绝（403 / PERMISSION_DENIED）。
	// 默认策略为"禁用后恢复探测"：生产请求首次 403 即移出可用池并进入自动探测
	//（已确认的产品策略）。"分类 ≠ 处理方式"——permission 只是归因标签，
	// 是否需要禁用由策略表决定。
	// 若是**全局性**的账号 / API Key / 认证配置问题，则由全局故障保护识别
	//（窗口内多个不同节点同因失败）而不归因到单个节点。
	FailurePermission FailureClass = "permission"

	// FailureRateLimit 限流（429 / RESOURCE_EXHAUSTED）：上游正常竞争，只做冷却避让。
	FailureRateLimit FailureClass = "ratelimit"

	// FailureTimeout 超时类（业务 deadline exceeded / 流式包间空闲超时 / 通用 timeout）：
	// 慢节点信号，默认走亚健康降权与短冷却。
	// **注意**：连接级超时（"dial timeout after 15s"、"i/o timeout"）先命中连接级网络判定，
	// 不在本类别——这是已实测通过的路径，分类顺序不得调整。
	FailureTimeout FailureClass = "timeout"

	// FailureUpstream5xx 上游 5xx / UNAVAILABLE：公共依赖或上游抖动，只记录。
	FailureUpstream5xx FailureClass = "upstream"

	// FailureProbeFlow 探测流程自身错误（节点解析失败、请求构造失败、签名缺失等）：
	// 问题在测试器或请求构造而非节点，绝不能据此禁用。
	FailureProbeFlow FailureClass = "probe_flow"

	// FailureUnsupported 协议不支持：节点对本代理不可用，由 admin 测试路径直接禁用，
	// 来源标记 unsupported，不参与自动恢复。
	FailureUnsupported FailureClass = "unsupported"

	// FailureUnknown 未归类（面板展示为「其他」）：默认只记录，不禁用。
	// 它是"真正无法判断归因"的兜底——明确属于节点不可达的信号必须先被上面的规则收走
	// （如 wsarecv / failed to respond 已归入 FailureNodeNetwork），本类别应持续收窄。
	FailureUnknown FailureClass = "other"
)

// FailureInfo 是一次失败的归类结果。
type FailureInfo struct {
	Class FailureClass
	// Status 是从错误文本中提取到的 HTTP 状态码（0 = 未提取到）。
	Status int
	// Code 是错误码：优先取 gRPC / Google 状态枚举名（如 RESOURCE_EXHAUSTED），
	// 其次回退为 HTTP 状态码字符串，都没有则为空。供面板展示"完整错误码"。
	Code string
	// Detail 是脱敏并截断后的原始错误文本（面板展开项直接展示它）。
	Detail string
	// Sig 是聚合签名：由 Detail 归一而来（数字串折叠为 #），作为故障记录的聚合键，
	// 使"同一类故障的不同次发生"落到同一行做计数。脱敏在归一之前完成，签名不含凭据。
	Sig string
}

// faultDetailMaxLen / faultSigMaxLen 分别是入库原文与聚合签名的长度上限：
// 避免超长上游响应体把故障表撑大，同时保留足够定位问题的上下文片段。
const (
	faultDetailMaxLen = 1024
	faultSigMaxLen    = 240
)

var (
	// httpStatusRe 抓取错误文本里的 HTTP 状态码：覆盖本仓库各路径实际产生的写法
	//（"unexpected HTTP response status: 403" / "status code 403" / "HTTP 429" / "code = 403"）。
	httpStatusRe = regexp.MustCompile(`(?i)\b(?:status|code|http)\D{0,10}?([1-5]\d{2})\b`)

	// urlQueryRe 匹配 URL 的完整 query：query 里可能带 API key / token / 签名，
	// 只保留 scheme+host+path，整个 query 替换为 <redacted>（域名与路径仍在，便于定位）。
	urlQueryRe = regexp.MustCompile(`(?i)(https?://[^\s"'?#]+)\?[^\s"']*`)

	// headerRe 匹配请求头形式的敏感字段（key: value）。值取到行尾/引号为止——
	// "Authorization: Bearer <token>" 的凭据在第二个词上，只吃一个词会把 token 漏出去。
	headerRe = regexp.MustCompile(`(?i)\b(authorization|cookie|set-cookie|x-goog-api-key|x-api-key)\s*:\s*[^\r\n"']{0,300}`)

	// bearerRe 匹配裸的 Bearer 凭据（请求头被截断或只贴了片段时仍要脱敏）。
	bearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)

	// secretColonRe / secretEqualRe 匹配敏感键值：冒号形式可跨空格取值（值可能含空格），
	// 等号形式只取单个词（避免把 & 分隔的参数串整段吃掉）。
	// 覆盖评审点名要脱敏的 API Key，以及常见的 token/secret/签名类参数。
	secretColonRe = regexp.MustCompile(`(?i)\b(api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|token|secret|password|passwd|pwd|session[-_]?id|sessionid|jwt|credential|signature|sig)\s*:\s*[^\r\n"']{0,300}`)
	secretEqualRe = regexp.MustCompile(`(?i)\b(api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|token|secret|password|passwd|pwd|session[-_]?id|sessionid|jwt|credential|signature|sig)\s*=\s*("[^"]*"|'[^']*'|[^\s,;&"']+)`)

	// digitsRe 把数字串折叠为 #，让端口 / 请求 ID / 状态码 / 耗时不影响聚合。
	digitsRe = regexp.MustCompile(`\d+`)
)

// knownErrorCodes 是需要在面板展示为"完整错误码"的上游枚举名。
// QUERY_SIGNATURE_NOT_FOUND 必须排在 NOT_FOUND 之前（前者包含后者）。
var knownErrorCodes = []string{
	"QUERY_SIGNATURE_NOT_FOUND",
	"RESOURCE_EXHAUSTED",
	"PERMISSION_DENIED",
	"FAILED_PRECONDITION",
	"DEADLINE_EXCEEDED",
	"UNAUTHENTICATED",
	"INVALID_ARGUMENT",
	"OUT_OF_RANGE",
	"UNIMPLEMENTED",
	"UNAVAILABLE",
	"DATA_LOSS",
	"ALREADY_EXISTS",
	"NOT_FOUND",
	"CANCELLED",
	"ABORTED",
	"INTERNAL",
}

// ClassifyFailure 把一次失败的错误文本归类。调用方若已从结构化错误（如 VertexError.Kind）
// 得知更可信的类别，应显式覆盖 Class——文本分类只作为无结构信息时的兜底。
func ClassifyFailure(errStr string) FailureInfo {
	trimmed := strings.TrimSpace(errStr)
	info := FailureInfo{Class: FailureUnknown}
	if trimmed == "" {
		return info
	}

	info.Status = extractHTTPStatus(trimmed)
	info.Code = extractErrorCode(trimmed, info.Status)
	info.Detail = MaskSecrets(trimmed)
	info.Sig = faultSignature(info.Detail)

	lower := strings.ToLower(trimmed)
	switch {
	case hasAnyPrefix(lower, "unsupported:", "unsupported "):
		info.Class = FailureUnsupported
	case hasAnyPrefix(lower, "parse failed"):
		info.Class = FailureProbeFlow
	case info.Code == "QUERY_SIGNATURE_NOT_FOUND":
		// 请求签名缺失是请求构造问题（上游提示），与节点可用性无关。
		info.Class = FailureProbeFlow
	case isDNSFailure(lower):
		info.Class = FailureDNS
	case isConnNetworkFailure(lower):
		info.Class = FailureNodeNetwork
	case isTimeoutFailure(lower):
		info.Class = FailureTimeout
	case info.Code == "RESOURCE_EXHAUSTED" || info.Status == 429:
		info.Class = FailureRateLimit
	case info.Code == "PERMISSION_DENIED" || info.Status == 403:
		info.Class = FailurePermission
	case info.Code == "UNAUTHENTICATED" || info.Status == 401:
		info.Class = FailureAuth
	case info.Status >= 500 || info.Code == "UNAVAILABLE":
		info.Class = FailureUpstream5xx
	}
	return info
}

// GuardsAgainstGlobalFault 报告该类别是否需要受全局故障保护约束。
//
// 语义已锁死为"只防、不治"：保护命中时**只阻止后续节点被摘除**（照常记账），
// 不解除、不恢复任何已存在的禁用状态——已禁用节点仍只按各自来源的既有规则恢复
// （health → 探测成功；permanent/single_shot/admin_test/unsupported/人工 → 人工启用）。
//
// 范围：node_network（链路/公共依赖抖动的典型）+ permission + auth。后两者纳入，
// 正是"仅当能够明确判断为全局账号 / API Key / 认证配置问题时才不归因到单节点"的落地判据
// （窗口内 ≥N 个不同节点同因失败）。DNS 不纳入：它本就不禁用，无需保护。
func (c FailureClass) GuardsAgainstGlobalFault() bool {
	return c == FailureNodeNetwork || c == FailurePermission || c == FailureAuth
}

func hasAnyPrefix(lower string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// isDNSFailure 判定 DNS 解析类失败。包含 Go 解析器与上游返回的几种典型文本，
// 以及 Issue #5 原文里的 `lookup <host>: (exchange4: NXDOMAIN | exchange6: NXDOMAIN)`。
func isDNSFailure(lower string) bool {
	return strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "nxdomain") ||
		strings.Contains(lower, "server misbehaving") ||
		strings.Contains(lower, "temporary failure in name resolution") ||
		strings.Contains(lower, "lookup ") ||
		strings.Contains(lower, "dns")
}

// isConnNetworkFailure 判定连接级网络故障（强连通性信号）。
//
// 有意收窄：不匹配裸 "connection"（会命中一切恰好含该词的业务/协议错误）。
// 关键词表按"明确表示对端不可达"收录，覆盖 Windows(Winsock) / POSIX 两套表述——
// 评审实测的 `wsarecv: ... connected host has failed to respond` 此前落入 other，
// 现应归入本类别并进入自动禁用/探测流程。
func isConnNetworkFailure(lower string) bool {
	// 连接建立与读写阶段的通用失败信号。
	patterns := []string{
		"dial",                   // dial tcp ... / dial timeout after 15s / dial: i/o timeout
		"refused",                // connection refused
		"connection reset",       // 对端 RESET
		"network is unreachable", // 网络不可达
		"no route to host",       // 无路由（POSIX）
		"host is down",           // 主机不可达
		"network is down",        // 链路断开
		"tls handshake",          // TLS 握手失败
		"i/o timeout",            // 连接级读写超时（先于超时类判定）
		"connection timed out",   // 连接阶段超时（POSIX 表述，不含 "dial"）
		// Winsock（Windows）表述：实测出现过，且明确表示对端未响应。
		"wsarecv",                   // 接收失败
		"wsasend",                   // 发送失败
		"wsaconnect",                // 连接失败
		"wsaeconnreset",             // 连接被重置
		"forcibly closed",           // "An existing connection was forcibly closed by the remote host"
		"failed to respond",         // "... connected host has failed to respond"
		"did not properly respond",  // "... the connected party did not properly respond"
		"connection attempt failed", // "A connection attempt failed because..."
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// isTimeoutFailure 判定超时类失败。慢节点是劣化信号而非失效信号，
// 走亚健康降权与短冷却，不参与禁用。
func isTimeoutFailure(lower string) bool {
	return strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "idle timeout") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "context canceled")
}

// extractHTTPStatus 从错误文本抓取 HTTP 状态码，取不到返回 0。
func extractHTTPStatus(s string) int {
	m := httpStatusRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// extractErrorCode 优先返回上游枚举错误码，其次回退 HTTP 状态码字符串。
func extractErrorCode(s string, status int) string {
	upper := strings.ToUpper(s)
	for _, c := range knownErrorCodes {
		if strings.Contains(upper, c) {
			return c
		}
	}
	if status > 0 {
		return strconv.Itoa(status)
	}
	return ""
}

// MaskSecrets 脱敏错误文本：URL 的完整 query 整体替换、敏感键的值替换为 <redacted>、
// 超长文本截断。域名、路径、错误码、HTTP 状态码与错误类型等定位信息全部保留。
//
// 这是"故障详情可安全展示与一键复制"的前提：面板展示的原文必须已经过它处理。
func MaskSecrets(s string) string {
	out := urlQueryRe.ReplaceAllString(s, "$1?<redacted>")
	out = headerRe.ReplaceAllString(out, "$1=<redacted>")
	out = bearerRe.ReplaceAllString(out, "Bearer=<redacted>")
	out = secretColonRe.ReplaceAllString(out, "$1=<redacted>")
	out = secretEqualRe.ReplaceAllString(out, "$1=<redacted>")
	if len(out) > faultDetailMaxLen {
		out = out[:faultDetailMaxLen] + "…<truncated>"
	}
	return out
}

// faultSignature 把脱敏后的原文归一为聚合签名：小写化、数字串折叠为 #、压缩空白并截断。
func faultSignature(detail string) string {
	sig := digitsRe.ReplaceAllString(strings.ToLower(detail), "#")
	sig = strings.Join(strings.Fields(sig), " ")
	if len(sig) > faultSigMaxLen {
		sig = sig[:faultSigMaxLen]
	}
	return sig
}
