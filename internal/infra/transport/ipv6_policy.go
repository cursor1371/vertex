package transport

import (
	"log"
	"net"
	"net/netip"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
)

// applyIPV6Policy 按配置对 outbound 注入 IPv6 拨号策略：
//   - force_node_ipv6：节点 server 为域名时，设置 outbound 的 DomainResolver.Strategy 为
//     ipv6_only（强制解析只取 AAAA、走 IPv6 拨号）。server 为 IP 字面量时域策略不适用，
//     记录日志并跳过该项。
//   - 宿主机绑定：host_ipv6_address 优先（仅设 Inet6BindAddress），否则 host_ipv6_interface
//     （仅设 BindInterface）。地址解析失败时跳过并记录日志，避免 panic。
//
// 返回是否实际对 outbound 做出改动（便于调用方调试日志）。
func applyIPV6Policy(o *option.Outbound, n *ParsedNode, cfg config.ConfigProvider) bool {
	if o == nil || cfg == nil {
		return false
	}
	s, ok := o.Options.(dialerOptionsSetter)
	if !ok {
		return false
	}
	opts := s.TakeDialerOptions()
	changed := false

	if cfg.ForceNodeIPv6() {
		if n != nil && net.ParseIP(n.Server) == nil {
			opts.DomainResolver = &option.DomainResolveOptions{
				Strategy: option.DomainStrategy(C.DomainStrategyIPv6Only),
			}
			changed = true
		} else if n != nil {
			log.Printf("[Transport] force_node_ipv6 对 IP 节点无效 (server=%q)，跳过", n.Server)
		}
	}

	if addr := cfg.HostIPv6Address(); addr != "" {
		parsed, err := netip.ParseAddr(addr)
		if err != nil {
			log.Printf("[Transport] host_ipv6_address 非法 (%q)，跳过绑定: %v", addr, err)
		} else {
			bindAddr := badoption.Addr(parsed)
			opts.Inet6BindAddress = &bindAddr
			opts.BindInterface = ""
			changed = true
		}
	} else if iface := cfg.HostIPv6Interface(); iface != "" {
		opts.BindInterface = iface
		opts.Inet6BindAddress = nil
		changed = true
	}

	if changed {
		s.ReplaceDialerOptions(opts)
	}
	return changed
}
