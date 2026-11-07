package services

import (
	"context"
	"fmt"
	"net/netip"
	"syscall"
)

var (
	v4Deny = mustPrefixes(
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	)
	// Special-purpose blocks inside 2000::/3; everything outside it is rejected separately.
	v6Deny = mustPrefixes(
		"2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32", "3fff::/20", "5f00::/16",
	)
	v6GlobalUnicast = netip.MustParsePrefix("2000::/3")
	nat64WellKnown  = netip.MustParsePrefix("64:ff9b::/96")
	nat64LocalUse   = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour       = netip.MustParsePrefix("2002::/16")
)

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(values))
	for i, value := range values {
		prefixes[i] = netip.MustParsePrefix(value)
	}
	return prefixes
}

func inAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// PublicAddr is not a complete SSRF guarantee: a DNS64 translator using a
// network-specific prefix can map a global IPv6 address onto a private IPv4 host.
func PublicAddr(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return !inAny(addr, v4Deny)
	}
	bytes := addr.As16()
	switch {
	// The embedded-IPv4 layout of local-use NAT64 depends on an operator-chosen prefix length.
	case nat64LocalUse.Contains(addr):
		return false
	case nat64WellKnown.Contains(addr):
		return PublicAddr(netip.AddrFrom4([4]byte(bytes[12:16])))
	case sixToFour.Contains(addr):
		return PublicAddr(netip.AddrFrom4([4]byte(bytes[2:6])))
	// IANA allocates global unicast only from 2000::/3, so anything else fails closed.
	case !v6GlobalUnicast.Contains(addr):
		return false
	}
	return !inAny(addr, v6Deny)
}

// policyControl checks the literal address of every socket right before connect,
// which covers DNS rebinding, happy-eyeballs fallbacks and redirect hops.
func policyControl(policy func(netip.Addr) bool) func(context.Context, string, string, syscall.RawConn) error {
	return func(_ context.Context, network, address string, _ syscall.RawConn) error {
		if network != "tcp4" && network != "tcp6" {
			return fmt.Errorf("%w: network %s", ErrImportSSRF, network)
		}
		addrPort, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrImportSSRF, err)
		}
		if addrPort.Addr().Zone() != "" || !policy(addrPort.Addr()) {
			return ErrImportSSRF
		}
		return nil
	}
}
