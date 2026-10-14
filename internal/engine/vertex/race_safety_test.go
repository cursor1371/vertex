package vertex

import (
	"context"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// TestSafetyFailFastRecordsNodeWithoutWin 验证一个反直觉的记账结果：
// 安全拦截（safety）属于 FailFast 全局硬错误，竞速会终局归属触发该错误的节点，
// 但只记 error、不记 win；而 handler 侧 isSafetyBlock 命中会把请求判为 success=true。
// 于是该节点在「节点统计」里成功 +1，在「Racing 统计」里却没有胜出。
func TestSafetyFailFastRecordsNodeWithoutWin(t *testing.T) {
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	// stats.Init 幂等（只生效第一次），若同包其它用例已初始化过单例，
	// 直接 Init 会静默绑到别人的 store 上导致本用例读到空结果。
	// 先 Close 释放单例，确保本次能绑定到自己的 store。
	stats.Close()
	stats.Init(store)
	defer stats.Close()

	setupRaceNodes(t, "uri1", "uri2")
	defer testNodes.Reset()

	cfg := config.StaticProvider(raceTestConfig())
	ctx := context.WithValue(context.Background(), RequestIDKey{}, "safety-req")

	// 模拟流式路径（WithFailFastOnHardError）：首个候选返回 safety 全局硬错误
	run := func(ctx context.Context, uri string) (string, error) {
		if uri == "uri1" {
			return "", NewSafetyError("blocked by safety filter", "SAFETY", nil)
		}
		<-ctx.Done()
		return "", ctx.Err()
	}

	_, err = RunRace(ctx, testNodes, cfg, run, WithFailFastOnHardError[string]())
	if err == nil {
		t.Fatal("expected error from safety fail-fast")
	}

	stats.Close()
	byNode, err := store.RaceByNode(0, time.Now().Unix()+1)
	if err != nil {
		t.Fatalf("RaceByNode: %v", err)
	}
	// 归属到了节点（节点统计会有该节点的成功记录），但没有任何 win
	var attempts, wins, errs int
	for _, st := range byNode {
		attempts += st.Attempts
		wins += st.Wins
		errs += st.Errors
	}
	if wins != 0 {
		t.Errorf("safety 终局不应产生 win, got wins=%d", wins)
	}
	if errs == 0 {
		t.Error("safety 候选应被记为 error")
	}
	t.Logf("记账结果: attempts=%d wins=%d errors=%d（节点归属已写入，但无胜出）", attempts, wins, errs)
}
