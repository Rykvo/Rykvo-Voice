package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCleanupRetriesStages(t *testing.T) {
	b := tunnelBinding{Domain: "panel.example.com", Name: "rykvo-fixture", TunnelID: fixtureTunnel}
	counts := map[string]int{}
	var order []string
	c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		counts[key]++
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, fixtureTunnel):
			cfReply(w, cfTunnel{ID: fixtureTunnel, Name: b.Name})
		case r.Method == "GET":
			if counts[key] == 1 {
				w.WriteHeader(503)
				w.Write([]byte("upstream offline"))
				return
			}
			cfReply(w, []cfDNS{{ID: fixtureDNS, Name: b.Domain, Type: "CNAME", Content: fixtureTunnel + ".cfargotunnel.com", Comment: b.Name}})
		case strings.HasSuffix(r.URL.Path, fixtureDNS):
			if counts[key] == 1 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(429)
				w.Write([]byte(`{"success":false,"errors":[{"code":1015,"message":"fixture-secret"}]}`))
				return
			}
			order = append(order, "dns")
			cfReply(w, nil)
		case strings.HasSuffix(r.URL.Path, "/connections"):
			order = append(order, "connections")
			cfReply(w, nil)
		case r.Method == "DELETE":
			if counts[key] == 1 {
				w.WriteHeader(400)
				w.Write([]byte(`{"success":false,"errors":[{"code":1022,"message":"fixture-secret"}]}`))
				return
			}
			order = append(order, "tunnel")
			cfReply(w, nil)
		}
	})
	var delays []time.Duration
	c.wait = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(original)
	if err := c.removeOwned(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "dns,connections,tunnel" {
		t.Fatal(order)
	}
	if len(delays) != 3 || delays[1] != 7*time.Second {
		t.Fatal(delays)
	}
	text := output.String()
	for _, expected := range []string{"stage=list_dns", "stage=delete_dns", "stage=delete_tunnel", "http=400 provider=1022"} {
		if !strings.Contains(text, expected) {
			t.Fatal("missing diagnostic", expected)
		}
	}
	if strings.Contains(text, "fixture-secret") || strings.Contains(text, fixtureAccount) {
		t.Fatal("secret in log")
	}
}

func TestCleanupIdempotencyAndOwnership(t *testing.T) {
	for _, state := range []string{"missing", "deleted", "foreign", "denied"} {
		t.Run(state, func(t *testing.T) {
			b := tunnelBinding{Domain: "panel.example.com", Name: "rykvo-fixture", TunnelID: fixtureTunnel}
			deletes, waits := 0, 0
			c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "dns_records"):
					if r.Method == "DELETE" {
						deletes++
						w.WriteHeader(404)
						return
					}
					cfReply(w, []cfDNS{{ID: fixtureDNS, Name: b.Domain, Type: "CNAME", Content: fixtureTunnel + ".cfargotunnel.com", Comment: b.Name}})
				case state == "missing":
					w.WriteHeader(404)
				case state == "denied":
					w.WriteHeader(403)
				case state == "foreign":
					cfReply(w, cfTunnel{ID: fixtureTunnel, Name: "someone-else"})
				default:
					now := time.Now()
					cfReply(w, cfTunnel{ID: fixtureTunnel, Name: b.Name, DeletedAt: &now})
				}
			})
			c.wait = func(context.Context, time.Duration) error { waits++; return nil }
			err := c.removeOwned(context.Background(), b)
			if state == "foreign" || state == "denied" {
				if err == nil || deletes != 0 {
					t.Fatal("changed unverified resources")
				}
				stage, _ := cleanupDetails(err)
				if stage != "check_tunnel" {
					t.Fatal(stage)
				}
			} else if err != nil || deletes != 1 {
				t.Fatal("idempotent DNS cleanup failed", err, deletes)
			}
			if waits != 0 {
				t.Fatal("retried a permanent error")
			}
		})
	}
}

func TestCleanupRetryLimits(t *testing.T) {
	attempts, waits := 0, 0
	err := cleanupStep(context.Background(), "list_dns", func(_ context.Context, d time.Duration) error {
		if d != 2*time.Second<<waits {
			t.Fatal(d)
		}
		waits++
		return nil
	}, func() error { attempts++; return &cfRequestError{code: "CLOUDFLARE_UNAVAILABLE", status: 503} })
	if attempts != 5 || waits != 4 || err == nil {
		t.Fatal(attempts, waits, err)
	}
	stage, code := cleanupDetails(err)
	if stage != "list_dns" || code != "CLOUDFLARE_UNAVAILABLE" {
		t.Fatal(stage, code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cleanupStep(ctx, "delete_dns", nil, func() error { t.Fatal("ran after cancellation"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = cleanupStep(ctx, "delete_dns", nil, func() error {
		return &cfRequestError{code: "CLOUDFLARE_RATE_LIMITED", status: 429, retryAfter: time.Hour}
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("ignored time budget", err)
	}
}

func TestCleanupRetriesTransportFailure(t *testing.T) {
	attempts := 0
	c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		cfReply(w, nil)
	})
	c.wait = func(context.Context, time.Duration) error { return nil }
	if err := cleanupStep(context.Background(), "delete_dns", c.wait, func() error {
		return c.request(context.Background(), "DELETE", "/fixture", nil, nil)
	}); err != nil || attempts != 2 {
		t.Fatal(err, attempts)
	}
}

func TestConnectorGracefulStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process signals")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "connector")
	script := "#!/bin/sh\ntrap 'echo stopped > stopped; exit 0' TERM\necho ready > ready\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := &tunnelManager{dir: dir, bin: bin}
	cmd := tm.command(ctx)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	_ = cmd.Wait()
	if _, err := os.Stat(filepath.Join(dir, "stopped")); err != nil {
		t.Fatal("connector was killed without graceful shutdown")
	}
}
