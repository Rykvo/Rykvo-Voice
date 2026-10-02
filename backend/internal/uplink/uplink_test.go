package uplink

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIdentityAndDNS(t *testing.T) {
	ctx := WithNetwork(context.Background(), "0123456789abcdef0123456789abcdef")
	if !ValidID(Network(ctx)) || Network(context.Background()) != "" || ValidID("../eth0") {
		t.Fatal("identity validation")
	}
	dir := t.TempDir()
	device := filepath.Join(dir, "usb", "function")
	if err := os.MkdirAll(device, 0700); err != nil {
		t.Fatal(err)
	}
	for file, value := range map[string]string{"idVendor": "1234", "idProduct": "5678", "serial": "phone-A"} {
		if err := os.WriteFile(filepath.Join(dir, "usb", file), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(device, "bInterfaceNumber"), []byte("01"), 0600); err != nil {
		t.Fatal(err)
	}
	a := stableID(device, "00:11:22:33:44:55")
	if !ValidID(a) || stableID(device, "66:77:88:99:aa:bb") != a {
		t.Fatal("USB serial binding depends on randomized MAC")
	}
	if err := os.WriteFile(filepath.Join(dir, "usb", "serial"), []byte("phone-B"), 0600); err != nil {
		t.Fatal(err)
	}
	if a == stableID(device, "00:11:22:33:44:55") {
		t.Fatal("different phone reused binding")
	}
	if err := os.WriteFile(filepath.Join(dir, "usb", "idVendor"), []byte("2c7c"), 0600); err != nil {
		t.Fatal(err)
	}
	if stableID(device, "00:11:22:33:44:55") != "" {
		t.Fatal("modem bearer admitted")
	}
	want := []string{"192.168.7.1", "fe80::1"}
	if got := validDNS([]string{"127.0.0.53", "::1", "0.0.0.0", "192.168.7.1", "bad", "192.168.7.1", "fe80::1", "fe80::1%other"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("DNS: %v", got)
	}
}

func TestEarlyPolicyRulesFailClosed(t *testing.T) {
	for _, rules := range []string{"0: from all lookup local\n32766: from all lookup main", "0: from all lookup local\n1: from all fwmark 0x52000001 lookup 1375731713"} {
		if !compatibleRules(rules) {
			t.Fatalf("valid rules rejected: %s", rules)
		}
	}
	for _, rules := range []string{"0: from all lookup main", "1: from all lookup 100", "1: from all fwmark 0x52 lookup 100"} {
		if compatibleRules(rules) {
			t.Fatalf("conflicting rule allowed: %s", rules)
		}
	}
}

func TestUnreadyLinkStates(t *testing.T) {
	for _, test := range []struct {
		flags   []string
		address bool
		want    string
	}{
		{nil, false, "unconfigured"},
		{[]string{"UP"}, false, "no-carrier"},
		{[]string{"UP", "LOWER_UP"}, false, "address-pending"},
		{[]string{"UP", "LOWER_UP"}, true, "gateway-missing"},
	} {
		n := NetworkInfo{}
		if test.address {
			n.Addresses = []Address{{Local: "192.168.7.2"}}
		}
		if got := linkState(linkInfo{Flags: test.flags}, n); got != test.want {
			t.Fatalf("state=%s want=%s", got, test.want)
		}
	}
}

func TestHostDefaultLabels(t *testing.T) {
	routes := []routeInfo{
		{Family: "IPv4", Destination: "default", Device: "eth0", Metric: 100},
		{Family: "IPv4", Destination: "default", Device: "usb0", Metric: 600},
		{Family: "IPv6", Destination: "default", Device: "usb0", Metric: 50},
		{Family: "IPv4", Destination: "192.168.1.0/24", Device: "other", Metric: 1},
		{Family: "IPv4", Destination: "default", Device: "down", Flags: []string{"linkdown"}},
	}
	want := map[string][]string{"eth0": {"IPv4"}, "usb0": {"IPv6"}}
	if got := hostDefaults(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults=%v", got)
	}
	routes = append(routes, routeInfo{Family: "IPv4", Destination: "default", Metric: 100, NextHops: []routeHop{{Device: "eth1"}, {Device: "dead", Flags: []string{"dead"}}}})
	want["eth1"] = []string{"IPv4"}
	if got := hostDefaults(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("equal-cost=%v", got)
	}
	routes = append(routes, routeInfo{Family: "IPv4", Destination: "default", Device: "vpn0", Metric: 1})
	if got := hostDefaults(routes); len(got["eth0"]) != 0 || len(got["vpn0"]) != 1 {
		t.Fatalf("vpn default=%v", got)
	}
}
