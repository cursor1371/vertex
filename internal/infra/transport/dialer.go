package transport

import (
	"context"
	"net"
)

type ProxyDialer interface {
	CreateDialer(uri string, reqID string) (func(ctx context.Context, network, addr string) (net.Conn, error), func(), error)
	StopAll()
	// GetNextEntrySocksAddr 按请求轮询返回前置代理池中一个 SOCKS5 回环地址；池为空返回 ""。
	GetNextEntrySocksAddr() string
	// Stopped 报告前置代理池是否已被 StopAll 关闭。
	// 与"池为空"区分：空池降级直连是既有降级语义；关闭中（优雅停机）继续放行直连
	// 会让在途请求绕过前置代理出网，调用方应显式拒绝。
	Stopped() bool
	// SyncEntryPool 从 entrynodes 加载可选前置节点并增量同步轮询池。
	SyncEntryPool() error
	TestEntryProxy(uri string) (func(ctx context.Context, network, addr string) (net.Conn, error), func(), error)
}
