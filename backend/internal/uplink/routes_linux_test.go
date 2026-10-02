package uplink

import (
	"context"
	"encoding/binary"
	"net"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUplinkRouteLossRecovery(t *testing.T) {
	isolated(t)
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	run := func(args ...string) {
		t.Helper()
		if b, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", args, b, err)
		}
	}
	run("link", "add", "route-test", "type", "dummy")
	defer exec.Command("ip", "link", "del", "route-test").Run()
	run("link", "set", "route-test", "up")
	run("addr", "add", "192.0.2.2/24", "dev", "route-test")
	nic, err := net.InterfaceByName("route-test")
	if err != nil {
		t.Fatal(err)
	}
	oldDirectory := leaseDirectory
	leaseDirectory = t.TempDir()
	defer func() { leaseDirectory = oldDirectory }()
	network := NetworkInfo{ID: digest("routes"), Name: nic.Name, Index: nic.Index, State: "configured", Addresses: []Address{{Family: "inet", Local: "192.0.2.2", Prefix: 24, Scope: "global"}}, Gateways: []string{"192.0.2.1"}, DNS: []string{"192.0.2.1"}}
	before, _ := command(ctx, "ip", "route", "show", "table", "main")
	for _, loss := range []string{"rule4", "rule6", "default", "terminal6", "connected"} {
		t.Run(loss, func(t *testing.T) {
			lease, err := openNetwork(ctx, network)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			state, _, _, err := lease.routeState()
			if err != nil || !slices.Equal(state, lease.routeBaseline) {
				t.Fatal("unstable baseline", err)
			}
			watch, cancel := context.WithCancel(ctx)
			defer cancel()
			go lease.Watch(watch, cancel)
			switch loss {
			case "rule4", "rule6":
				run("-"+loss[4:], "rule", "del", "priority", "1", "fwmark", lease.table, "lookup", lease.table)
			case "default":
				run("route", "del", "default", "via", "192.0.2.1", "dev", nic.Name, "metric", "100", "table", lease.table)
			case "terminal6":
				run("-6", "route", "del", "unreachable", "default", "metric", "32767", "table", lease.table)
			case "connected":
				run("route", "del", "192.0.2.0/24", "dev", nic.Name, "table", lease.table)
			}
			select {
			case <-watch.Done():
				if ctx.Err() != nil {
					t.Fatal("parent expired")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("lost route did not cancel stale session")
			}
			lease.Close()
			replacement, err := openNetwork(ctx, network)
			if err != nil {
				t.Fatal("lease rebuild", err)
			}
			replacement.Close()
		})
	}
	after, _ := command(ctx, "ip", "route", "show", "table", "main")
	if string(before) != string(after) {
		t.Fatal("host main routes changed")
	}
}

func TestOwnedRouteKey(t *testing.T) {
	for _, kind := range []uint16{unix.RTM_NEWRULE, unix.RTM_NEWROUTE} {
		data := make([]byte, 20)
		data[0] = unix.AF_INET
		binary.NativeEndian.PutUint16(data[12:], 8)
		binary.NativeEndian.PutUint16(data[14:], unix.RTA_TABLE)
		binary.NativeEndian.PutUint32(data[16:], 0x52000001)
		msg := syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: kind}, Data: data}
		key, owned, err := ownedRouteKey(msg, 0x52000001)
		if err != nil || !owned || key == "" {
			t.Fatal(owned, err)
		}
		if _, owned, _ := ownedRouteKey(msg, 0x52000002); owned {
			t.Fatal("another lease matched")
		}
		msg.Data = data[:19]
		if _, _, err := ownedRouteKey(msg, 0x52000001); err == nil {
			t.Fatal("truncated attribute accepted")
		}
	}
}
