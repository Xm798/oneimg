package services

import (
	"errors"
	"net/netip"
	"testing"
)

func TestPublicAddrDeniesSpecialPurpose(t *testing.T) {
	for _, raw := range []string{
		"0.1.2.3", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1",
		"192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.255",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::127.0.0.1", "::", "::1", "fe80::1%eth0",
		"fe80::1", "fc00::1", "fec0::1", "ff02::1", "100::1",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::808:808",
		"2002:7f00:1::1", "2002:a9fe:a9fe::1", "2002:c612:1::1",
		"2001:0:4136:e378:8000:63bf:80ff:fffe", "2001:2::1", "2001:10::1", "2001:20::1",
		"2001:db8::1", "3fff::1", "5f00::1",
	} {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatal(raw, err)
		}
		if PublicAddr(addr) {
			t.Errorf("PublicAddr(%s) = true, want false", raw)
		}
	}
	if PublicAddr(netip.Addr{}) {
		t.Error("PublicAddr(invalid) = true")
	}
}

func TestPublicAddrAllowsGlobal(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808", "2002:808:808::1", "::ffff:8.8.8.8"} {
		if !PublicAddr(netip.MustParseAddr(raw)) {
			t.Errorf("PublicAddr(%s) = false, want true", raw)
		}
	}
}

func TestPolicyControl(t *testing.T) {
	control := policyControl(PublicAddr)
	for _, test := range []struct {
		network, address string
		allowed          bool
	}{
		{"tcp4", "10.0.0.1:80", false},
		{"tcp6", "[fe80::1%eth0]:80", false},
		{"udp4", "8.8.8.8:53", false},
		{"tcp4", "8.8.8.8:443", true},
		{"tcp6", "[2606:4700:4700::1111]:443", true},
	} {
		err := control(t.Context(), test.network, test.address, nil)
		if test.allowed && err != nil {
			t.Errorf("%s %s: %v", test.network, test.address, err)
		}
		if !test.allowed && !errors.Is(err, ErrImportSSRF) {
			t.Errorf("%s %s: error = %v, want ErrImportSSRF", test.network, test.address, err)
		}
	}
}
