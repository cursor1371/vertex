package stats

import (
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/infra/db"
	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
)

const (
	httpLogBuffer   = 8192
	httpLogBatch    = 100
	httpLogInterval = 3 * time.Second
	// httpLogBatchBytes 是单批落库的累计 body 字节上限：达到即强制 flush，
	// 防止大 body 记录在一次事务中长时间占用共享的单一 SQLite 连接（SetMaxOpenConns(1)），
	// 从而拖慢同库的 usage_log 写入与 admin 查询。
	httpLogBatchBytes = 8 << 20 // 8 MiB
)

var (
	httpLogStore     *db.HTTPLogStore
	httpLogCh        chan db.HTTPLogRecord
	httpLogStop      chan struct{}
	httpLogStopped   chan struct{}
	httpLogRun       atomic.Bool
	httpLogEnabled   atomic.Bool
	httpLogRetention atomic.Int64
	// httpLogBodyCapBytes 是单条请求/响应体捕获上限（字节），0 表示不截断。
	httpLogBodyCapBytes atomic.Int64
	nodeNameOf          func(uri string) string
)

// httpLogRecorder 适配 transport.HTTPLogRecorder，把记录桥接到本收集器。
type httpLogRecorder struct{}

// HTTPLogRecorder 返回一个 transport.HTTPLogRecorder 适配器（注入给 transport 层）。
func HTTPLogRecorder() transport.HTTPLogRecorder {
	return httpLogRecorder{}
}

// Enabled 返回全局开关是否开启（transport 层据此决定是否捕获）。
func (httpLogRecorder) Enabled() bool {
	return httpLogEnabled.Load()
}

// BodyCapBytes 返回请求/响应体捕获上限。
func (httpLogRecorder) BodyCapBytes() int {
	n := httpLogBodyCapBytes.Load()
	return int(n)
}

// SetHTTPLogBodyCapBytes 设置单条 body 捕获上限（字节），0 表示不截断。
func SetHTTPLogBodyCapBytes(capBytes int) {
	if capBytes < 0 {
		return
	}
	httpLogBodyCapBytes.Store(int64(capBytes))
}

// Record 实现 transport.HTTPLogRecorder：转换并异步入队。
func (httpLogRecorder) Record(r transport.HTTPLogRecord) {
	RecordHTTPLog(r)
}

// RecordHTTPLog 转换 transport 记录并交给 http_log 收集器异步落库。
func RecordHTTPLog(r transport.HTTPLogRecord) {
	if !httpLogEnabled.Load() || !httpLogRun.Load() {
		return
	}
	if r.NodeName == "" && nodeNameOf != nil {
		r.NodeName = nodeNameOf(r.NodeURI)
	}
	rec := db.HTTPLogRecord{
		TS:                   r.TS,
		RequestID:            r.RequestID,
		Scope:                r.Scope,
		NodeURI:              r.NodeURI,
		NodeName:             r.NodeName,
		PeerIP:               r.PeerIP,
		TCPReused:            r.TCPReused,
		HTTPVersion:          r.HTTPVersion,
		FinalURL:             r.FinalURL,
		RequestHeaders:       r.RequestHeaders,
		RequestBody:          r.RequestBody,
		ResponseHeaders:      r.ResponseHeaders,
		ResponseBody:         r.ResponseBody,
		StatusCode:           r.StatusCode,
		Success:              r.Success,
		Error:                r.Error,
		ResponseCode:         r.ResponseCode,
		ResponseResult:       r.ResponseResult,
		ResponseErrorType:    r.ResponseErrorType,
		ResponseErrorMessage: r.ResponseErrorMessage,
		ElapsedMs:            r.ElapsedMs,
	}
	select {
	case httpLogCh <- rec:
	default:
	}
}

// InitHTTPLog 启动 http_log 收集器（幂等）。store 为 nil 时退化为空实现。
func InitHTTPLog(store *db.HTTPLogStore) {
	if httpLogRun.Load() {
		return
	}
	httpLogStore = store
	httpLogCh = make(chan db.HTTPLogRecord, httpLogBuffer)
	httpLogStop = make(chan struct{})
	httpLogStopped = make(chan struct{})
	httpLogRetention.Store(30)
	httpLogRun.Store(true)
	go httpLogFlushLoop()
}

// SetHTTPLogEnabled 设置全局记录开关。
func SetHTTPLogEnabled(enabled bool) {
	httpLogEnabled.Store(enabled)
}

// HTTPLogEnabled 返回当前开关状态。
func HTTPLogEnabled() bool {
	return httpLogEnabled.Load()
}

// SetHTTPLogRetentionDays 设置 http_log 保留天数（<1 时忽略）。
func SetHTTPLogRetentionDays(days int) {
	if days < 1 {
		return
	}
	httpLogRetention.Store(int64(days))
}

// HTTPLogRetentionDays 返回当前保留天数。
func HTTPLogRetentionDays() int {
	d := httpLogRetention.Load()
	if d < 1 {
		return 30
	}
	return int(d)
}

// SetHTTPLogNodeNamer 注入 node_uri -> 节点名解析函数。
func SetHTTPLogNodeNamer(fn func(uri string) string) {
	nodeNameOf = fn
}

// CloseHTTPLog 停止收集器并排空剩余缓冲。幂等：多次调用安全。
func CloseHTTPLog() {
	if !httpLogRun.CompareAndSwap(true, false) {
		return
	}
	close(httpLogStop)
	<-httpLogStopped
}

// httpLogFlushLoop 周期性批量落库 + 保留窗口清理。
func httpLogFlushLoop() {
	buf := make([]db.HTTPLogRecord, 0, httpLogBatch)
	accBytes := 0
	flush := func() {
		if len(buf) == 0 {
			return
		}
		batch := buf
		buf = make([]db.HTTPLogRecord, 0, httpLogBatch)
		accBytes = 0
		if httpLogStore != nil {
			_ = httpLogStore.InsertBatch(batch)
		}
	}
	recBytes := func(rec db.HTTPLogRecord) int {
		return len(rec.FinalURL) + len(rec.RequestHeaders) + len(rec.RequestBody) +
			len(rec.ResponseHeaders) + len(rec.ResponseBody)
	}
	retentionTicker := time.NewTicker(retentionInterval)
	defer retentionTicker.Stop()
	ticker := time.NewTicker(httpLogInterval)
	defer ticker.Stop()
	for {
		select {
		case rec := <-httpLogCh:
			buf = append(buf, rec)
			accBytes += recBytes(rec)
			if len(buf) >= httpLogBatch || accBytes >= httpLogBatchBytes {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-retentionTicker.C:
			if httpLogStore != nil {
				cutoff := time.Now().Unix() - int64(HTTPLogRetentionDays())*86400
				_, _ = httpLogStore.ResetOlderThan(cutoff)
			}
		case <-httpLogStop:
			for {
				select {
				case rec := <-httpLogCh:
					buf = append(buf, rec)
					accBytes += recBytes(rec)
				default:
					flush()
					close(httpLogStopped)
					return
				}
			}
		}
	}
}
