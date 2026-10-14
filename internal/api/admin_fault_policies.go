package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/node/nodestore"
)

// 故障处理策略接口（管理面板「处理策略」页签）。
//
// 设计依据：Issue #5「策略可配置化：实施计划基准」§5 —— 至少能查看与调整
// 故障类别 / 当前处理方式 / 自动探测失败次数阈值（0 = 无限）/ 当前生效值与默认值。
//
// 接口只做校验与落库，动作语义全在 nodestore.PolicySet 内解析（单一真源），
// 避免"接口一套规则、池内另一套规则"的漂移。

// policyView 是单个类别的策略视图（生效值 + 默认值，供面板对比展示）。
type policyView struct {
	Class              string `json:"class"`
	Action             string `json:"action"`
	ActionLabel        string `json:"action_label"`
	DefaultAction      string `json:"default_action"`
	ProbeFailThreshold int    `json:"probe_fail_threshold"`
	// FollowsGlobal 表示该阈值来自全局配置 health_probe_fail_threshold，而非类别自定义。
	FollowsGlobal bool `json:"follows_global"`
	// Custom 表示该类别有用户自定义行（false = 完全按内置默认运行）。
	Custom bool `json:"custom"`
	// Note 是语义提示（如"前置池第一版不执行自动探测"），面板直接展示，避免用户误解。
	Note string `json:"note,omitempty"`
}

type faultPoliciesResponse struct {
	Policies []policyView `json:"policies"`
	// GlobalProbeFailThreshold 是全局默认阈值（面板展示"跟随全局"的具体取值）。
	GlobalProbeFailThreshold int `json:"global_probe_fail_threshold"`
	// EntryPoolProbes 明确告诉前端：前置池当前不执行自动探测。
	EntryPoolProbes bool `json:"entry_pool_probes"`
}

// 动作语义提示：把"单次禁用"与"禁用后恢复探测"的区别写死在服务端，
// 保证面板、文档、日志三处口径一致。
var policyActionNotes = map[nodestore.PolicyAction]string{
	nodestore.PolicyRecordOnly: "记录故障，节点保持可用，不会移出节点池。",
	nodestore.PolicySingleShot: "发生一次即移出可用池，不会被自动恢复，需要手动启用。",
	nodestore.PolicyDisableThenProbe: "发生一次即移出可用池，随后由自动探测判断：探测成功自动恢复；" +
		"连续失败达阈值转为永久禁用（阈值 0 表示无限探测）。",
}

func (adm *AdminHandler) adminGetFaultPolicies(w http.ResponseWriter, _ *http.Request) {
	resp := faultPoliciesResponse{
		Policies:        []policyView{},
		EntryPoolProbes: false, // 前置池第一版不执行自动探测（UI 需据此提示）
	}
	if adm.deps.Exit == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	set := adm.deps.Exit.PolicySet()
	custom := set.Customizations()

	for _, pol := range set.All() {
		_, isCustom := custom[pol.Class]
		v := policyView{
			Class:              string(pol.Class),
			Action:             string(pol.Action),
			ActionLabel:        nodestore.PolicyActionLabel(pol.Action),
			DefaultAction:      string(nodestore.BuiltinPolicyAction(pol.Class)),
			ProbeFailThreshold: pol.ProbeFailThreshold,
			FollowsGlobal:      pol.ThresholdFollowsGlobal,
			Custom:             isCustom,
			Note:               policyActionNotes[pol.Action],
		}
		resp.Policies = append(resp.Policies, v)
	}
	// 全局默认阈值直接取解析器口径（不能取某类别的生效值——它可能已被自定义覆盖）。
	resp.GlobalProbeFailThreshold = set.GlobalThreshold()
	writeJSON(w, http.StatusOK, resp)
}

type policyUpdateRequest struct {
	Policies []struct {
		Class string `json:"class"`
		// Action 为空表示"只改阈值、不动动作"。
		Action string `json:"action"`
		// ProbeFailThreshold 为空表示"跟随全局"；显式 0 表示无限探测。
		ProbeFailThreshold *int `json:"probe_fail_threshold"`
	} `json:"policies"`
}

func (adm *AdminHandler) adminPutFaultPolicies(w http.ResponseWriter, r *http.Request) {
	if adm.deps.Exit == nil {
		writeJSON(w, http.StatusServiceUnavailable, adminErr("节点池未装配"))
		return
	}
	var body policyUpdateRequest
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if len(body.Policies) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("policies 不能为空"))
		return
	}

	type validated struct {
		row nodestore.PolicyRow
	}
	rows := make([]validated, 0, len(body.Policies))
	now := time.Now().Unix()
	for _, p := range body.Policies {
		class := nodestore.FailureClass(p.Class)
		// 只接受已知类别：未知类别写进去也不会被任何分类命中，属无效配置，
		// 直接拒绝比静默接受更利于排查（未来新增类别会先进入 KnownFailureClasses）。
		if !isKnownFailureClass(class) {
			writeJSON(w, http.StatusBadRequest, adminErr("未知的故障类别: "+p.Class))
			return
		}
		action := nodestore.BuiltinPolicyAction(class)
		if p.Action != "" {
			a, ok := nodestore.PolicyActionFromString(p.Action)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr("非法的处理方式: "+p.Action+"（可选 record_only / single_shot / disable_then_probe）"))
				return
			}
			action = a
		}
		threshold := nodestore.ProbeThresholdFollowGlobal
		if p.ProbeFailThreshold != nil {
			threshold = *p.ProbeFailThreshold
			if threshold < 0 || threshold > 10 {
				writeJSON(w, http.StatusBadRequest, adminErr("探测失败阈值必须在 0~10（0 = 无限探测）"))
				return
			}
		}
		rows = append(rows, validated{row: nodestore.PolicyRow{
			Class: class, Action: action, ProbeFailThreshold: threshold, UpdatedAt: now,
		}})
	}

	for _, v := range rows {
		if err := adm.deps.Exit.UpsertPolicy(v.row); err != nil {
			log.Printf("[Admin] [FaultPolicies] 写入失败 %s: %v", v.row.Class, err)
			writeJSON(w, http.StatusInternalServerError, adminErr("写入策略失败: "+err.Error()))
			return
		}
		log.Printf("[Admin] [FaultPolicies] %s → %s（阈值 %d）", v.row.Class,
			nodestore.PolicyActionLabel(v.row.Action), v.row.ProbeFailThreshold)
	}
	adm.adminGetFaultPolicies(w, r)
}

// adminDeleteFaultPolicies 还原策略：
//   - 带 ?class=xxx → 只删除该类别的自定义行（回落内置默认）；
//   - 不带参数 → 重置全部自定义行。
//
// 单类别"还原默认"必须是**真正的删行**，不能走 PUT 把内置默认动作写回成一条自定义行——
// 那样数据库里会留下一条 permanent 的自定义记录，页面刷新后又显示"自定义"，
// 与"稀疏覆盖、删行即回落默认"的设计矛盾。
func (adm *AdminHandler) adminDeleteFaultPolicies(w http.ResponseWriter, r *http.Request) {
	if adm.deps.Exit == nil {
		writeJSON(w, http.StatusServiceUnavailable, adminErr("节点池未装配"))
		return
	}
	if class := strings.TrimSpace(r.URL.Query().Get("class")); class != "" {
		c := nodestore.FailureClass(class)
		if !isKnownFailureClass(c) {
			writeJSON(w, http.StatusBadRequest, adminErr("未知的故障类别: "+class))
			return
		}
		if err := adm.deps.Exit.DeletePolicy(c); err != nil {
			log.Printf("[Admin] [FaultPolicies] 还原 %s 失败: %v", class, err)
			writeJSON(w, http.StatusInternalServerError, adminErr("还原策略失败: "+err.Error()))
			return
		}
		log.Printf("[Admin] [FaultPolicies] 已还原 %s 为内置默认（删除自定义行）", class)
		return
	}
	n, err := adm.deps.Exit.ResetPolicies()
	if err != nil {
		log.Printf("[Admin] [FaultPolicies] 重置失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, adminErr("重置策略失败: "+err.Error()))
		return
	}
	log.Printf("[Admin] [FaultPolicies] 已重置 %d 条自定义策略", n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reset": n})
}

// isKnownFailureClass 报告类别是否属于内置分类（PUT 白名单）。
func isKnownFailureClass(c nodestore.FailureClass) bool {
	for _, k := range nodestore.KnownFailureClasses() {
		if k == c {
			return true
		}
	}
	return false
}
