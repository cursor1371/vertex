package nodestore

import (
	"strings"
	"testing"
)

// TestClassifyFailure 是失败归因的权威口径测试：覆盖 Issue #5 正文列出的两类真实失败
// （403 与 DNS NXDOMAIN），以及全部类别的判定与"是否参与阈值自动禁用"。
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name        string
		errStr      string
		wantClass   FailureClass
		wantStatus  int
		wantCode    string
		wantDisable bool
	}{
		{
			// Issue #5 原文样例 1：GraphQL 403。属权限/策略拒绝，绝不据此禁用节点。
			name:        "issue原文_403权限拒绝_默认会摘出",
			errStr:      `Post "https://cloudconsole-pa.clients6.google.com/v3/entityServices/AiplatformEntityService/schemas/AIPLATFORM_GRAPHQL:batchGraphql?d=abc&key=SECRET": unexpected HTTP response status: 403`,
			wantClass:   FailurePermission,
			wantStatus:  403,
			wantCode:    "403",
			wantDisable: true,
		},
		{
			// Issue #5 原文样例 2：reCAPTCHA anchor 域名解析失败（NXDOMAIN）。
			// 评审第 3 条：DNS 错误只记录、不触发自动禁用。
			name:      "issue原文_DNS_NXDOMAIN_只记录不摘出",
			errStr:    `GET anchor 失败: Get "https://www.google.com/recaptcha/enterprise/anchor?k=x": lookup ct.cloudflare.byoip.top: (exchange4: NXDOMAIN | exchange6: NXDOMAIN)`,
			wantClass: FailureDNS,
		},
		{
			name:        "拨号被拒_默认即摘出",
			errStr:      "dial tcp 1.2.3.4:443: connect: connection refused",
			wantClass:   FailureNodeNetwork,
			wantDisable: true,
		},
		{
			name:        "连接级读写超时_默认即摘出",
			errStr:      "dial tcp 1.2.3.4:443: i/o timeout",
			wantClass:   FailureNodeNetwork,
			wantDisable: true,
		},
		{
			name:        "TLS握手失败_默认即摘出",
			errStr:      "net/http: TLS handshake timeout",
			wantClass:   FailureNodeNetwork,
			wantDisable: true,
		},
		{
			name:        "网络不可达_默认即摘出",
			errStr:      "dial tcp: connect: network is unreachable",
			wantClass:   FailureNodeNetwork,
			wantDisable: true,
		},
		{
			name:        "连接重置_默认即摘出",
			errStr:      "read tcp: connection reset by peer",
			wantClass:   FailureNodeNetwork,
			wantDisable: true,
		},
		{
			name:        "整体超时_只降权",
			errStr:      "context deadline exceeded",
			wantClass:   FailureTimeout,
			wantDisable: false,
		},
		{
			name:      "流式空闲超时_只降权",
			errStr:    "stream idle timeout",
			wantClass: FailureTimeout,
		},
		{
			name:       "限流429_只冷却",
			errStr:     "unexpected HTTP response status: 429",
			wantClass:  FailureRateLimit,
			wantStatus: 429,
			wantCode:   "429",
		},
		{
			name:      "限流枚举_只冷却",
			errStr:    "status: RESOURCE_EXHAUSTED",
			wantClass: FailureRateLimit,
			wantCode:  "RESOURCE_EXHAUSTED",
		},
		{
			name:        "认证失败401",
			errStr:      "unexpected HTTP response status: 401",
			wantClass:   FailureAuth,
			wantStatus:  401,
			wantCode:    "401",
			wantDisable: true,
		},
		{
			name:        "认证枚举",
			errStr:      "status: UNAUTHENTICATED",
			wantClass:   FailureAuth,
			wantCode:    "UNAUTHENTICATED",
			wantDisable: true,
		},
		{
			name:       "上游503_只记录",
			errStr:     "status: 503, body: upstream busy",
			wantClass:  FailureUpstream5xx,
			wantStatus: 503,
			wantCode:   "503",
		},
		{
			name:      "上游枚举UNAVAILABLE",
			errStr:    "UNAVAILABLE: backend down",
			wantClass: FailureUpstream5xx,
			wantCode:  "UNAVAILABLE",
		},
		{
			name:      "协议不支持",
			errStr:    "unsupported: 未知协议 vmess",
			wantClass: FailureUnsupported,
		},
		{
			name:      "解析失败_探测流程错误",
			errStr:    "parse failed: invalid port",
			wantClass: FailureProbeFlow,
		},
		{
			name:      "签名缺失_探测流程错误",
			errStr:    "QUERY_SIGNATURE_NOT_FOUND",
			wantClass: FailureProbeFlow,
			wantCode:  "QUERY_SIGNATURE_NOT_FOUND",
		},
		{
			name:      "空错误文本_归为其他",
			errStr:    "",
			wantClass: FailureUnknown,
		},
		{
			name:      "无关文本_归为其他且不禁用",
			errStr:    "some entirely unrelated message",
			wantClass: FailureUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailure(tc.errStr)
			if got.Class != tc.wantClass {
				t.Fatalf("Class = %q, want %q (errStr=%q)", got.Class, tc.wantClass, tc.errStr)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, tc.wantStatus)
			}
			if tc.wantCode != "" && got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			// 动作断言走策略表：默认策略下"首次匹配即移出可用池"的类别。
			pol := DefaultPolicySet().For(got.Class)
			if pol.DisablesOnFirstFailure() != tc.wantDisable {
				t.Errorf("默认策略 DisablesOnFirstFailure() = %v, want %v（类别 %s）",
					pol.DisablesOnFirstFailure(), tc.wantDisable, got.Class)
			}
		})
	}
}

// TestClassifyFailure_DNSNeverDisables 单独锁定评审第 3 条：任何 DNS 形态都不得被禁用。
// DNS 的默认策略是 record_only：不移出可用池、不进自动探测、不永久禁用，
// 也因此不需要为它引入全局故障保护。
func TestClassifyFailure_DNSNeverDisables(t *testing.T) {
	dnsErrors := []string{
		"dial tcp: lookup example.com: no such host",
		"lookup a.b.c: (exchange4: NXDOMAIN | exchange6: NXDOMAIN)",
		"dns: server misbehaving",
		"lookup example.com on 8.8.8.8:53: temporary failure in name resolution",
	}
	for _, e := range dnsErrors {
		info := ClassifyFailure(e)
		if info.Class != FailureDNS {
			t.Errorf("ClassifyFailure(%q) = %q, want %q", e, info.Class, FailureDNS)
		}
		pol := DefaultPolicySet().For(info.Class)
		if pol.DisablesOnFirstFailure() || pol.ProbesForRecovery() {
			t.Errorf("ClassifyFailure(%q) 默认策略不得禁用/进探测, got %s", e, pol.Action)
		}
	}
}

// TestClassifyFailure_WinsockUnreachable 锁定评审实测样例：Winsock 的"对端未响应"
// 必须归入 node_network（默认策略即"禁用后恢复探测"），不得继续落在 other。
func TestClassifyFailure_WinsockUnreachable(t *testing.T) {
	samples := []string{
		"read tcp 1.2.3.4:443: wsarecv: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.",
		"wsasend: An existing connection was forcibly closed by the remote host.",
		"dial tcp 1.2.3.4:443: connectex: A connection attempt failed because the connected party did not properly respond.",
		"connect: no route to host",
		"connect: host is down",
	}
	for _, e := range samples {
		info := ClassifyFailure(e)
		if info.Class != FailureNodeNetwork {
			t.Errorf("ClassifyFailure(%q) = %q, want %q（明确不可达不得落入 other）", e, info.Class, FailureNodeNetwork)
		}
		if !DefaultPolicySet().For(info.Class).DisablesOnFirstFailure() {
			t.Errorf("%q 应进入「首次即移出可用池」的默认流程", e)
		}
	}
}

// TestClassifyFailure_DialTimeoutStillNetwork 锁定评审第 5 条：连接级超时必须继续
// 归入 node_network，不能因为存在 timeout 类别就把已实测通过的节点网络故障路径改坏。
func TestClassifyFailure_DialTimeoutStillNetwork(t *testing.T) {
	if got := ClassifyFailure("dial timeout after 15s").Class; got != FailureNodeNetwork {
		t.Fatalf("dial timeout 应归入 %q, got %q", FailureNodeNetwork, got)
	}
	if got := ClassifyFailure("dial tcp 1.2.3.4:443: i/o timeout").Class; got != FailureNodeNetwork {
		t.Fatalf("i/o timeout 应归入 %q, got %q", FailureNodeNetwork, got)
	}
	// 业务 deadline 才归超时类（不禁用）。
	if got := ClassifyFailure("context deadline exceeded").Class; got != FailureTimeout {
		t.Fatalf("context deadline exceeded 应归入 %q, got %q", FailureTimeout, got)
	}
	if DefaultPolicySet().For(FailureTimeout).DisablesOnFirstFailure() {
		t.Error("业务/上游超时默认不得摘出节点")
	}
}

// TestDefaultPolicies_DisableAndProbe 锁定开工前确认的默认策略表（防回退）：
// 403 必须进入自动恢复流程（不再"永不自动禁用"）；other 保守不禁用。
func TestDefaultPolicies_DisableAndProbe(t *testing.T) {
	set := DefaultPolicySet()
	cases := []struct {
		class       FailureClass
		wantAction  PolicyAction
		wantDisable bool
		wantProbe   bool
	}{
		{FailureNodeNetwork, PolicyDisableThenProbe, true, true},
		{FailurePermission, PolicyDisableThenProbe, true, true},
		{FailureAuth, PolicyDisableThenProbe, true, true},
		{FailureDNS, PolicyRecordOnly, false, false},
		{FailureUnknown, PolicyRecordOnly, false, false},
		{FailureTimeout, PolicyRecordOnly, false, false},
		{FailureRateLimit, PolicyRecordOnly, false, false},
		{FailureUpstream5xx, PolicyRecordOnly, false, false},
		{FailureProbeFlow, PolicyRecordOnly, false, false},
		{FailureUnsupported, PolicyRecordOnly, false, false},
	}
	for _, tc := range cases {
		pol := set.For(tc.class)
		if pol.Action != tc.wantAction {
			t.Errorf("%s 默认动作=%q, want %q", tc.class, pol.Action, tc.wantAction)
		}
		if pol.DisablesOnFirstFailure() != tc.wantDisable {
			t.Errorf("%s DisablesOnFirstFailure=%v, want %v", tc.class, pol.DisablesOnFirstFailure(), tc.wantDisable)
		}
		if pol.ProbesForRecovery() != tc.wantProbe {
			t.Errorf("%s ProbesForRecovery=%v, want %v", tc.class, pol.ProbesForRecovery(), tc.wantProbe)
		}
	}
}

// TestIsNetworkClassFailure_ExcludesDNS 锁定共享判定的行为变更：
// DNS 相关文本不再被判为网络类失败（旧实现包含 "no such host"，会导致全球 DNS 故障误禁整池）。
func TestIsNetworkClassFailure_ExcludesDNS(t *testing.T) {
	if IsNetworkClassFailure("dial tcp: lookup x: no such host") {
		t.Error("DNS 失败不应被判为可禁用")
	}
	if IsNetworkClassFailure("unexpected HTTP response status: 403") {
		t.Error("403 不应被判为可禁用")
	}
	if !IsNetworkClassFailure("dial tcp: connection refused") {
		t.Error("连接被拒应被判为可禁用")
	}
}

// TestMaskSecrets 覆盖脱敏边界：URL query、敏感键值、请求头；同时确认定位信息不被抹掉。
func TestMaskSecrets(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		mustContain []string
		mustNotHave []string
	}{
		{
			name:        "URL查询串整体脱敏",
			in:          `Post "https://host.example/v1/chat?key=SECRET123&alt=json": status 403`,
			mustContain: []string{"https://host.example/v1/chat", "<redacted>", "403"},
			mustNotHave: []string{"SECRET123", "alt=json"},
		},
		{
			name:        "Authorization头",
			in:          "Authorization: Bearer abcdef.token.value",
			mustContain: []string{"Authorization"},
			mustNotHave: []string{"abcdef.token.value"},
		},
		{
			name:        "Cookie头",
			in:          "Cookie: SID=xyz123; other=1",
			mustContain: []string{"Cookie"},
			mustNotHave: []string{"xyz123"},
		},
		{
			name:        "APIKey参数",
			in:          "x-goog-api-key=AIzaSyABCDEFG",
			mustContain: []string{"x-goog-api-key"},
			mustNotHave: []string{"AIzaSyABCDEFG"},
		},
		{
			name:        "保留域名与错误类型",
			in:          "lookup ct.cloudflare.byoip.top: (exchange4: NXDOMAIN)",
			mustContain: []string{"ct.cloudflare.byoip.top", "NXDOMAIN"},
		},
		{
			name:        "普通错误文本原样保留",
			in:          "dial tcp 1.2.3.4:443: connect: connection refused",
			mustContain: []string{"dial tcp 1.2.3.4:443", "connection refused"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskSecrets(tc.in)
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("MaskSecrets(%q) = %q，应包含 %q", tc.in, got, want)
				}
			}
			for _, bad := range tc.mustNotHave {
				if strings.Contains(got, bad) {
					t.Errorf("MaskSecrets(%q) = %q，不应包含敏感值 %q", tc.in, got, bad)
				}
			}
		})
	}
}

// TestMaskSecrets_Truncates 超长文本必须被截断，避免超长上游响应撑大故障表。
func TestMaskSecrets_Truncates(t *testing.T) {
	got := MaskSecrets(strings.Repeat("x", faultDetailMaxLen*2))
	if len(got) > faultDetailMaxLen+32 {
		t.Fatalf("超长文本未被截断: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "<truncated>") {
		t.Fatalf("截断标记缺失: %q", got[len(got)-32:])
	}
}

// TestFaultSignature_Aggregates 聚合签名必须把同因不同次（端口/请求 ID/状态码/耗时不同）
// 归一到同一签名，使故障表按"节点+类别+签名"聚合成一行计数。
func TestFaultSignature_Aggregates(t *testing.T) {
	a := ClassifyFailure("dial tcp 1.2.3.4:443: connect: connection refused")
	b := ClassifyFailure("dial tcp 5.6.7.8:8443: connect: connection refused")
	if a.Sig != b.Sig {
		t.Fatalf("同因故障签名应一致:\n a=%q\n b=%q", a.Sig, b.Sig)
	}
	if a.Sig == "" {
		t.Fatal("签名不应为空")
	}

	c := ClassifyFailure("dial tcp 1.2.3.4:443: i/o timeout")
	if c.Sig == a.Sig {
		t.Fatal("不同故障不应共用签名")
	}
}

// TestFaultSignature_NoSecret 签名由脱敏文本归一而来，绝不允许凭据进入聚合键。
func TestFaultSignature_NoSecret(t *testing.T) {
	info := ClassifyFailure(`Post "https://h/v1?key=SECRET123": status 403`)
	if strings.Contains(info.Sig, "SECRET123") {
		t.Fatalf("签名泄露凭据: %q", info.Sig)
	}
	if strings.Contains(info.Detail, "SECRET123") {
		t.Fatalf("详情泄露凭据: %q", info.Detail)
	}
}
