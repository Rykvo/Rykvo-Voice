package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localRequest(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = "127.0.0.1"
	return r
}

func TestPublicAndLocalMounts(t *testing.T) {
	s := &server{webRoot: t.TempDir(), origin: "https://panel.example.com", limits: map[string]attempts{}, slots: make(chan struct{}, 2)}
	for name, body := range map[string]string{
		"login.html": "<!doctype html><html><head><script src=\"login.js\"></script></head><body>LOGIN_ONLY</body></html>",
		"index.html": "<html><head></head><body>APPLICATION_ONLY</body></html>",
		"login.js":   "PUBLIC_SCRIPT", "app.js": "PRIVATE_SCRIPT",
	} {
		if err := os.WriteFile(filepath.Join(s.webRoot, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		host, path string
		status     int
		location   string
	}{
		{"127.0.0.1", "/", 200, ""}, {"localhost", "/", 200, ""}, {"[::1]", "/", 200, ""},
		{"127.0.0.1:80", "/api/session", 401, ""}, {"127.0.0.1", "/gly", 404, ""},
		{"panel.example.com", "/", 404, ""}, {"panel.example.com", "/login.js", 404, ""},
		{"panel.example.com", "/api/session", 404, ""}, {"panel.example.com", "/gly", 200, ""},
		{"panel.example.com", "/gly/", 308, "/gly"}, {"panel.example.com", "/gly/login.html", 303, "/gly"},
		{"panel.example.com", "/gly/index.html", 303, "/gly"},
		{"panel.example.com", "/gly/login.js", 200, ""}, {"panel.example.com", "/gly/app.js", 401, ""},
		{"panel.example.com", "/gly/api/session", 401, ""}, {"panel.example.com", "/glyevil", 404, ""},
		{"panel.example.com", "/gly/../login.js", 404, ""},
		{"panel.example.com", "/gly/%2e%2e/login.js", 404, ""},
		{"user@127.0.0.1", "/", 404, ""}, {"127.0.0.1.evil", "/", 404, ""},
	} {
		t.Run(tc.host+tc.path, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			r.Host = tc.host
			r.Header.Set("X-Forwarded-Host", "127.0.0.1")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			if tc.path == "/gly" && tc.status == 200 {
				if !strings.Contains(w.Body.String(), `<base href="/gly/">`) || strings.Contains(w.Body.String(), "APPLICATION_ONLY") {
					t.Fatal("invalid public entry")
				}
			}
			if tc.path == "/" && tc.status == 200 && strings.Contains(w.Body.String(), "<base") {
				t.Fatal("LAN base was changed")
			}
			if tc.status == 404 && (strings.Contains(w.Body.String(), "LOGIN_ONLY") || strings.Contains(w.Body.String(), "/gly")) {
				t.Fatal("hidden entry leaked")
			}
		})
	}
	for _, origin := range []string{"https://panel.example.com", "https://foreign.example.com"} {
		r := httptest.NewRequest("POST", "/gly/api/session", strings.NewReader(`{}`))
		r.Host = "panel.example.com"
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		want := 400
		if origin != s.origin {
			want = 403
		}
		if w.Code != want {
			t.Fatal("prefixed origin validation", w.Code)
		}
	}
}

func testPrefixedSession(t *testing.T, s *server, value, csrf string) {
	t.Helper()
	original := s.webRoot
	s.webRoot = t.TempDir()
	defer func() { s.webRoot = original }()
	for name, body := range map[string]string{"index.html": "<html><head></head><body>APPLICATION_ONLY</body></html>", "login.html": "LOGIN_ONLY", "app.js": "PRIVATE_SCRIPT"} {
		if err := os.WriteFile(filepath.Join(s.webRoot, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
		text         string
	}{
		{"GET", "/gly", 200, "APPLICATION_ONLY"},
		{"GET", "/gly/app.js", 200, "PRIVATE_SCRIPT"},
		{"GET", "/gly/api/session", 200, csrf},
		{"GET", "/api/session", 404, ""},
		{"PUT", "/gly/api/settings/visibility", 403, "CSRF_REJECTED"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.Host = "panel.example.com"
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: value})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.text) {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
		if tc.path == "/gly" && strings.Contains(w.Body.String(), "LOGIN_ONLY") {
			t.Fatal("mixed login and application pages")
		}
	}
}
