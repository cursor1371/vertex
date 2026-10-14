package api

import (
	"runtime"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/spool"
)

// memStatsTTL 是 ReadMemStats 采样的缓存窗口。ReadMemStats 会触发全局 STW，
// 管理面板/外部监控轮询本接口时不能每次都真实采样；goroutines 与 spilled_mb
// 是廉价原子读取，保持实时。
const memStatsTTL = 5 * time.Second

var (
	memStatsMu     sync.Mutex
	memStatsCached runtime.MemStats
	memStatsAt     time.Time
)

// memStatsSnapshot 返回 TTL 内缓存的 MemStats；过期时持锁重采样。
// 双检在锁内完成，避免缓存刚过期时并发请求各自触发一次 STW。
func memStatsSnapshot() runtime.MemStats {
	memStatsMu.Lock()
	defer memStatsMu.Unlock()
	if memStatsAt.IsZero() || time.Since(memStatsAt) >= memStatsTTL {
		runtime.ReadMemStats(&memStatsCached)
		memStatsAt = time.Now()
	}
	return memStatsCached
}

func metricsBody() map[string]any {
	m := memStatsSnapshot()

	return map[string]any{
		"status": "ok",
		"memory": map[string]any{
			"alloc_mb":   bToMb(m.Alloc),
			"heap_inuse": bToMb(m.HeapInuse),
			"num_gc":     m.NumGC,
			"goroutines": runtime.NumGoroutine(),
			"spilled_mb": bToMb(uint64(spool.SpilledBytes())),
		},
	}
}

func bToMb(b uint64) float64 {
	return float64(b) / 1024 / 1024
}
