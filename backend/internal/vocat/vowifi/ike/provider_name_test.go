package ike

import (
	"net"
	"testing"
)

func TestTunnelNameIsStableShortAndUsesCompleteDeviceID(t *testing.T) {
	t.Parallel()
	first := tunnelName("quectel-0125-1-6")
	if first != tunnelName("QUECTEL-0125-1-6") {
		t.Fatalf("name is not stable across normalized device IDs: %q", first)
	}
	if len(first) != 15 {
		t.Fatalf("interface name length = %d, want 15", len(first))
	}
	if first == tunnelName("quectel-0125-1-7") {
		t.Fatalf("distinct devices sharing a long prefix received %q", first)
	}
}

func TestPCSCFFilterKeepsOnlyAssignedAddressFamilies(t *testing.T) {
	t.Parallel()
	values := []net.IP{
		net.ParseIP("2001:db8::20"),
		net.IPv4(10, 127, 192, 82),
	}
	ipv4 := pcscfForAssignedFamilies(values, true, false)
	if len(ipv4) != 1 || ipv4[0].String() != "10.127.192.82" {
		t.Fatalf("IPv4 P-CSCF list = %v", ipv4)
	}
	dual := pcscfForAssignedFamilies(values, true, true)
	if len(dual) != 2 {
		t.Fatalf("dual-stack P-CSCF list = %v", dual)
	}
	duplicate := pcscfForAssignedFamilies(
		append(values, net.IPv4(10, 127, 192, 82)),
		true,
		true,
	)
	if len(duplicate) != 2 {
		t.Fatalf("duplicate P-CSCF was not removed: %v", duplicate)
	}
}
