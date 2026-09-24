package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	salt, hash, err := newPassword("unit-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword("unit-test-password", salt, hash, passwordIterations) {
		t.Fatal("Valid password rejected")
	}
	if verifyPassword("wrong-password", salt, hash, passwordIterations) {
		t.Fatal("Wrong password accepted")
	}
	otherSalt, otherHash, _ := newPassword("unit-test-password")
	if string(salt) == string(otherSalt) || string(hash) == string(otherHash) {
		t.Fatal("Salt is not unique")
	}
	if verifyPassword("unit-test-password", salt, hash, 1) {
		t.Fatal("Invalid cost accepted")
	}
}

func TestSessionCookie(t *testing.T) {
	s := &server{secure: true}
	response := httptest.NewRecorder()
	value := token()
	s.cookie(response, value, time.Now().Add(sessionTTL))
	cookie := response.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 43200 || cookie.Value != value {
		t.Fatal("Invalid cookie attributes")
	}
	if len(tokenHash(value)) != 32 || value == token() {
		t.Fatal("Invalid token")
	}
	response = httptest.NewRecorder()
	s.cookie(response, "", time.Unix(0, 0))
	if response.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("Logout did not expire cookie")
	}
}

func TestUnauthenticatedAndOrigin(t *testing.T) {
	s := &server{origin: "http://example.test", limits: map[string]attempts{}, slots: make(chan struct{}, 2)}
	for _, path := range []string{"/api/session", "/api/modules", "/api/tunnel"} {
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, localRequest("GET", path, nil))
		if recorder.Code != 401 {
			t.Fatal("Unauthenticated request allowed")
		}
	}
	for _, origin := range []string{"", "http://evil.test", "null"} {
		request := localRequest("POST", "/api/session", strings.NewReader(`{}`))
		request.Header.Set("Origin", origin)
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, request)
		if recorder.Code != 403 {
			t.Fatal("Foreign login origin accepted")
		}
	}
}

func TestRateLimit(t *testing.T) {
	s := &server{limits: map[string]attempts{}}
	for i := 0; i < 5; i++ {
		if !s.allow("ip") {
			t.Fatal("Premature rate limit")
		}
	}
	if s.allow("ip") {
		t.Fatal("Rate limit exceeded")
	}
	if !s.allow("other-ip") {
		t.Fatal("Unrelated IP blocked")
	}
	s.limits["ip"] = attempts{5, time.Now().Add(-time.Second)}
	if !s.allow("ip") {
		t.Fatal("Expired rate limit did not reset")
	}
}

func TestLoginInput(t *testing.T) {
	for _, body := range []string{`{}`, `{"username":"a","password":"b","unknown":true}`, `{"username":"a","password":"b"}{}`, strings.Repeat("a", 5000)} {
		s := &server{origin: "http://example.test", limits: map[string]attempts{}, slots: make(chan struct{}, 2)}
		r := localRequest("POST", "/api/session", strings.NewReader(body))
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("Invalid input status %d", w.Code)
		}
	}
}
func TestLocalOrigin(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1", "http://127.0.0.1:80", "http://localhost", "http://[::1]"} {
		if !localOrigin(origin) {
			t.Fatalf("valid local origin rejected: %s", origin)
		}
	}
	for _, origin := range []string{"", "null", "http://evil.test", "http://127.0.0.1.evil", "http://127.0.0.1:8080", "http://user@127.0.0.1", "http://127.0.0.1/", "http://0.0.0.0", "http://192.0.2.123", "http://127.0.0.1?x", "https://localhost"} {
		if localOrigin(origin) {
			t.Fatalf("foreign origin accepted: %s", origin)
		}
	}
}
