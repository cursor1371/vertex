package recaptcha

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
)

// resetVerCache 清空版本缓存全局态并恢复生产钩子，测试间互不污染。
func resetVerCache(t *testing.T) {
	t.Helper()
	versionMu.Lock()
	cachedVer = ""
	cachedVerSuspect = false
	cachedVerInvalidAt = time.Time{}
	versionMu.Unlock()
	oldTTL := versionNegativeCacheTTL
	t.Cleanup(func() {
		versionNegativeCacheTTL = oldTTL
		fetchVersionFn = fetchVersionFromSession
		versionMu.Lock()
		cachedVer = ""
		cachedVerSuspect = false
		cachedVerInvalidAt = time.Time{}
		versionMu.Unlock()
	})
}

// stubSess 仅作占位：注入的钩子不触达 session。
func stubSess() *transport.Session { return &transport.Session{} } //nolint:exhaustruct

// TestCurrentVersion_EmptyCacheNegativeCache 验证空缓存拉取失败后进入负缓存窗口：
// 窗口内重复调用不再穿透 enterprise.js（此前每次 attempt 都重拉一次）。
func TestCurrentVersion_EmptyCacheNegativeCache(t *testing.T) {
	resetVerCache(t)
	var calls atomic.Int32
	fetchVersionFn = func(ctx context.Context, sess *transport.Session) (string, bool) {
		calls.Add(1)
		return "", false // 模拟 enterprise.js 拉取失败
	}
	sess := stubSess()
	for i := 0; i < 5; i++ {
		if ver, ok := currentVersion(context.Background(), sess); ok || ver != "" {
			t.Fatalf("第 %d 次调用应失败回落, got %q ok=%v", i, ver, ok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("负缓存窗口内 5 次调用只应穿透 1 次, got %d", got)
	}
}

// TestCurrentVersion_CachedHitNoRefetchInWindow 验证核心放大修复：
// token 失败 invalidate 后，负缓存窗口内继续复用最后已知版本，不重拉脚本。
func TestCurrentVersion_CachedHitNoRefetchInWindow(t *testing.T) {
	resetVerCache(t)
	var calls atomic.Int32
	fetchVersionFn = func(ctx context.Context, sess *transport.Session) (string, bool) {
		n := calls.Add(1)
		return fmt.Sprintf("v%d", n), true
	}
	sess := stubSess()
	if ver, ok := currentVersion(context.Background(), sess); !ok || ver != "v1" {
		t.Fatalf("首次拉取应得 v1, got %q ok=%v", ver, ok)
	}
	invalidateVersion() // 模拟 token 失败：置疑似过期
	for i := 0; i < 5; i++ {
		if ver, ok := currentVersion(context.Background(), sess); !ok || ver != "v1" {
			t.Fatalf("窗口内应复用 v1, got %q ok=%v", ver, ok)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("窗口内不应重拉 enterprise.js, got %d 次", got)
	}
}

// TestCurrentVersion_SuspectRefetchAfterWindow 验证疑似过期在窗口到期后重取换新，
// 成功即清除 suspect，后续调用走缓存。
func TestCurrentVersion_SuspectRefetchAfterWindow(t *testing.T) {
	resetVerCache(t)
	versionNegativeCacheTTL = 0 // 窗口立即到期
	var calls atomic.Int32
	fetchVersionFn = func(ctx context.Context, sess *transport.Session) (string, bool) {
		n := calls.Add(1)
		return fmt.Sprintf("v%d", n), true
	}
	sess := stubSess()
	if ver, _ := currentVersion(context.Background(), sess); ver != "v1" {
		t.Fatalf("首次应得 v1, got %q", ver)
	}
	invalidateVersion()
	if ver, _ := currentVersion(context.Background(), sess); ver != "v2" {
		t.Fatalf("窗口到期应重取得 v2, got %q", ver)
	}
	if ver, _ := currentVersion(context.Background(), sess); ver != "v2" {
		t.Fatalf("重取后应走缓存, got %q", ver)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("共应 2 次拉取, got %d", got)
	}
}

// TestCurrentVersion_SingleflightFoldsConcurrentFetches 验证缓存为空的瞬间，
// 并发 goroutine（预取池 + 竞速候选）折叠为一次 enterprise.js 请求。
func TestCurrentVersion_SingleflightFoldsConcurrentFetches(t *testing.T) {
	resetVerCache(t)
	var calls atomic.Int32
	fetchVersionFn = func(ctx context.Context, sess *transport.Session) (string, bool) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // 拉长执行窗口，确保并发到达时仍在拉取中
		return "vfold", true
	}
	sess := stubSess()
	const n = 20
	var wg sync.WaitGroup
	versions := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ver, ok := currentVersion(context.Background(), sess); ok {
				versions[i] = ver
			}
		}(i)
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d 并发只应穿透 1 次拉取, got %d", n, got)
	}
	for i, ver := range versions {
		if ver != "vfold" {
			t.Fatalf("goroutine %d 未共享折叠结果: %q", i, ver)
		}
	}
}
