package api

import (
	"net/http"

	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// adminStopRequest 强制停止一条在途请求（请求监控页每行的"强制停止"）。
// body: {"req_id":"..."}；返回 {"ok":true,"stopped":bool}——stopped=false 表示该请求
// 已结束或未登记取消函数（前端据此提示"请求已结束"）。
//
// 停止语义：取消该请求的派生上下文 → 上游流/竞速候选当场中断（见 vertex 的取消链路）。
// 不留痕：不写额外日志与持久化状态，在途表下一轮轮询即不再出现该行。
func (adm *AdminHandler) adminStopRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ReqID string `json:"req_id"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.ReqID == "" {
		writeJSON(w, http.StatusBadRequest, adminErr("缺少 req_id (missing req_id)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": stats.CancelReq(body.ReqID)})
}

// substituteAdminStop 把"被管理员强制停止"的错误替换为可区分的明确错误（503）。
//
// 强制停止取消的是派生上下文，管线只会返回一个笼统的 context 取消错误；这里换成语义
// 清晰的文案，让客户端能区分"被管理员停掉"与"上游/网络故障"。非管理员停止时原样返回。
func substituteAdminStop(reqID string, ve *vertex.VertexError) *vertex.VertexError {
	if ve == nil || !stats.ReqStoppedByAdmin(reqID) {
		return ve
	}
	return vertex.NewUnavailableError("请求已被管理员强制停止 (request stopped by admin)", ve)
}
