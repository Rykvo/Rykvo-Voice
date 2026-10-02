//go:build linux

package ike

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRoutingCleanupRequiresKernelProof(t *testing.T) {
	for _, tc := range []struct {
		name, routes, rules string
		wantOK              bool
	}{
		{"absent", `[]`, `[]`, true},
		{"foreign", `[{"table":254}]`, `[{"priority":32766,"table":254}]`, true},
		{"owned-route", `[{"table":16909060}]`, `[]`, false},
		{"owned-rule", `[]`, `[{"table":16909060}]`, false},
		{"owned-priority", `[]`, `[{"priority":19060,"table":254}]`, false},
		{"invalid-json", `broken`, `[]`, false},
		{"null", `null`, `[]`, false},
		{"null-entry", `[null]`, `[]`, false},
		{"invalid-rules", `[]`, `broken`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ip")
			script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n*'route show'*) printf '%%s\\n' '%s';;\n*'rule show'*) printf '%%s\\n' '%s';;\n*) exit 1;;\nesac\n", tc.routes, tc.rules)
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			h := &linuxUserspaceHandle{ipCommand: path, config: ChildSAConfig{InboundSPI: 0x01020304}}
			if err := h.confirmRoutingRemoved(context.Background()); (err == nil) != tc.wantOK {
				t.Fatal(tc.name, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := h.confirmRoutingRemoved(ctx); err == nil {
				t.Fatal("cancelled query accepted")
			}
		})
	}
}

func TestLinuxUserspaceCleanupAfterExternalRemoval(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("isolated network namespace required")
	}
	ip := func(args ...string) string {
		t.Helper()
		body, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, body)
		}
		return string(body)
	}
	main4, main6 := ip("-4", "route", "show", "table", "main"), ip("-6", "route", "show", "table", "main")
	for _, missing := range []string{"ipv4", "ipv6", "both", "denied"} {
		t.Run(missing, func(t *testing.T) {
			c := userspaceRouteTestConfig()
			c.Name, c.InboundSPI, c.OutboundSPI = "vocat-clean-t", 0x01020304, 0x05060708
			c.InnerLocalIPv6 = net.ParseIP("2001:db8:1::2")
			c.PCSCF = append(c.PCSCF, net.ParseIP("2001:db8:2::5"))
			c.InitiatorSelectors = append(c.InitiatorSelectors, trafficSelector{StartPort: 0, EndPort: 65535, StartIP: c.InnerLocalIPv6, EndIP: c.InnerLocalIPv6})
			c.ResponderSelectors = append(c.ResponderSelectors, trafficSelector{StartPort: 0, EndPort: 65535, StartIP: net.ParseIP("2001:db8:2::"), EndIP: net.ParseIP("2001:db8:2::ffff")})
			c.Encryption, c.Integrity = "aes-cbc-128", "hmac-sha1-96"
			c.InboundEncKey, c.OutboundEncKey = make([]byte, 16), make([]byte, 16)
			c.InboundAuthKey, c.OutboundAuthKey = make([]byte, 20), make([]byte, 20)
			c.UDPEncapsulation, c.Relay = true, blockingNATTRelay{}
			installed, err := (linuxUserspaceInstaller{ipCommand: "ip"}).Install(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			h := installed.(*linuxUserspaceHandle)
			t.Cleanup(func() { h.ipCommand = "ip"; h.Close(context.Background()); h.cleanupNetwork(context.Background()) })
			if missing == "denied" {
				realIP, err := exec.LookPath("ip")
				if err != nil {
					t.Fatal(err)
				}
				wrapper := filepath.Join(t.TempDir(), "ip")
				script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *' delete '*) echo 'Operation not permitted' >&2; exit 1;; esac\nexec %s \"$@\"\n", strconv.Quote(realIP))
				if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				h.ipCommand = wrapper
				if err := h.Close(context.Background()); err == nil {
					t.Fatal("real cleanup failure ignored")
				}
				if err := h.Close(context.Background()); err == nil {
					t.Fatal("repeat close hid failure")
				}
				h.ipCommand = "ip"
				if err := h.cleanupNetwork(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := h.OpenMediaRoute(context.Background(), &net.UDPAddr{IP: c.InnerLocalIPv4, Port: 32000}, &net.UDPAddr{IP: net.ParseIP("10.128.1.2"), Port: 40000})
				if err != nil {
					t.Fatal(err)
				}
				for i := len(h.cleanup) - 1; i >= 0; i-- {
					args := h.cleanup[i].arguments
					if missing == "both" || missing == "ipv4" && args[0] == "-4" || missing == "ipv6" && args[0] == "-6" {
						ip(args...)
					}
				}
				if missing == "both" {
					for _, route := range h.mediaRoutes {
						ip(route.remove...)
					}
				}
				if err := h.Close(context.Background()); err != nil {
					t.Fatal("already removed resources blocked recovery", err)
				}
				if err := h.confirmRoutingRemoved(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := exec.Command("ip", "link", "show", "dev", c.Name).Run(); err == nil {
					t.Fatal("TUN survived cleanup")
				}
				if len(h.mediaRoutes) != 0 {
					t.Fatal("media ownership retained after proof")
				}
			}
			// A new session can use the same routing slot without weakening admission.
			next, err := (linuxUserspaceInstaller{ipCommand: "ip"}).Install(context.Background(), c)
			if err != nil {
				t.Fatal("fresh session blocked", err)
			}
			if err := next.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	if ip("-4", "route", "show", "table", "main") != main4 || ip("-6", "route", "show", "table", "main") != main6 {
		t.Fatal("main routing changed")
	}
}
