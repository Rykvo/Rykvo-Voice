//go:build linux

package ike

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestDataNetworkWithoutPCSCFPreservesSelectors(t *testing.T) {
	c := userspaceRouteTestConfig()
	c.PCSCF = nil
	c.DataNetwork = true
	if err := validateUserspaceRoutes(c); err != nil {
		t.Fatal(err)
	}
	c.InnerLocalIPv4 = net.ParseIP("192.0.2.1")
	if validateUserspaceRoutes(c) == nil {
		t.Fatal("unnegotiated source accepted")
	}
}
func TestLinuxDataBearerRoutesAndCleanup(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("isolated network namespace required")
	}
	c := userspaceRouteTestConfig()
	c.DataNetwork = true
	c.PCSCF = nil
	c.Name = "vocat-data-test"
	c.InboundSPI = 0x09020304
	c.OutboundSPI = 0x09060708
	c.Encryption = "aes-cbc-128"
	c.Integrity = "hmac-sha1-96"
	c.InboundEncKey = make([]byte, 16)
	c.OutboundEncKey = make([]byte, 16)
	c.InboundAuthKey = make([]byte, 20)
	c.OutboundAuthKey = make([]byte, 20)
	c.UDPEncapsulation = true
	c.Relay = blockingNATTRelay{}
	before, err := exec.Command("ip", "route", "show", "table", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	h, err := (linuxUserspaceInstaller{ipCommand: "ip"}).Install(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(context.Background())
	if output, err := exec.Command("ip", "route", "get", "10.200.186.4", "from", c.InnerLocalIPv4.String()).CombinedOutput(); err != nil || !strings.Contains(string(output), "dev "+c.Name) {
		t.Fatal(string(output), err)
	}
	after, err := exec.Command("ip", "route", "show", "table", "main").Output()
	if err != nil || string(before) != string(after) {
		t.Fatal("host routes changed", err)
	}
	if err = h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exec.Command("ip", "link", "show", "dev", c.Name).Run() == nil {
		t.Fatal("data TUN leaked")
	}
	rules, _ := exec.Command("ip", "rule").Output()
	if strings.Contains(string(rules), c.InnerLocalIPv4.String()) {
		t.Fatal("source rule leaked")
	}
}
