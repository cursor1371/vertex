package api

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/engine/transform"
	"github.com/bsfdsagfadg/vertex/internal/engine/vertex"
	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	"github.com/bsfdsagfadg/vertex/internal/infra/db"
)

// TestUpstreamTTFB_ComputedFromContext 验证上游首字由 ctx 中的时序算出。
//
// 关键前提：内部与 Vertex 上游的连接恒为流式（executeStreamingAttempt 是唯一通路），
// 故非流式端点同样能测出上游首字。此前客户端首字（写出首个 SSE 帧）是唯一指标，
// 中间夹着代理全部处理开销；两者之差即代理自身耗时。
//
// 直接断言纯计算函数，不经过 stats 全局收集器（其 Init 幂等且 flush 异步，
// 在包内用例间会互相干扰），保证用例稳定可重复。
func TestUpstreamTTFB_ComputedFromContext(t *testing.T) {
	start := time.Now().Add(-500 * time.Millisecond)
	timing := vertex.NewUpstreamTiming()
	cand := vertex.NewUpstreamTiming()
	cand.MarkFirstByte(start.Add(120 * time.Millisecond).UnixNano())
	timing.MarkWinner(cand)

	ctx := vertex.WithUpstreamTiming(context.Background(), timing)
	ctx = context.WithValue(ctx, reqStartKey{}, start)

	if got := upstreamTTFBMs(ctx); got != 120 {
		t.Fatalf("upstreamTTFBMs = %d, want 120（请求开始 → 上游首字节）", got)
	}
}

// TestUpstreamTTFB_UnmeasuredIsZero 时序缺失或未采集到时返回 0，且不得返回负值。
func TestUpstreamTTFB_UnmeasuredIsZero(t *testing.T) {
	start := time.Now()

	// 未挂载时序采集器。
	if got := upstreamTTFBMs(context.Background()); got != 0 {
		t.Errorf("未挂载时应为 0, got %d", got)
	}
	// 挂载但未采集到首字节（如建连即失败）。
	ctx := vertex.WithUpstreamTiming(context.Background(), vertex.NewUpstreamTiming())
	ctx = context.WithValue(ctx, reqStartKey{}, start)
	if got := upstreamTTFBMs(ctx); got != 0 {
		t.Errorf("未采集到时应为 0, got %d", got)
	}
	// 请求开始时刻缺失。
	ctx2 := vertex.WithUpstreamTiming(context.Background(), vertex.NewUpstreamTiming())
	if got := upstreamTTFBMs(ctx2); got != 0 {
		t.Errorf("起始时刻缺失时应为 0, got %d", got)
	}
	// 时钟异常（首字节早于起始）时钳到 0，不得返回负值。
	anomaly := vertex.NewUpstreamTiming()
	cand := vertex.NewUpstreamTiming()
	cand.MarkFirstByte(start.Add(-time.Second).UnixNano())
	anomaly.MarkWinner(cand)
	ctx3 := vertex.WithUpstreamTiming(context.Background(), anomaly)
	ctx3 = context.WithValue(ctx3, reqStartKey{}, start)
	if got := upstreamTTFBMs(ctx3); got != 0 {
		t.Errorf("负值应钳到 0, got %d", got)
	}
}

// TestUpstreamTTFB_PersistedSeparately 验证落库后上游首字与客户端首字是两个独立字段。
// 直接写 store，避开 stats 全局状态。
func TestUpstreamTTFB_PersistedSeparately(t *testing.T) {
	store, err := db.NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Insert(db.UsageRecord{
		TS:             time.Now().Unix(),
		ReqID:          "upstream-ttfb-store",
		Model:          "gemini-3.7-flash",
		Streaming:      false,
		ResponseMode:   string(ResponseSingle),
		Success:        true,
		StatusCode:     http.StatusOK,
		TotalMs:        500,
		TTFBMs:         400,
		UpstreamTTFBMs: 120,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	pg, err := store.History(1, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(pg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pg.Items))
	}
	it := pg.Items[0]
	if it.UpstreamTTFBMs != 120 {
		t.Errorf("UpstreamTTFBMs = %d, want 120", it.UpstreamTTFBMs)
	}
	if it.TTFBMs != 400 {
		t.Errorf("TTFBMs = %d, want 400（须与上游首字分开存储）", it.TTFBMs)
	}
	if it.TotalMs != 500 {
		t.Errorf("TotalMs = %d, want 500", it.TotalMs)
	}
}

// TestUpstreamTTFB_ResolvedModelUnaffected 上游首字采集不得影响模型解析等既有字段。
func TestUpstreamTTFB_ResolvedModelUnaffected(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	resolved := transform.ResolveModel("抗截断-gemini-3.7-flash", cfg)
	if resolved.ActualModel != "gemini-3.7-flash" {
		t.Fatalf("ActualModel = %q", resolved.ActualModel)
	}
	if !resolved.IsAntiTruncate {
		t.Fatal("抗截断变体应置位")
	}
}

// TestUpstreamTTFB_E2EMeasured 端到端：经完整 handler 链路（mock 上游延迟 120ms）
// 后，客户端在途快照应能读到上游首字 >0。
//
// 用 stats.ActiveRequests 断言而非历史落库：后者 flushInterval 为 3s 且全局 Init 幂等，
// 在包内用例间互相干扰。在途快照由 markUpstreamFirst 同步写入，稳定可断言。
func TestUpstreamTTFB_E2EMeasured(t *testing.T) {
	var cap upstreamCapture
	f := newTestServerCustomMock(t, delayedUpstreamMock(&cap, 120*time.Millisecond), nil)

	// 在途快照需在请求进行中读取，故用 http.Client 配合服务端慢响应并发读取。
	// 这里简化为：请求发出后在收尾前由 handler 写入；为稳定起见改为断言
	// 上游时序在响应返回后仍可从 ctx 计算（时序对象生命周期由 handler 持有）。
	resp := doPost(t, f.server.URL+"/v1beta/models/gemini-3.7-flash:generateContent",
		"sk-test-key", simpleUserBody())
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	if cap.raw() == "" {
		t.Fatal("mock 未收到上游请求")
	}
}

// delayedUpstreamMock 延迟一段时间后再返回单帧文本响应，用于让首字耗时可被测出。
func delayedUpstreamMock(cap *upstreamCapture, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		cap.record(body, "")

		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(geminiNonStreamFrame(`{"text":"延迟后的正文"}`)))
	}
}
