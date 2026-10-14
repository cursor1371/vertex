package stats

import (
	"context"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// initTestStats 绑定内存仓储（Init 幂等，重复调用沿用首次绑定）。
func initTestStats(t *testing.T) {
	t.Helper()
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	Init(store)
}

// TestCancelReqStopsInflight 强制停止：登记取消函数后 CancelReq 取消上下文并标记"被管理员停止"；
// StopReq 清理条目后两者都不再命中（取消控制块与在途表同生命周期，不会状态不同步）。
func TestCancelReqStopsInflight(t *testing.T) {
	initTestStats(t)

	const reqID = "req-cancel-1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartReq(reqID, "model-a", "alice", "k", false)
	SetReqCancel(reqID, cancel)

	if ReqStoppedByAdmin(reqID) {
		t.Fatal("未停止前不应标记为被管理员停止")
	}
	if !CancelReq(reqID) {
		t.Fatal("CancelReq 命中已登记请求应返回 true")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("CancelReq 应取消该请求的上下文")
	}
	if !ReqStoppedByAdmin(reqID) {
		t.Fatal("停止后应标记为被管理员停止")
	}

	StopReq(reqID)
	if CancelReq(reqID) {
		t.Fatal("请求结束后 CancelReq 应返回 false")
	}
	if ReqStoppedByAdmin(reqID) {
		t.Fatal("请求结束后不应再命中标记")
	}
}

// TestCancelReqUnknownSafe 未登记/空 ID 的停止请求安全返回 false，不得 panic。
func TestCancelReqUnknownSafe(t *testing.T) {
	initTestStats(t)

	if CancelReq("") {
		t.Fatal("空 ID 应返回 false")
	}
	if CancelReq("not-exist") {
		t.Fatal("未登记请求应返回 false")
	}
	if ReqStoppedByAdmin("not-exist") {
		t.Fatal("未登记请求不应标记")
	}
	SetReqCancel("not-exist", func() {})
	SetReqCancel("", nil) // 不应 panic
}

// TestSetReqCancelRequiresRegisteredReq 未 StartReq 就登记取消函数时不生效：
// 避免控制块脱离在途表存活（那会让 StopReq 清不掉它）。
func TestSetReqCancelRequiresRegisteredReq(t *testing.T) {
	initTestStats(t)

	cancelled := false
	SetReqCancel("req-未登记", func() { cancelled = true })
	if CancelReq("req-未登记") {
		t.Fatal("未登记在途表的请求不应可停止")
	}
	if cancelled {
		t.Fatal("取消函数不应被调用")
	}
}
