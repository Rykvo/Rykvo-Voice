package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const fixtureZone = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const fixtureAccount = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const fixtureDNS = "cccccccccccccccccccccccccccccccc"
const fixtureTunnel = "11111111-2222-3333-4444-555555555555"

func TestTunnelInput(t *testing.T) {
	for _, bad := range []string{"", "localhost", "192.168.1.1", "http://a.com", "a.com:80", "a.com/x", "*.a.com", "a@a.com", "a..com", "-a.com", "a.test", "a\n.com", strings.Repeat("a", 64) + ".com"} {
		if normalizeDomain(bad) != "" {
			t.Fatalf("accepted %q", bad)
		}
	}
	if normalizeDomain(" Panel.Example.com. ") != "panel.example.com" {
		t.Fatal("normalization")
	}
	for _, bad := range []string{"http://dash.cloudflare.com/argotunnel?a", "https://dash.cloudflare.com.evil/argotunnel?a", "https://x@dash.cloudflare.com/argotunnel?a", "https://dash.cloudflare.com:443/argotunnel?a", "https://dash.cloudflare.com/other?a"} {
		if authorizationURL(bad) != "" {
			t.Fatal("untrusted authorization URL")
		}
	}
	if authorizationURL("https://dash.cloudflare.com/argotunnel?fixture") == "" {
		t.Fatal("valid authorization URL rejected")
	}
}
func fixtureCertificate(t *testing.T, path string) originCertificate {
	t.Helper()
	cert := originCertificate{ZoneID: fixtureZone, AccountID: fixtureAccount, APIToken: "fixture-secret"}
	data, _ := json.Marshal(cert)
	if err := privateFile(path, pem.EncodeToMemory(&pem.Block{Type: "ARGO TUNNEL TOKEN", Bytes: data})); err != nil {
		t.Fatal(err)
	}
	return cert
}
func TestTunnelCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cert.pem")
	cert := fixtureCertificate(t, path)
	actual, err := readCertificate(path)
	if err != nil || actual != cert {
		t.Fatal("certificate parsing", err)
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("credentials readable by others")
	}
	if err = privateFile(path, []byte("invalid")); err != nil {
		t.Fatal(err)
	}
	if _, err = readCertificate(path); err != tunnelError("AUTH_INVALID") {
		t.Fatal("invalid certificate accepted")
	}
}
func TestCloudflaredAuthorizationCLI(t *testing.T) {
	bin := os.Getenv("CLOUDFLARED_SMOKE_BIN")
	if bin == "" {
		t.Skip("Optional installed cloudflared smoke test")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".cloudflared"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tm := &tunnelManager{dir: dir, bin: bin, binding: tunnelBinding{Status: "connecting"}}
	done := make(chan error, 1)
	go func() { done <- tm.authorize(ctx) }()
	defer func() { cancel(); <-done }()
	waitTunnel(t, tm, "authorizing")
	tm.mu.Lock()
	valid := authorizationURL(tm.authURL) != ""
	tm.mu.Unlock()
	if !valid {
		t.Fatal("installed CLI did not produce supported authorization URL")
	}
}
func cfFixture(t *testing.T, handler http.HandlerFunc) *cfClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("missing credential")
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	c := newCFClient(originCertificate{ZoneID: fixtureZone, AccountID: fixtureAccount, APIToken: "fixture-secret"})
	c.base = server.URL
	return c
}
func cfReply(w http.ResponseWriter, value any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": value})
}
func TestCloudflareOwnership(t *testing.T) {
	b := tunnelBinding{Domain: "panel.example.com", Name: "rykvo-fixture", TunnelID: fixtureTunnel}
	records := []cfDNS{{ID: fixtureDNS, Name: b.Domain, Type: "A", Content: "192.0.2.1"}}
	var deleted []string
	c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted = append(deleted, r.URL.Path)
			cfReply(w, nil)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "dns_records"):
			if r.Method == "POST" {
				t.Error("overwrote existing DNS")
			}
			cfReply(w, records)
		case strings.HasSuffix(r.URL.Path, fixtureTunnel):
			cfReply(w, cfTunnel{ID: fixtureTunnel, Name: b.Name})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	if _, err := c.ensureDNS(context.Background(), b); err != tunnelError("DNS_CONFLICT") {
		t.Fatal("existing DNS accepted", err)
	}
	if err := c.removeOwned(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || !strings.HasSuffix(deleted[0], "/connections") || !strings.HasSuffix(deleted[1], fixtureTunnel) {
		t.Fatal("removed unrelated DNS", deleted)
	}
	records = []cfDNS{{ID: fixtureDNS, Name: b.Domain, Type: "CNAME", Content: fixtureTunnel + ".cfargotunnel.com", Comment: b.Name, Proxied: true}}
	if id, err := c.ensureDNS(context.Background(), b); err != nil || id != fixtureDNS {
		t.Fatal("owned DNS not reused", err)
	}
	deleted = nil
	if err := c.removeOwned(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 || !strings.HasSuffix(deleted[0], fixtureDNS) {
		t.Fatal("owned resource cleanup", deleted)
	}
}
func TestCloudflareCreateAndRecover(t *testing.T) {
	var existing bool
	var creates int
	c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			items := []cfTunnel{}
			if existing {
				items = append(items, cfTunnel{ID: fixtureTunnel, Name: "rykvo-fixture"})
			}
			cfReply(w, items)
			return
		}
		var body struct {
			Name   string `json:"name"`
			Secret []byte `json:"tunnel_secret"`
			Source string `json:"config_src"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Secret) != 32 || body.Source != "local" {
			t.Error("invalid create request")
		}
		creates++
		existing = true
		cfReply(w, cfTunnel{ID: fixtureTunnel, Name: body.Name})
	})
	for i := 0; i < 2; i++ {
		if id, err := c.ensureTunnel(context.Background(), "rykvo-fixture", make([]byte, 32)); err != nil || id != fixtureTunnel {
			t.Fatal(err)
		}
	}
	if creates != 1 {
		t.Fatal("duplicate tunnel created")
	}
}
func TestCloudflareZoneAndErrors(t *testing.T) {
	c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
		cfReply(w, map[string]any{"name": "example.com", "account": map[string]string{"id": fixtureAccount}})
	})
	if err := c.checkZone(context.Background(), "panel.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.checkZone(context.Background(), "notexample.com"); err != tunnelError("DOMAIN_NOT_AUTHORIZED") {
		t.Fatal("zone boundary", err)
	}
	denied := cfFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); w.Write([]byte("fixture-secret")) })
	if err := denied.checkZone(context.Background(), "example.com"); !errors.Is(err, tunnelError("CLOUDFLARE_PERMISSION")) {
		t.Fatal("unsafe error", err)
	}
}
func TestTunnelOrigin(t *testing.T) {
	tm := &tunnelManager{binding: tunnelBinding{Domain: "panel.example.com", Resume: true, DNSID: fixtureDNS}}
	s := &server{origin: "http://192.168.1.1", tunnels: tm}
	for _, origin := range []string{"http://192.168.1.1", "https://panel.example.com"} {
		if !s.allowedOrigin(origin) {
			t.Fatal("valid origin rejected")
		}
	}
	for _, origin := range []string{"", "https://panel.example.com.evil", "http://panel.example.com", "https://other.example.com"} {
		if s.allowedOrigin(origin) {
			t.Fatal("untrusted origin allowed")
		}
	}
	w := httptest.NewRecorder()
	s.cookie(w, token(), time.Now().Add(sessionTTL), true)
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie not secure")
	}
	tm.binding.Resume = false
	if s.allowedOrigin("https://panel.example.com") {
		t.Fatal("removed origin remains authorized")
	}
}
func waitTunnel(t *testing.T, tm *tunnelManager, status string) {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		if tm.read().Status == status {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state is %s, want %s", tm.read().Status, status)
}
func testTunnelDatabase(t *testing.T, pool *pgxpool.Pool, s *server, request func(string, string, any, int) map[string]any) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' 'https://dash.cloudflare.com/argotunnel?fixture'\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	tm, err := newTunnelManager(ctx, pool, dir, bin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); tm.workers.Wait() }()
	s.tunnels = tm
	s.limits = map[string]attempts{}
	request("GET", "/api/tunnel", nil, 200)
	request("POST", "/api/tunnel/connect", map[string]string{"domain": "http://127.0.0.1"}, 400)
	request("POST", "/api/tunnel/connect", map[string]string{"domain": "panel.example.com"}, 200)
	waitTunnel(t, tm, "authorizing")
	owner := tm.read().Owner
	if tm.view(owner).AuthorizationURL == "" || tm.view("someone-else").AuthorizationURL != "" {
		t.Fatal("authorization owner isolation")
	}
	if err = tm.connect("someone-else", "panel.example.com"); err != tunnelError("TUNNEL_OWNER_REQUIRED") {
		t.Fatal("foreign owner allowed")
	}
	if err = tm.connect(owner, "other.example.com"); err != tunnelError("DISCONNECT_FIRST") {
		t.Fatal("domain replaced without cleanup")
	}
	request("POST", "/api/tunnel/disconnect", map[string]any{}, 200)
	waitTunnel(t, tm, "disconnected")
	var saved []byte
	if err = pool.QueryRow(ctx, `SELECT binding FROM tunnel_settings`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	var binding tunnelBinding
	json.Unmarshal(saved, &binding)
	if binding.Domain != "" || binding.Status != "disconnected" {
		t.Fatal("cleanup not persisted")
	}
	// 资源创建后的 DNS 冲突必须保留归属，供注销安全清理。
	fixtureCertificate(t, tm.certificatePath())
	c := cfFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			cfReply(w, nil)
		case r.URL.Path == "/zones/"+fixtureZone:
			cfReply(w, map[string]any{"name": "example.com", "account": map[string]string{"id": fixtureAccount}})
		case strings.Contains(r.URL.Path, "dns_records"):
			cfReply(w, []cfDNS{{ID: fixtureDNS, Name: "panel.example.com", Type: "A", Content: "192.0.2.1"}})
		case strings.HasSuffix(r.URL.Path, fixtureTunnel):
			cfReply(w, cfTunnel{ID: fixtureTunnel, Name: tm.read().Name})
		case r.Method == "POST":
			cfReply(w, cfTunnel{ID: fixtureTunnel, Name: tm.read().Name})
		default:
			cfReply(w, []cfTunnel{})
		}
	})
	tm.client = func(originCertificate) *cfClient { return c }
	request("POST", "/api/tunnel/connect", map[string]string{"domain": "panel.example.com"}, 200)
	waitTunnel(t, tm, "failed")
	if b := tm.read(); b.ErrorCode != "DNS_CONFLICT" || b.TunnelID != fixtureTunnel || !b.Provisioned {
		t.Fatal("lost provisioned resource state")
	}
	request("POST", "/api/tunnel/disconnect", map[string]any{}, 200)
	waitTunnel(t, tm, "disconnected")
	if _, err = os.Stat(tm.certificatePath()); !os.IsNotExist(err) {
		t.Fatal("credentials not removed")
	}
	fixtureCertificate(t, tm.certificatePath())
	if err = tm.update(func(b *tunnelBinding) {
		*b = tunnelBinding{Domain: "panel.example.com", Owner: owner, Name: "rykvo-fixture", TunnelID: fixtureTunnel, Status: "failed", Removing: true, Provisioned: true}
	}); err != nil {
		t.Fatal(err)
	}
	denied := cfFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	tm.client = func(originCertificate) *cfClient { return denied }
	request("POST", "/api/tunnel/disconnect", map[string]any{}, 200)
	waitTunnel(t, tm, "failed")
	if b := tm.read(); b.CleanupStage != "check_tunnel" || b.CleanupCode != "CLOUDFLARE_PERMISSION" || b.ErrorCode != "DISCONNECT_FAILED" || b.Resume {
		t.Fatal("lost cleanup diagnostics", b)
	}
	if _, err = os.Stat(tm.certificatePath()); err != nil {
		t.Fatal("removed recovery credentials")
	}
	if err = tm.connect(owner, "panel.example.com"); err != tunnelError("DISCONNECT_FIRST") {
		t.Fatal("reconnected while cleanup incomplete")
	}
	tm.client = func(originCertificate) *cfClient { return c }
	request("POST", "/api/tunnel/disconnect", map[string]any{}, 200)
	waitTunnel(t, tm, "disconnected")
	// 即使上次清理后凭据已删除，重启仍能完成剩余本地清理。
	if err = tm.update(func(b *tunnelBinding) {
		*b = tunnelBinding{Domain: "panel.example.com", Owner: owner, Name: "rykvo-fixture", Status: "disconnecting", Removing: true, Provisioned: true, RemoteCleared: true}
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := newTunnelManager(ctx, pool, dir, bin)
	if err != nil {
		t.Fatal(err)
	}
	waitTunnel(t, restored, "disconnected")
	restored.workers.Wait()
}
