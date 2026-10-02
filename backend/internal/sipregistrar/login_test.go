package sipregistrar

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func loginRequest(t *testing.T, r *Registrar, device, session, source string, seq, expires int, stable bool) *sip.Response {
	t.Helper()
	req := request(t, session, seq)
	if stable {
		req.Contact().Params.Add("+sip.instance", "\""+device+"\"")
	} else {
		req.Contact().Params.Remove("+sip.instance")
		req.Contact().Address.User = "1001-" + device
	}
	req.Contact().Params.Add("expires", fmt.Sprint(expires))
	req.SetSource(source)
	authorize(t, r, req, "test-secret", "SHA-256")
	return r.Handle(req, 20001)
}

func TestLoginMultipleClientsReconnectWithoutCooldown(t *testing.T) {
	for _, stable := range []bool{true, false} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			r := New()
			r.Replace([]Account{fixtureAccount("test-secret", 1)})
			now := time.Unix(1800000000, 0)
			r.now = func() time.Time { return now }
			for i := 0; i < 8; i++ {
				source := "127.0.0.1:5090"
				if stable {
					source = fmt.Sprintf("127.0.0.2:%d", 5090+i)
				}
				if res := loginRequest(t, r, "A", fmt.Sprintf("restart-%d", i), source, 1, 300, stable); res.StatusCode != 200 {
					t.Fatal("owner reconnect blocked", res.StatusCode)
				}
				now = now.Add(time.Minute)
				res := loginRequest(t, r, "B", "other", "127.0.0.1:5091", i+1, 300, stable)
				if res.StatusCode != 200 || len(r.Registrations()) != 2 {
					t.Fatal("multiple clients failed to coexist", res.StatusCode)
				}
			}
			// Expiry releases the real registration, not a separate five-minute timer.
			now = now.Add(5 * time.Minute)
			if r.Online("account-1") {
				t.Fatal("expired binding is online")
			}
			if res := loginRequest(t, r, "B", "accepted", "127.0.0.1:5091", 1, 300, stable); res.StatusCode != 200 {
				t.Fatal("offline account retained a cooldown", res.StatusCode)
			}
		})
	}
}

func TestLoginLogoutReleasesOnlyOwner(t *testing.T) {
	for _, stable := range []bool{true, false} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			r := New()
			r.Replace([]Account{fixtureAccount("test-secret", 1)})
			loginRequest(t, r, "A", "first", "127.0.0.1:5090", 1, 300, stable)
			if res := loginRequest(t, r, "B", "foreign", "127.0.0.1:5091", 1, 0, stable); res.StatusCode != 200 || !r.Online("account-1") {
				t.Fatal("foreign logout freed owner", res.StatusCode)
			}
			if res := loginRequest(t, r, "A", "new-sequence", "127.0.0.1:5090", 1, 0, stable); res.StatusCode != 200 || r.Online("account-1") {
				t.Fatal("owner logout with a new Call-ID failed", res.StatusCode)
			}
			if res := loginRequest(t, r, "B", "second", "127.0.0.1:5091", 1, 300, stable); res.StatusCode != 200 {
				t.Fatal("logout did not release account", res.StatusCode)
			}
			if res := loginRequest(t, r, "A", "stale", "127.0.0.1:5090", 1, 300, stable); res.StatusCode != 200 || len(r.Registrations()) != 2 {
				t.Fatal("both clients should remain registered", res.StatusCode)
			}
		})
	}
}

func TestLoginDifferentEndpointsStayIndependent(t *testing.T) {
	for _, mode := range []string{"source-port", "source-ip", "contact", "transport", "new-instance"} {
		t.Run(mode, func(t *testing.T) {
			r := New()
			r.Replace([]Account{fixtureAccount("test-secret", 1)})
			loginRequest(t, r, "A", "first", "127.0.0.1:5090", 1, 300, false)
			req := request(t, "new", 1)
			req.Contact().Params.Remove("+sip.instance")
			req.Contact().Address.User = "1001-A"
			switch mode {
			case "source-port":
				req.SetSource("127.0.0.1:5091")
			case "source-ip":
				req.SetSource("127.0.0.2:5090")
			case "contact":
				req.Contact().Address.User = "1001-B"
			case "transport":
				req.SetTransport("TCP")
			case "new-instance":
				req.Contact().Params.Add("+sip.instance", "A")
			}
			authorize(t, r, req, "test-secret", "MD5")
			if res := r.Handle(req, 20001); res.StatusCode != 200 {
				t.Fatal("second endpoint was blocked", res.StatusCode)
			}
			found := false
			for _, reg := range r.Registrations() {
				found = found || reg.CallID == "first"
			}
			if !found || len(r.Registrations()) != 2 {
				t.Fatal("existing endpoint changed")
			}
		})
	}
}

func TestLoginConcurrentClientsAllRegister(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			r := New()
			r.Replace([]Account{fixtureAccount("test-secret", 1)})
			now := time.Unix(1800000000, 0)
			r.now = func() time.Time { return now }
			if expired {
				loginRequest(t, r, "old", "old", "127.0.0.1:5090", 1, 60, true)
				now = now.Add(time.Minute)
			}
			var requests []*sip.Request
			for i := 0; i < 12; i++ {
				req := request(t, fmt.Sprintf("phone-%d", i), 1)
				authorize(t, r, req, "test-secret", "MD5")
				requests = append(requests, req)
			}
			start, results := make(chan struct{}), make(chan int, len(requests))
			var wg sync.WaitGroup
			for _, req := range requests {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; results <- r.Handle(req, 20001).StatusCode }()
			}
			close(start)
			wg.Wait()
			close(results)
			winners := 0
			for status := range results {
				if status == 200 {
					winners++
				} else {
					t.Fatal(status)
				}
			}
			if winners != len(requests) || len(r.Registrations()) != len(requests) {
				t.Fatal("concurrent login was rejected", winners)
			}
		})
	}
}

func TestLoginListenerResetDoesNotLeaveOfflineLock(t *testing.T) {
	r := New()
	r.Replace([]Account{fixtureAccount("test-secret", 1)})
	loginRequest(t, r, "A", "first", "127.0.0.1:5090", 1, 300, true)
	r.OfflinePort(20001)
	if res := loginRequest(t, r, "B", "second", "127.0.0.1:5091", 1, 300, true); res.StatusCode != 200 {
		t.Fatal("offline account was locked", res.StatusCode)
	}
}
