package uplink

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolated(t *testing.T) {
	t.Helper()
	if os.Getenv("RYKVO_UPLINK_NETNS") != "1" {
		t.Skip("isolated root network namespace required")
	}
	self, _ := os.Readlink("/proc/self/ns/net")
	init, _ := os.Readlink("/proc/1/ns/net")
	if self == "" || self == init || os.Geteuid() != 0 {
		t.Fatal("refusing host network mutation")
	}
}
func TestUplinkPeer(t *testing.T) {
	label := os.Getenv("RYKVO_UPLINK_PEER")
	if label == "" {
		t.Skip("namespace helper")
	}
	isolated(t)
	udp, e := net.ListenPacket("udp4", "0.0.0.0:500")
	if e != nil {
		t.Fatal(e)
	}
	defer udp.Close()
	dns, e := net.ListenPacket("udp4", "0.0.0.0:53")
	if e != nil {
		t.Fatal(e)
	}
	defer dns.Close()
	tcp, e := net.Listen("tcp4", "0.0.0.0:8088")
	if e != nil {
		t.Fatal(e)
	}
	defer tcp.Close()
	go func() {
		for {
			c, e := tcp.Accept()
			if e != nil {
				return
			}
			_, _ = io.WriteString(c, label)
			c.Close()
		}
	}()
	go func() {
		b := make([]byte, 1024)
		for {
			_, a, e := udp.ReadFrom(b)
			if e != nil {
				return
			}
			_, _ = udp.WriteTo([]byte(label), a)
		}
	}()
	go func() {
		b := make([]byte, 1024)
		for {
			n, a, e := dns.ReadFrom(b)
			if e != nil {
				return
			}
			if n < 17 {
				continue
			}
			end := 12
			for end < n && b[end] != 0 {
				end += int(b[end]) + 1
			}
			end += 5
			if end > n {
				continue
			}
			q := append([]byte{}, b[:end]...)
			q[2], q[3] = 0x81, 0x80
			q[6], q[7], q[10], q[11] = 0, 0, 0, 0
			if binary.BigEndian.Uint16(q[end-4:end-2]) == 1 {
				q[7] = 1
				v := byte(1)
				if label == "B" {
					v = 2
				}
				q = append(q, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 198, 51, 100, v)
			}
			_, _ = dns.WriteTo(q, a)
		}
	}()
	fmt.Println("PEER_READY")
	time.Sleep(50 * time.Second)
}
func TestUplinkOverlappingNetworks(t *testing.T) {
	isolated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		if b, e := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
	}
	run("ip", "link", "set", "lo", "up")
	previousDirectory := leaseDirectory
	leaseDirectory = t.TempDir()
	defer func() { leaseDirectory = previousDirectory }()
	var leases []*Lease
	for i, label := range []string{"A", "B"} {
		peer := exec.CommandContext(ctx, "unshare", "--net", "sleep", "50")
		if e := peer.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
		pid := fmt.Sprint(peer.Process.Pid)
		for attempt := 0; attempt < 100; attempt++ {
			a, _ := os.Readlink("/proc/self/ns/net")
			b, _ := os.Readlink("/proc/" + pid + "/ns/net")
			if a != b && b != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		name := fmt.Sprintf("up%d", i)
		other := fmt.Sprintf("peer%d", i)
		run("ip", "link", "add", name, "type", "veth", "peer", "name", other)
		// Remove links synchronously; namespace destruction can outlive killed peers.
		t.Cleanup(func() { _ = exec.Command("ip", "link", "del", name).Run() })
		run("ip", "link", "set", other, "netns", pid)
		run("ip", "address", "add", "192.168.7.2/24", "dev", name)
		run("ip", "link", "set", name, "up")
		if e := os.WriteFile(filepath.Join("/proc/sys/net/ipv4/conf", name, "rp_filter"), []byte("2"), 0600); e != nil {
			t.Fatal(e)
		}
		run("nsenter", "-t", pid, "-n", "ip", "address", "add", "192.168.7.1/24", "dev", other)
		run("nsenter", "-t", pid, "-n", "ip", "link", "set", other, "up")
		run("nsenter", "-t", pid, "-n", "ip", "link", "set", "lo", "up")
		if i == 0 {
			run("ip", "route", "add", "default", "via", "192.168.7.1", "dev", name)
		}
		server := exec.CommandContext(ctx, "nsenter", "-t", pid, "-n", os.Args[0], "-test.run", "^TestUplinkPeer$", "-test.v")
		server.Env = append(os.Environ(), "RYKVO_UPLINK_PEER="+label)
		pipe, e := server.StdoutPipe()
		if e != nil {
			t.Fatal(e)
		}
		server.Stderr = os.Stderr
		if e = server.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
		b := make([]byte, 128)
		ready := ""
		for !strings.Contains(ready, "PEER_READY") {
			n, e := pipe.Read(b)
			if e != nil {
				t.Fatal(e)
			}
			ready += string(b[:n])
		}
		nic, e := net.InterfaceByName(name)
		if e != nil {
			t.Fatal(e)
		}
		info := NetworkInfo{ID: digest(label), Name: name, Index: nic.Index, State: "configured", Addresses: []Address{{Family: "inet", Local: "192.168.7.2", Prefix: 24, Scope: "global"}}, Gateways: []string{"192.168.7.1"}, DNS: []string{"192.168.7.1"}}
		l, e := openNetwork(ctx, info)
		if e != nil {
			t.Fatal(e)
		}
		defer l.Close()
		leases = append(leases, l)
	}
	before, _ := command(ctx, "ip", "route", "show", "table", "main")
	for i, l := range leases {
		want := string(rune('A' + i))
		for _, network := range []string{"udp4", "tcp4"} {
			port := "500"
			if network == "tcp4" {
				port = "8088"
			}
			c, e := l.Dialer.DialContext(ctx, network, "192.168.7.1:"+port)
			if e != nil {
				t.Fatal(e)
			}
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			if network == "udp4" {
				_, _ = c.Write([]byte("probe"))
			}
			b := make([]byte, 10)
			n, e := c.Read(b)
			c.Close()
			if e != nil || string(b[:n]) != want {
				t.Fatalf("crossed uplink %s: %q %v", want, b[:n], e)
			}
		}
		ips, e := l.Resolver.LookupHost(ctx, "egress.test")
		if e != nil || len(ips) != 1 || ips[0] != fmt.Sprintf("198.51.100.%d", i+1) {
			t.Fatalf("DNS crossed uplink: %v %v", ips, e)
		}
	}
	run("ip", "link", "set", "up1", "down")
	if c, e := leases[1].Dialer.DialContext(ctx, "udp4", "192.168.7.1:500"); e == nil {
		c.Close()
		t.Fatal("down interface dial accepted")
	}
	run("ip", "link", "set", "up1", "up")
	for _, l := range leases {
		l.Close()
		l.Close()
		routes, _ := command(ctx, "ip", "-4", "route", "show", "table", l.table)
		if len(routes) > 0 {
			t.Fatal("route leak")
		}
	}
	after, _ := command(ctx, "ip", "route", "show", "table", "main")
	if string(before) != string(after) {
		t.Fatalf("host main routes changed: %s / %s", before, after)
	}
}
