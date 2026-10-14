package vertex

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// TestRunRace_OnlyWinnerCountsAsSuccess 验证竞速记账口径：
// 一次批量竞速只有胜出者记 win，窗口内其余候选一律记 lost（不成功）。
func TestRunRace_OnlyWinnerCountsAsSuccess(t *testing.T) {
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	// stats.Init 幂等（仅首次生效）：若同包其它用例已初始化过单例，直接 Init 会静默
	// 绑到别人的 store 上。先 Close 释放，确保本次绑定到自己的 store。
	stats.Close()
	stats.Init(store)
	// defer 兜底：用例中途失败也不把单例留在 started=true 状态污染后续用例。
	defer stats.Close()

	setupRaceNodes(t, "uri1", "uri2", "uri3")
	defer testNodes.Reset()

	cfg := config.StaticProvider(raceTestConfigSize(3))
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), RequestIDKey{}, "race-accounting"), 5*time.Second)
	defer cancel()

	// 首个返回者胜出；其余候选阻塞等待被 cancelOthers 抢先取消（不交付结果）。
	var order int32
	run := func(ctx context.Context, uri string) (string, error) {
		if atomic.AddInt32(&order, 1) == 1 {
			return "winner", nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	}

	if _, err := RunRace(ctx, testNodes, cfg, run); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 先排空异步落库缓冲，再读库断言（defer 里的那次 Close 此时已是幂等 no-op）。
	stats.Close()
	byNode, err := store.RaceByNode(0, time.Now().Unix()+1)
	if err != nil {
		t.Fatalf("RaceByNode: %v", err)
	}
	var wins, lost, attempts int
	for _, st := range byNode {
		wins += st.Wins
		lost += st.Lost
		attempts += st.Attempts
	}
	if wins != 1 {
		t.Errorf("应恰好 1 个胜出, got wins=%d attempts=%d byNode=%+v", wins, attempts, byNode)
	}
	if attempts != 3 {
		t.Errorf("窗口内 3 个候选都应记账, got attempts=%d byNode=%+v", attempts, byNode)
	}
	if lost != 2 {
		t.Errorf("其余 2 个候选应记为 lost, got lost=%d byNode=%+v", lost, byNode)
	}
}
