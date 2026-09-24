package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type tunnelBinding struct {
	Domain        string `json:"domain"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	TunnelID      string `json:"tunnelId"`
	DNSID         string `json:"dnsId"`
	Status        string `json:"status"`
	ErrorCode     string `json:"errorCode,omitempty"`
	CleanupStage  string `json:"cleanupStage,omitempty"`
	CleanupCode   string `json:"cleanupCode,omitempty"`
	Resume        bool   `json:"resume"`
	Provisioned   bool   `json:"provisioned"`
	Removing      bool   `json:"removing"`
	RemoteCleared bool   `json:"remoteCleared"`
}
type tunnelView struct {
	Status           string `json:"status"`
	Domain           string `json:"domain"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	ErrorCode        string `json:"errorCode,omitempty"`
	CleanupStage     string `json:"cleanupStage,omitempty"`
	CleanupCode      string `json:"cleanupCode,omitempty"`
}
type tunnelManager struct {
	mu       sync.Mutex
	workers  sync.WaitGroup
	db       *pgxpool.Pool
	client   func(originCertificate) *cfClient
	ctx      context.Context
	dir, bin string
	binding  tunnelBinding
	authURL  string
	cancel   context.CancelFunc
	done     chan struct{}
	running  bool
}

func newTunnelManager(ctx context.Context, db *pgxpool.Pool, dir, bin string) (*tunnelManager, error) {
	if !filepath.IsAbs(dir) || !filepath.IsAbs(bin) {
		return nil, errors.New("invalid tunnel configuration")
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return nil, errors.New("cloudflared not installed")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".cloudflared"), 0700); err != nil {
		return nil, err
	}
	t := &tunnelManager{ctx: ctx, db: db, client: newCFClient, dir: dir, bin: bin, binding: tunnelBinding{Status: "disconnected"}}
	var data []byte
	if err := db.QueryRow(ctx, `SELECT binding FROM tunnel_settings WHERE singleton=true`).Scan(&data); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &t.binding); err != nil {
		return nil, err
	}
	if t.binding.Status == "" {
		t.binding.Status = "disconnected"
	}
	if t.binding.Domain != "" {
		if normalizeDomain(t.binding.Domain) != t.binding.Domain || t.binding.Name == "" {
			return nil, errors.New("invalid saved tunnel")
		}
		if t.binding.Removing && t.binding.Status == "disconnecting" {
			t.binding.Resume = false
			t.startCleanupLocked()
		} else if t.binding.Resume && !t.binding.Removing {
			t.binding.Status = "connecting"
			t.startLocked()
		} else {
			t.binding.Status = "failed"
			t.binding.ErrorCode = "RETRY_REQUIRED"
			if t.binding.Removing {
				t.binding.ErrorCode = "DISCONNECT_FAILED"
			}
		}
	}
	return t, nil
}
func (t *tunnelManager) saveLocked() error {
	data, err := json.Marshal(t.binding)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = t.db.Exec(ctx, `UPDATE tunnel_settings SET binding=$1,updated_at=now() WHERE singleton=true`, data)
	return err
}
func (t *tunnelManager) update(change func(*tunnelBinding)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.binding
	change(&t.binding)
	if err := t.saveLocked(); err != nil {
		t.binding = previous
		return err
	}
	return nil
}
func (t *tunnelManager) read() tunnelBinding { t.mu.Lock(); defer t.mu.Unlock(); return t.binding }
func (t *tunnelManager) view(owner string) tunnelView {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := tunnelView{Status: t.binding.Status, Domain: t.binding.Domain, ErrorCode: t.binding.ErrorCode, CleanupStage: t.binding.CleanupStage, CleanupCode: t.binding.CleanupCode}
	if owner == t.binding.Owner {
		v.AuthorizationURL = t.authURL
	}
	// 授权地址仅返回给发起者。
	if v.Status == "authorizing" && v.AuthorizationURL == "" {
		v.Status = "connecting"
	}
	return v
}
func (t *tunnelManager) allowsOrigin(origin string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.binding.Resume && t.binding.DNSID != "" && origin == "https://"+t.binding.Domain
}
func (t *tunnelManager) connect(owner, domain string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.binding.Owner != "" && t.binding.Owner != owner {
		return tunnelError("TUNNEL_OWNER_REQUIRED")
	}
	if t.binding.Domain != "" && t.binding.Domain != domain {
		return tunnelError("DISCONNECT_FIRST")
	}
	if t.ctx.Err() != nil {
		return t.ctx.Err()
	}
	if t.binding.Removing {
		return tunnelError("DISCONNECT_FIRST")
	}
	if t.running {
		return nil
	}
	previous := t.binding
	if t.binding.Domain == "" {
		name := make([]byte, 16)
		if _, err := rand.Read(name); err != nil {
			return err
		}
		t.binding = tunnelBinding{Domain: domain, Owner: owner, Name: "rykvo-" + hex.EncodeToString(name)}
	}
	t.binding.Status = "connecting"
	t.binding.ErrorCode = ""
	t.authURL = ""
	if err := t.saveLocked(); err != nil {
		t.binding = previous
		return err
	}
	t.startLocked()
	return nil
}
func (t *tunnelManager) startLocked() {
	ctx, cancel := context.WithCancel(t.ctx)
	done := make(chan struct{})
	t.cancel, t.done, t.running = cancel, done, true
	t.workers.Add(1)
	go func() {
		defer t.workers.Done()
		defer close(done)
		err := t.connectFlow(ctx)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.running = false
		t.authURL = ""
		if t.binding.Status == "disconnecting" || t.ctx.Err() != nil {
			return
		}
		t.binding.Status = "failed"
		t.binding.ErrorCode = "TUNNEL_START_FAILED"
		var e tunnelError
		if errors.As(err, &e) {
			t.binding.ErrorCode = string(e)
		}
		if t.saveLocked() != nil {
			t.binding.ErrorCode = "DATABASE_UNAVAILABLE"
		}
	}()
}
func (t *tunnelManager) certificatePath() string {
	return filepath.Join(t.dir, ".cloudflared", "cert.pem")
}
func (t *tunnelManager) connectFlow(ctx context.Context) error {
	cert, err := readCertificate(t.certificatePath())
	if err != nil {
		if t.read().Provisioned {
			return err
		}
		if err = t.authorize(ctx); err != nil {
			return err
		}
		cert, err = readCertificate(t.certificatePath())
		if err != nil {
			return err
		}
	}
	c := t.client(cert)
	b := t.read()
	if err = c.checkZone(ctx, b.Domain); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err = t.update(func(b *tunnelBinding) {
		if !b.Removing {
			b.Resume = true
			b.Status = "connecting"
		}
	}); err != nil {
		return err
	}
	t.mu.Lock()
	t.authURL = ""
	t.mu.Unlock()
	secretPath := filepath.Join(t.dir, "secret")
	secret, err := os.ReadFile(secretPath)
	if os.IsNotExist(err) {
		secret = make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			return err
		}
		if err = privateFile(secretPath, secret); err != nil {
			return err
		}
	} else if err != nil || len(secret) != 32 {
		return tunnelError("CREDENTIALS_INVALID")
	}
	defer clear(secret)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err = t.update(func(b *tunnelBinding) { b.Provisioned = true }); err != nil {
		return err
	}
	id, err := c.ensureTunnel(ctx, b.Name, secret)
	if err != nil {
		return err
	}
	if err = t.update(func(b *tunnelBinding) { b.TunnelID = id }); err != nil {
		return err
	}
	b = t.read()
	dnsID, err := c.ensureDNS(ctx, b)
	if err != nil {
		return err
	}
	if err = t.update(func(b *tunnelBinding) { b.DNSID = dnsID }); err != nil {
		return err
	}
	credentials, _ := json.Marshal(map[string]any{"AccountTag": cert.AccountID, "TunnelSecret": secret, "TunnelID": id})
	if err = privateFile(filepath.Join(t.dir, "credentials.json"), credentials); err != nil {
		return err
	}
	// JSON 是有效 YAML；仅使用固定本机源站，输入域名不会成为代理目标。
	config, _ := json.Marshal(map[string]any{"tunnel": id, "credentials-file": filepath.Join(t.dir, "credentials.json"), "metrics": "127.0.0.1:20241", "ingress": []any{map[string]any{"hostname": b.Domain, "service": "http://127.0.0.1:80"}, map[string]string{"service": "http_status:404"}}})
	if err = privateFile(filepath.Join(t.dir, "config.json"), config); err != nil {
		return err
	}
	return t.runConnector(ctx)
}
func (s *server) tunnel(w http.ResponseWriter, r *http.Request, current session) {
	if s.tunnels == nil {
		fail(w, 503, "TUNNEL_NOT_CONFIGURED")
		return
	}
	var err error
	switch {
	case r.URL.Path == "/api/tunnel" && r.Method == "GET":
	case r.URL.Path == "/api/tunnel/connect" && r.Method == "POST":
		var input struct {
			Domain string `json:"domain"`
		}
		if !decodeBody(w, r, &input) {
			return
		}
		domain := normalizeDomain(input.Domain)
		if domain == "" {
			fail(w, 400, "INVALID_DOMAIN")
			return
		}
		if !s.allow("tunnel:" + current.User.ID) {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "RATE_LIMITED")
			return
		}
		err = s.tunnels.connect(current.User.ID, domain)
	case r.URL.Path == "/api/tunnel/disconnect" && r.Method == "POST":
		if !decodeBody(w, r, &struct{}{}) {
			return
		}
		err = s.tunnels.disconnect(current.User.ID)
	default:
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	if err != nil {
		var e tunnelError
		if errors.As(err, &e) {
			fail(w, 409, string(e))
		} else {
			fail(w, 503, "TUNNEL_UNAVAILABLE")
		}
		return
	}
	reply(w, 200, map[string]any{"data": s.tunnels.view(current.User.ID)})
}
