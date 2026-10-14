package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/stats"
)

// TestGenerate_TTFBVisibleWhileInFlight 回归"首字要等数据传输结束才显示"。
//
// 用【非流式】端点：它要等上游流收完才写响应，因此 handler 内"写首帧后推送"的兜底路径
// 在途期间根本不会触发；只有"上游首字节就绪"回调才能让首字在数据传输过程中可见。
// 这正是用户看到的现象（请求回应列为「非流式→…」时首字一直为 -，直到数据传输结束）。
//
// mock 先 sleep 再回首帧并保持连接不结束，断言此刻在途快照中的上游首字已就绪。
// sleep 使首字远大于 1ms，避开 Milliseconds() 亚毫秒截断成 0 的干扰。
func TestGenerate_TTFBVisibleWhileInFlight(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()

	contentFrame := `{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED"}]}}}}]}`
	mock := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond) // 让上游首字可测（>1ms）
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(contentFrame))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-release // 保持请求在途，供测试在"数据传输"中读取在途快照
	}

	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	stats.Close() // 先释放单例，确保绑定到本用例的 store
	stats.Init(store)
	defer stats.Close()

	fx := newTestServerCustomMock(t, mock, nil)

	go func() {
		body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		req, _ := http.NewRequest(http.MethodPost,
			fx.server.URL+"/v1beta/models/gemini-2.5-flash:generateContent?key=sk-test-key",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
	}()

	deadline := time.Now().Add(3 * time.Second)
	var gotTTFB int64
	var lastSnap string
	for time.Now().Before(deadline) {
		for _, a := range stats.ActiveRequests() {
			lastSnap = fmt.Sprintf("state=%s upstreamTTFB=%d", a.Status, a.UpstreamTTFBMs)
			if a.UpstreamTTFBMs > gotTTFB {
				gotTTFB = a.UpstreamTTFBMs
			}
		}
		if gotTTFB > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if gotTTFB <= 0 {
		t.Fatalf("在途期间上游首字未就绪（%s）：首字会一直显示 - 直到数据传输结束", lastSnap)
	}
}
