package uplink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Opt-in, data traffic only. Never invokes a modem, SMS, MMS or SIP operation.
func TestRuntimeLiveVPN(t *testing.T) {
	id := os.Getenv("RYKVO_VERIFY_VPN_ID")
	if id == "" {
		t.Skip("explicit test network required")
	}
	if os.Getenv("RYKVO_VERIFY_WORKER") != "1" {
		leaseDirectory = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	selected, err := Find(ctx, id)
	if err != nil {
		t.Fatal("network discovery", err)
	}
	t.Log("selected transport", selected.Name, selected.State)
	l, err := Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go l.Watch(ctx, cancel)
	if l.network.Name[:4] != "rvpn" {
		t.Fatal("module lease bypassed VPN")
	}
	if ips, err := l.Resolver.LookupHost(ctx, "example.com"); err != nil || len(ips) == 0 {
		t.Fatal("VPN DNS", err)
	}
	transport := &http.Transport{DialContext: l.Dialer.DialContext, ForceAttemptHTTP2: false}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=json", nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal("VPN HTTPS", err)
	}
	defer response.Body.Close()
	var result struct {
		IP string `json:"ip"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 512)).Decode(&result) != nil || result.IP != os.Getenv("RYKVO_VERIFY_EXIT") {
		t.Fatalf("unexpected VPN exit: %s", result.IP)
	}
	t.Log("module-bound HTTPS exit", result.IP, "DNS passed")
	if os.Getenv("RYKVO_VERIFY_UDP") == "1" {
		udp, err := l.Dialer.DialContext(ctx, "udp", "time.cloudflare.com:123")
		if err != nil {
			t.Fatal("VPN UDP dial", err)
		}
		defer udp.Close()
		udp.SetDeadline(time.Now().Add(8 * time.Second))
		packet := make([]byte, 48)
		packet[0] = 0x23
		if _, err = udp.Write(packet); err != nil {
			t.Fatal("VPN UDP write", err)
		}
		received := make([]byte, 512)
		if n, err := udp.Read(received); err != nil || n < 48 {
			t.Fatal("VPN UDP response", n, err)
		}
		t.Log("module-bound UDP NTP response passed")
	}
	select {
	case <-ctx.Done():
		t.Fatal("VPN lease lost", ctx.Err())
	case <-time.After(10 * time.Second):
		t.Log("VPN lease stable across three network checks")
	}
}
