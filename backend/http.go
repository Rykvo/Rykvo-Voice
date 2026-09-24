package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const cookieName = "rykvo_session"
const sessionTTL = 12 * time.Hour

type attempts struct {
	count int
	until time.Time
}
type server struct {
	webRoot  string
	db       *pgxpool.Pool
	origin   string
	secure   bool
	mu       sync.Mutex
	limits   map[string]attempts
	slots    chan struct{}
	gestures map[string]gesture
	tunnels  *tunnelManager
	modules  *moduleManager
}
type session struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	CSRF    string    `json:"csrfToken"`
	Expires time.Time `json:"expiresAt"`
}

func reply(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]any{"error": map[string]string{"code": code}})
}
func token() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic("Random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}
func tokenHash(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
func requestToken(r *http.Request) string {
	cookie, err := r.Cookie(cookieName)
	if err != nil || len(cookie.Value) != 43 {
		return ""
	}
	return cookie.Value
}
func (s *server) cookie(w http.ResponseWriter, value string, expires time.Time, https ...bool) {
	age := int(sessionTTL.Seconds())
	if value == "" {
		age = -1
	}
	secure := s.secure || (len(https) > 0 && https[0])
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: age})
}
func (s *server) allowedOrigin(origin string) bool {
	return (s.origin != "" && origin == s.origin) || localOrigin(origin) || (s.tunnels != nil && s.tunnels.allowsOrigin(origin))
}
func localOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "80") {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// 只接受本机网卡地址，换 IP 后无需修改配置。
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		local, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.Equal(local) {
			return true
		}
	}
	return false
}
func (s *server) allow(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for ip, record := range s.limits {
		if now.After(record.until) {
			delete(s.limits, ip)
		}
	}
	record, exists := s.limits[key]
	if !exists {
		if len(s.limits) >= 4096 {
			return false
		}
		record.until = now.Add(time.Minute)
	}
	if record.count >= 5 {
		return false
	}
	record.count++
	s.limits[key] = record
	return true
}
func (s *server) readSession(ctx context.Context, r *http.Request) (session, error) {
	var result session
	value := requestToken(r)
	if value == "" {
		return result, pgx.ErrNoRows
	}
	err := s.db.QueryRow(ctx, `SELECT a.id::text,a.username,s.csrf_token,s.expires_at FROM login_sessions s JOIN administrators a ON a.id=s.administrator_id WHERE s.token_hash=$1 AND s.expires_at>now()`, tokenHash(value)).Scan(&result.User.ID, &result.User.Username, &result.CSRF, &result.Expires)
	return result, err
}
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var ok bool
	if r, ok = s.mountRequest(w, r); !ok {
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		s.serveWeb(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if r.Method != http.MethodGet && !s.allowedOrigin(r.Header.Get("Origin")) {
		fail(w, 403, "ORIGIN_REJECTED")
		return
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodPost {
		s.login(ctx, w, r)
		return
	}
	current, err := s.readSession(ctx, r)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "UNAUTHENTICATED")
		return
	}
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(current.CSRF)) != 1 {
		fail(w, 403, "CSRF_REJECTED")
		return
	}
	if r.URL.Path == "/api/ui/activation" || strings.HasPrefix(r.URL.Path, "/api/settings/visibility") {
		s.visibility(ctx, w, r, current)
		return
	}
	if r.URL.Path == "/api/administrator" {
		s.administrator(ctx, w, r, current)
		return
	}
	if r.URL.Path == "/api/modules" || strings.HasPrefix(r.URL.Path, "/api/modules/") {
		s.modulesAPI(ctx, w, r)
		return
	}
	if r.URL.Path == "/api/tunnel" || strings.HasPrefix(r.URL.Path, "/api/tunnel/") {
		s.tunnel(w, r, current)
		return
	}
	if r.URL.Path != "/api/session" {
		fail(w, 503, "NOT_CONNECTED")
		return
	}
	switch r.Method {
	case http.MethodGet:
		reply(w, 200, map[string]any{"data": current})
	case http.MethodDelete:
		if _, err = s.db.Exec(ctx, `DELETE FROM login_sessions WHERE token_hash=$1`, tokenHash(requestToken(r))); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		s.cookie(w, "", time.Unix(0, 0), strings.HasPrefix(r.Header.Get("Origin"), "https://"))
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		fail(w, 405, "METHOD_NOT_ALLOWED")
	}
}
func decodeBody(w http.ResponseWriter, r *http.Request, input any) bool {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "application/json" {
		fail(w, 415, "JSON_REQUIRED")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "INVALID_INPUT")
		return false
	}
	return true
}
func (s *server) login(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	// 仅信任本机 Nginx 覆写的客户端地址。
	if net.ParseIP(peer).IsLoopback() && net.ParseIP(r.Header.Get("X-Real-IP")) != nil {
		peer = r.Header.Get("X-Real-IP")
	}
	if !s.allow(peer) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "RATE_LIMITED")
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		fail(w, 429, "BUSY")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || len(input.Username) > 128 || input.Password == "" || len(input.Password) > 128 {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	var id int64
	var salt, hash []byte
	var iterations int
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT id,password_salt,password_hash,password_iterations FROM administrators WHERE username=$1 FOR SHARE`, input.Username).Scan(&id, &salt, &hash, &iterations)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if missing {
		salt, hash, iterations = make([]byte, 16), make([]byte, 32), passwordIterations
	}
	valid := verifyPassword(input.Password, salt, hash, iterations)
	input.Password = ""
	if !valid || missing {
		fail(w, 401, "INVALID_CREDENTIALS")
		return
	}
	value := token()
	current := session{CSRF: token(), Expires: time.Now().UTC().Add(sessionTTL)}
	current.User.ID, current.User.Username = strconv.FormatInt(id, 10), input.Username
	if _, err = tx.Exec(ctx, `DELETE FROM login_sessions WHERE expires_at<=now() OR token_hash=$1`, tokenHash(requestToken(r))); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if _, err = tx.Exec(ctx, `INSERT INTO login_sessions(token_hash,administrator_id,csrf_token,expires_at) VALUES($1,$2,$3,$4)`, tokenHash(value), id, current.CSRF, current.Expires); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	s.cookie(w, value, current.Expires, strings.HasPrefix(r.Header.Get("Origin"), "https://"))
	reply(w, 200, map[string]any{"data": current})
}
