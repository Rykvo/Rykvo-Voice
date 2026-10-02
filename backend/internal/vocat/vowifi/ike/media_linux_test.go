//go:build linux

package ike

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestMediaRoutesValidateBearerAndUDPSelectors(t *testing.T) {
	c := userspaceRouteTestConfig()
	local := &net.UDPAddr{IP: c.InnerLocalIPv4, Port: 32000}
	remote := &net.UDPAddr{IP: net.ParseIP("10.128.1.2"), Port: 40000}
	if !validMediaEndpoints(c, local, remote) {
		t.Fatal("negotiated media rejected")
	}
	for _, addr := range []string{"127.0.0.1", "169.254.1.1", "224.1.1.1", "0.0.0.0", "203.0.113.1", "2001:db8::2"} {
		if validMediaEndpoints(c, local, &net.UDPAddr{IP: net.ParseIP(addr), Port: 40000}) {
			t.Fatal("unsafe endpoint accepted", addr)
		}
	}
	for _, port := range []int{-1, 0, 65536} {
		if validMediaEndpoints(c, local, &net.UDPAddr{IP: remote.IP, Port: port}) {
			t.Fatal("invalid port accepted")
		}
	}
	if validMediaEndpoints(c, &net.UDPAddr{IP: net.ParseIP("192.168.8.130"), Port: 32000}, remote) {
		t.Fatal("host source accepted")
	}
	c.ResponderSelectors[0].IPProtocol = 6
	if validMediaEndpoints(c, local, remote) {
		t.Fatal("TCP-only selector allowed RTP")
	}
	c.ResponderSelectors[0].IPProtocol = 17
	c.ResponderSelectors[0].EndPort = 35000
	if validMediaEndpoints(c, local, remote) {
		t.Fatal("RTP port escaped selector")
	}
}

func TestLinuxMediaRoutes(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("isolated network namespace required")
	}
	ip := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("ip", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("ip %v: %v %s", args, e, b)
		}
		return string(b)
	}
	name := "vocat-media-t"
	ip("link", "add", name, "type", "dummy")
	t.Cleanup(func() { exec.Command("ip", "link", "delete", name).Run() })
	c := userspaceRouteTestConfig()
	c.Name = name
	c.InboundSPI = 0x02030405
	c.InnerLocalIPv6 = net.ParseIP("2001:db8:1::2")
	v6proxy := net.ParseIP("2001:db8:2::5")
	c.PCSCF = append(c.PCSCF, v6proxy)
	c.InitiatorSelectors = append(c.InitiatorSelectors, trafficSelector{StartPort: 0, EndPort: 65535, StartIP: c.InnerLocalIPv6, EndIP: c.InnerLocalIPv6})
	c.ResponderSelectors = append(c.ResponderSelectors, trafficSelector{StartPort: 0, EndPort: 65535, StartIP: net.ParseIP("2001:db8:2::"), EndIP: net.ParseIP("2001:db8:2::ffff")})
	ip("addr", "add", c.InnerLocalIPv4.String()+"/32", "dev", name)
	ip("-6", "addr", "add", c.InnerLocalIPv6.String()+"/128", "dev", name, "nodad")
	ip("link", "set", name, "up")
	before4, before6 := ip("-4", "route", "show", "table", "main"), ip("-6", "route", "show", "table", "main")
	h := &linuxUserspaceHandle{config: c, ipCommand: "ip", runContext: context.Background()}
	table, priority := userspaceRoutingIdentifiers(c.InboundSPI)
	for _, tc := range []struct {
		family               string
		local, proxy, remote net.IP
		bits                 int
	}{
		{"-4", c.InnerLocalIPv4, c.PCSCF[0], net.ParseIP("10.128.1.2"), 32},
		{"-6", c.InnerLocalIPv6, v6proxy, net.ParseIP("2001:db8:2::20"), 128},
	} {
		if e := h.configureFamily(context.Background(), tc.family, tc.local, []net.IP{tc.proxy}, tc.bits, table, priority); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { h.cleanupNetwork(context.Background()) })
		get := func(dest net.IP) error {
			return exec.Command("ip", tc.family, "route", "get", dest.String(), "from", tc.local.String()).Run()
		}
		if get(tc.remote) == nil {
			t.Fatal("unnegotiated media escaped")
		}
		local := &net.UDPAddr{IP: tc.local, Port: 32000}
		remote := &net.UDPAddr{IP: tc.remote, Port: 40000}
		a, e := h.OpenMediaRoute(context.Background(), local, remote)
		if e != nil {
			t.Fatal(e)
		}
		b, e := h.OpenMediaRoute(context.Background(), local, remote)
		if e != nil {
			t.Fatal(e)
		}
		out := ip(tc.family, "route", "get", tc.remote.String(), "from", tc.local.String())
		if !strings.Contains(out, "dev "+name) || !strings.Contains(out, "table "+strconv.FormatUint(uint64(table), 10)) {
			t.Fatal("media escaped private table", out)
		}
		if e = a.Close(); e != nil {
			t.Fatal(e)
		}
		if get(tc.remote) != nil {
			t.Fatal("shared route removed")
		}
		if e = b.Close(); e != nil {
			t.Fatal(e)
		}
		if get(tc.remote) == nil {
			t.Fatal("media route survived hangup")
		}
		b.Close()
		proxy, e := h.OpenMediaRoute(context.Background(), local, &net.UDPAddr{IP: tc.proxy, Port: 40000})
		if e != nil {
			t.Fatal(e)
		}
		if e = proxy.Close(); e != nil {
			t.Fatal(e)
		}
		if get(tc.proxy) != nil {
			t.Fatal("removed registration route")
		}
		_, e = h.OpenMediaRoute(context.Background(), local, remote)
		if e != nil {
			t.Fatal(e)
		}
		// A tunnel ending also removes a still-held call lease.
	}
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	if _, e := h.OpenMediaRoute(context.Background(), &net.UDPAddr{IP: c.InnerLocalIPv4, Port: 32000}, &net.UDPAddr{IP: net.ParseIP("10.128.1.3"), Port: 40000}); e == nil {
		t.Fatal("closed tunnel allocated route")
	}
	if e := h.cleanupNetwork(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(h.mediaRoutes) != 0 {
		t.Fatal("media route ownership leaked")
	}
	if ip("-4", "route", "show", "table", "main") != before4 || ip("-6", "route", "show", "table", "main") != before6 {
		t.Fatal("main routes changed")
	}
}
