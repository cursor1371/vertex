package nodestore

// 本文件是两个节点池共享的健康判定策略（纯函数，无持久化语义）。
// exitpool 与 entrypool 的 RecordTest 曾各自内联同一段禁用判定，已经发生过口径漂移，
// 集中到共享内核避免再次分叉。
// 失败文本的归因在 failure_class.go（同一内核的另一半）。

// IsNetworkClassFailure 判定错误文本是否属于**节点级连通性故障**（FailureNodeNetwork）。
//
// 语义已收窄为纯分类：只回答"这是不是连接级/不可达类故障"，**不再回答"要不要禁用"**。
// 动作由策略表决定（见 failure_policy.go）——同一类别既可被配成禁止禁用，也可配成
// 单次禁用或禁用后恢复探测，把动作硬编码进分类器会与策略化冲突。
//
// DNS 解析失败（no such host / NXDOMAIN）不计入本判定：它只记录、不移出可用池。
func IsNetworkClassFailure(errStr string) bool {
	return ClassifyFailure(errStr).Class == FailureNodeNetwork
}
