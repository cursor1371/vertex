package transport

import (
	"net/netip"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/infra/config"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

// vlessOutboundOpts 从 outbound 取出 *option.VLESSOutboundOptions（本测试统一用 vless）。
func vlessOutboundOpts(t *testing.T, ob option.Outbound) *option.VLESSOutboundOptions {
	t.Helper()
	o, ok := ob.Options.(*option.VLESSOutboundOptions)
	if !ok {
		t.Fatalf("Options type = %T, want *option.VLESSOutboundOptions", ob.Options)
	}
	return o
}

func TestApplyIPV6Policy_ForceNodeIPv6Domain(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{ForceNodeIPv6: true}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if !applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected applyIPV6Policy to report change")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.DomainResolver == nil {
		t.Fatal("expected DomainResolver to be set")
	}
	if opts.DomainResolver.Strategy != option.DomainStrategy(C.DomainStrategyIPv6Only) {
		t.Fatalf("Strategy = %v, want ipv6_only", opts.DomainResolver.Strategy)
	}
}

func TestApplyIPV6Policy_ForceNodeIPv6Off(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected no change when disabled")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.DomainResolver != nil {
		t.Fatalf("expected no DomainResolver, got %+v", opts.DomainResolver)
	}
}

func TestApplyIPV6Policy_ForceNodeIPv6IPNode(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@1.2.3.4:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{ForceNodeIPv6: true}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@1.2.3.4:443")

	if applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected no change for IP node")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.DomainResolver != nil {
		t.Fatalf("expected no DomainResolver for IP node, got %+v", opts.DomainResolver)
	}
}

func TestApplyIPV6Policy_HostBindInterface(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{HostIPv6Interface: "eth0"}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if !applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected change")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.BindInterface != "eth0" {
		t.Fatalf("BindInterface = %q, want eth0", opts.BindInterface)
	}
	if opts.Inet6BindAddress != nil {
		t.Fatalf("Inet6BindAddress should be nil, got %v", opts.Inet6BindAddress)
	}
}

func TestApplyIPV6Policy_HostBindAddress(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{HostIPv6Address: "2001:db8::1"}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if !applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected change")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.Inet6BindAddress == nil {
		t.Fatal("expected Inet6BindAddress to be set")
	}
	if got := netip.Addr(*opts.Inet6BindAddress); got != netip.MustParseAddr("2001:db8::1") {
		t.Fatalf("Inet6BindAddress = %s, want 2001:db8::1", got)
	}
	if opts.BindInterface != "" {
		t.Fatalf("BindInterface should be empty when address set, got %q", opts.BindInterface)
	}
}

func TestApplyIPV6Policy_AddressPrecedence(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{
		HostIPv6Interface: "eth0",
		HostIPv6Address:   "2001:db8::2",
	}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if !applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected change")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.Inet6BindAddress == nil || netip.Addr(*opts.Inet6BindAddress) != netip.MustParseAddr("2001:db8::2") {
		t.Fatalf("Inet6BindAddress = %v, want 2001:db8::2 (address precedence)", opts.Inet6BindAddress)
	}
	if opts.BindInterface != "" {
		t.Fatalf("BindInterface should be empty when address present, got %q", opts.BindInterface)
	}
}

func TestApplyIPV6Policy_InvalidAddress(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{HostIPv6Address: "not-an-ip"}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected no change for invalid address")
	}
	opts := vlessOutboundOpts(t, ob)
	if opts.Inet6BindAddress != nil {
		t.Fatalf("Inet6BindAddress should be nil for invalid address, got %v", opts.Inet6BindAddress)
	}
}

func TestApplyIPV6Policy_AllOff(t *testing.T) {
	ob, err := buildOutbound("vless://12345678-1234-1234-1234-123456789012@example.com:443")
	if err != nil {
		t.Fatalf("buildOutbound: %v", err)
	}
	cfg := config.StaticProvider(config.AppConfig{}) //nolint:exhaustruct
	n, _ := ParseURI("vless://12345678-1234-1234-1234-123456789012@example.com:443")

	if applyIPV6Policy(&ob, n, cfg) {
		t.Fatal("expected no change when all off")
	}
}
