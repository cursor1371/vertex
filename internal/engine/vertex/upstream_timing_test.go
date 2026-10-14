package vertex

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestUpstreamTiming_MarkFirstByteOnlyOnce 首字节只记录第一次：
// touchActivity 每次读到字节都会触发，无条件覆写会把它变成"最后一个字节"。
func TestUpstreamTiming_MarkFirstByteOnlyOnce(t *testing.T) {
	ut := NewUpstreamTiming()
	first := time.Now().UnixNano()
	ut.MarkFirstByte(first)
	ut.MarkFirstByte(first + int64(time.Second))
	ut.MarkFirstByte(first + int64(2*time.Second))

	if got := ut.FirstByteNano(); got != first {
		t.Fatalf("首字节被覆写: got %d, want %d", got, first)
	}
}

// TestUpstreamTiming_MarkFirstByteConcurrent 竞速下并发调用不得错乱（CAS 保证唯一胜出值）。
func TestUpstreamTiming_MarkFirstByteConcurrent(t *testing.T) {
	ut := NewUpstreamTiming()
	base := time.Now().UnixNano()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ut.MarkFirstByte(base + int64(i))
		}(i)
	}
	wg.Wait()

	got := ut.FirstByteNano()
	if got < base || got >= base+64 {
		t.Fatalf("并发记录值越界: %d (base=%d)", got, base)
	}
}

// TestUpstreamTiming_MarkWinnerOnlyFirst 胜出值只取第一个产出有效内容的候选：
// 竞速下多个候选都可能读到字节，若被后续候选覆写会给出不对应的候选耗时。
func TestUpstreamTiming_MarkWinnerOnlyFirst(t *testing.T) {
	reqTiming := NewUpstreamTiming()

	winner := NewUpstreamTiming()
	winner.MarkFirstByte(1000)
	reqTiming.MarkWinner(winner)

	later := NewUpstreamTiming()
	later.MarkFirstByte(2000)
	reqTiming.MarkWinner(later)

	if got := reqTiming.winnerNano(); got != 1000 {
		t.Fatalf("胜出值被后续候选覆写: got %d, want 1000", got)
	}

	// 未采集到首字节的候选不得写入胜出值。
	empty := NewUpstreamTiming()
	reqTiming.MarkWinner(empty)
	if got := reqTiming.winnerNano(); got != 1000 {
		t.Fatalf("空候选覆盖了胜出值: %d", got)
	}
}

func TestUpstreamTiming_UpstreamTTFB(t *testing.T) {
	start := time.Unix(0, 1000)
	reqTiming := NewUpstreamTiming()
	cand := NewUpstreamTiming()
	cand.MarkFirstByte(start.Add(150 * time.Millisecond).UnixNano())
	reqTiming.MarkWinner(cand)

	if got := reqTiming.UpstreamTTFB(start); got != 150*time.Millisecond {
		t.Fatalf("UpstreamTTFB = %v, want 150ms", got)
	}

	// 未采集到时返回 0，不得返回负值或零值以外的哨兵。
	if got := NewUpstreamTiming().UpstreamTTFB(start); got != 0 {
		t.Fatalf("未采集时应返回 0, got %v", got)
	}
	// 首字节早于 start（时钟异常）时钳到 0，不得返回负值。
	anomaly := NewUpstreamTiming()
	anomaly.MarkFirstByte(start.Add(-time.Second).UnixNano())
	req2 := NewUpstreamTiming()
	req2.MarkWinner(anomaly)
	if got := req2.UpstreamTTFB(start); got != 0 {
		t.Fatalf("负值应钳到 0, got %v", got)
	}
}

// TestUpstreamTiming_ContextRoundTrip 挂载/取回必须闭环；nil ctx 与未挂载均安全返回 nil。
func TestUpstreamTiming_ContextRoundTrip(t *testing.T) {
	if got := TimingFromContext(context.TODO()); got != nil {
		t.Fatalf("nil ctx 应返回 nil, got %v", got)
	}
	if got := TimingFromContext(context.Background()); got != nil {
		t.Fatalf("未挂载应返回 nil, got %v", got)
	}

	ut := NewUpstreamTiming()
	ctx := WithUpstreamTiming(context.Background(), ut)
	if got := TimingFromContext(ctx); got != ut {
		t.Fatal("挂载后应能取回同一实例")
	}
	// 同 key 覆盖（竞速闭包用它把请求级替换为候选级）。
	cand := NewUpstreamTiming()
	ctx2 := WithUpstreamTiming(ctx, cand)
	if got := TimingFromContext(ctx2); got != cand {
		t.Fatal("覆盖后应取回新实例")
	}
}

// TestUpstreamTiming_NilSafe 各方法对 nil 接收者安全：api 层在时序未挂载时不得 panic。
func TestUpstreamTiming_NilSafe(t *testing.T) {
	var ut *UpstreamTiming
	ut.MarkFirstByte(time.Now().UnixNano())
	ut.MarkWinner(NewUpstreamTiming())
	if got := ut.FirstByteNano(); got != 0 {
		t.Errorf("FirstByteNano = %d", got)
	}
	if got := ut.UpstreamTTFB(time.Now()); got != 0 {
		t.Errorf("UpstreamTTFB = %v", got)
	}
	if got := WithUpstreamTiming(context.Background(), nil); got == nil {
		t.Error("挂 nil 应返回原 ctx 而非 nil")
	}
}

// TestUpstreamTiming_OnWinnerFiresOnce 验证"上游首字节就绪"回调：
// 仅在请求级胜出值首次落定时触发一次；候选未采到首字节时不触发。
// 该回调是"首字不必等整包收完即可显示"的关键——非流式/聚合流下若只在收尾推送，
// 首字会一直为 "-" 直到数据传输结束。
func TestUpstreamTiming_OnWinnerFiresOnce(t *testing.T) {
	req := NewUpstreamTiming()
	var mu sync.Mutex
	fires := 0
	req.SetOnWinner(func() {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	// 候选未采到首字节：不应触发
	empty := NewUpstreamTiming()
	req.MarkWinner(empty)
	if got := fires; got != 0 {
		t.Fatalf("候选无首字节时不应触发回调, 实际 %d", got)
	}

	// 首个胜出候选：触发一次
	c1 := NewUpstreamTiming()
	c1.MarkFirstByte(time.Now().UnixNano())
	req.MarkWinner(c1)
	if got := fires; got != 1 {
		t.Fatalf("首次胜出应触发回调, 实际 %d", got)
	}

	// 后续候选（竞速并发）：不应重复触发
	c2 := NewUpstreamTiming()
	c2.MarkFirstByte(time.Now().UnixNano())
	req.MarkWinner(c2)
	if got := fires; got != 1 {
		t.Fatalf("回调应只触发一次, 实际 %d", got)
	}
	if req.UpstreamTTFB(time.Now().Add(-time.Second)) <= 0 {
		t.Error("胜出首字节应已写入请求级时序")
	}
}
