package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestActivation(t *testing.T) {
	s := &server{}
	for i := 0; i < 9; i++ {
		if s.activate("one", false) {
			t.Fatal("Early activation")
		}
	}
	if !s.activate("one", false) {
		t.Fatal("Tenth click did not activate")
	}
	if s.activate("one", false) {
		t.Fatal("Counter did not reset")
	}
	s.activate("one", true)
	if s.gestures["one"].count != 0 {
		t.Fatal("Outside click did not reset")
	}
	s.gestures["one"] = gesture{9, time.Now().Add(-3 * time.Second)}
	if s.activate("one", false) {
		t.Fatal("Expired sequence accepted")
	}
	if s.activate("two", false) {
		t.Fatal("Sessions share counters")
	}
}

func TestVisibilityDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("Set TEST_DATABASE_URL to an empty isolated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var dbName string
	if err = pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil || !strings.HasSuffix(dbName, "_test") {
		t.Fatal("Test database name must end in _test")
	}
	var existing bool
	if err = pool.QueryRow(ctx, `SELECT to_regclass('administrators') IS NOT NULL`).Scan(&existing); err != nil || existing {
		t.Fatal("Test requires a fresh database")
	}
	if _, err = pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	salt, hash, _ := newPassword("login-test-password")
	var id string
	if err = pool.QueryRow(ctx, `INSERT INTO administrators(username,password_salt,password_hash,password_iterations) VALUES('tester',$1,$2,$3) RETURNING id::text`, salt, hash, passwordIterations).Scan(&id); err != nil {
		t.Fatal(err)
	}
	guardSalt, guardHash, _ := newPassword("independent-test-password")
	if _, err = pool.Exec(ctx, `INSERT INTO visibility_security(password_salt,password_hash,password_iterations) VALUES($1,$2,$3)`, guardSalt, guardHash, passwordIterations); err != nil {
		t.Fatal(err)
	}
	value, csrf := token(), token()
	if _, err = pool.Exec(ctx, `INSERT INTO login_sessions(token_hash,administrator_id,csrf_token,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, tokenHash(value), id, csrf); err != nil {
		t.Fatal(err)
	}
	s := &server{db: pool, origin: "http://test.local", slots: make(chan struct{}, 2)}
	testModuleDatabase(t, s, value, csrf)
	testPrefixedSession(t, s, value, csrf)
	request := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		encoded, _ := json.Marshal(body)
		r := localRequest(method, path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", s.origin)
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: value})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var result map[string]any
		if w.Body.Len() > 0 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	testTunnelDatabase(t, pool, s, request)
	const base = "/api/settings/visibility"
	request("GET", base+"/access", nil, 403)
	request("PUT", base, map[string]any{"features": map[string]bool{"phone": false}}, 403)
	request("POST", base+"/unlock", map[string]string{"password": "login-test-password"}, 403)
	request("POST", base+"/unlock", map[string]string{"password": "independent-test-password"}, 200)
	request("GET", base+"/access", nil, 200)
	request("PUT", base, map[string]any{"features": map[string]bool{"unknown": false}}, 400)
	request("PUT", base, map[string]any{"features": map[string]bool{"phone": false}}, 200)
	result := request("GET", base, nil, 200)
	if result["data"].(map[string]any)["features"].(map[string]any)["phone"] != false {
		t.Fatal("Preference not persisted")
	}
	request("PUT", base+"/password", map[string]string{"currentPassword": "wrong-password", "newPassword": "replacement-test-password"}, 403)
	request("PUT", base+"/password", map[string]string{"currentPassword": "independent-test-password", "newPassword": "replacement-test-password"}, 204)
	request("GET", base+"/access", nil, 403)
	request("PUT", base, map[string]any{"features": map[string]bool{"phone": true}}, 403)
	request("POST", base+"/unlock", map[string]string{"password": "independent-test-password"}, 403)
	request("POST", base+"/unlock", map[string]string{"password": "replacement-test-password"}, 200)
	var currentHash []byte
	if err = pool.QueryRow(ctx, `SELECT password_hash FROM administrators WHERE id=$1`, id).Scan(&currentHash); err != nil || !bytes.Equal(hash, currentHash) {
		t.Fatal("Independent password changed login password")
	}
	if _, err = pool.Exec(ctx, `UPDATE login_sessions SET visibility_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	request("GET", base+"/access", nil, 403)
	request("POST", base+"/unlock", map[string]string{"password": "replacement-test-password"}, 200)
	request("DELETE", base+"/access", nil, 204)
	request("GET", base+"/access", nil, 403)
	for i := 0; i < 5; i++ {
		request("POST", base+"/unlock", map[string]string{"password": "wrong-password"}, 403)
	}
	s = &server{db: pool, origin: "http://test.local", slots: make(chan struct{}, 2)}
	request("POST", base+"/unlock", map[string]string{"password": "replacement-test-password"}, 429)

	// 页面按会话分开下发，业务资源逐个验证。
	s.webRoot = t.TempDir()
	for name, body := range map[string]string{"index.html": "APPLICATION_ONLY", "login.html": "LOGIN_ONLY", "app.js": "PRIVATE_SCRIPT", "login.js": "PUBLIC_SCRIPT"} {
		if err = os.WriteFile(s.webRoot+"/"+name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path          string
		authenticated bool
		status        int
		body          string
	}{
		{"/", false, 200, "LOGIN_ONLY"}, {"/", true, 200, "APPLICATION_ONLY"},
		{"/app.js", false, 401, ""}, {"/app.js", true, 200, "PRIVATE_SCRIPT"},
		{"/login.js", false, 200, "PUBLIC_SCRIPT"}, {"/login.html", true, 303, ""},
		{"/README.md", true, 404, ""}, {"/tests/auth.js", true, 404, ""},
	} {
		r := localRequest("GET", tc.path, nil)
		if tc.authenticated {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: value})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status || (tc.body != "" && w.Body.String() != tc.body) {
			t.Fatalf("web %s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("Private response cached")
		}
	}
	var independentBefore, independentAfter []byte
	pool.QueryRow(ctx, `SELECT password_hash FROM visibility_security`).Scan(&independentBefore)
	request("GET", "/api/administrator", nil, 200)
	request("PATCH", "/api/administrator", map[string]string{"account": "renamed", "currentPassword": "wrong-password"}, 403)
	request("PATCH", "/api/administrator", map[string]string{"account": "renamed", "currentPassword": "login-test-password", "newPassword": "new-login-test-password", "confirmPassword": "new-login-test-password"}, 204)
	request("GET", "/api/session", nil, 401)
	pool.QueryRow(ctx, `SELECT password_hash FROM visibility_security`).Scan(&independentAfter)
	if !bytes.Equal(independentBefore, independentAfter) {
		t.Fatal("Login change affected independent password")
	}
	var username string
	pool.QueryRow(ctx, `SELECT username,password_salt,password_hash,password_iterations FROM administrators WHERE id=$1`, id).Scan(&username, &salt, &hash, new(int))
	if username != "renamed" || !verifyPassword("new-login-test-password", salt, hash, passwordIterations) || verifyPassword("login-test-password", salt, hash, passwordIterations) {
		t.Fatal("Login credentials were not updated")
	}
	s.limits = make(map[string]attempts)
	request("POST", "/api/session", map[string]string{"username": "tester", "password": "login-test-password"}, 401)
	request("POST", "/api/session", map[string]string{"username": "renamed", "password": "new-login-test-password"}, 200)
}

func TestSIPServerVisibilityFeature(t *testing.T) {
	for _, name := range []string{"server", "sip", "sipServer"} {
		if !featureNames[name] {
			t.Fatalf("missing independent visibility feature %s", name)
		}
	}
}
