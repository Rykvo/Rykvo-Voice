package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContactURL(t *testing.T) {
	for _, value := range []string{"", "https://example.com/help?a=1&b=2", "HTTPS://example.com", "http://example.com", "https://t.me/help", "mailto:help@example.com", "tel:+12025550123"} {
		if !validContactURL(value) {
			t.Fatalf("rejected %q", value)
		}
	}
	for _, value := range []string{"javascript:alert(1)", "data:text/html,unsafe", "file:///tmp/test", "//example.com", "https:example.com", "https://", "mailto:", "tel:", "mailto://help@example.com", "https://user:pass@example.com", "https://example.com/ x", "https://example.com/\\evil", "https://example.com/\n", "https://example.com/<script>", "https://example.com/" + strings.Repeat("a", 2048)} {
		if validContactURL(value) {
			t.Fatalf("accepted %q", value)
		}
	}
}

func testContactSettingsDatabase(t *testing.T, s *server, session, csrf string) {
	t.Helper()
	request := func(method, path, body, cookie, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := localRequest(method, path, strings.NewReader(body))
		if strings.HasPrefix(path, "/gly/") {
			r.Host = "panel.example.com"
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		r.Header.Set("Origin", s.origin)
		r.Header.Set("X-CSRF-Token", token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("contact %s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w
	}
	request("GET", "/api/settings/contact", "", "", "", 401)
	request("PUT", "/api/settings/contact", `{"url":"https://example.com","revision":1}`, session, "", 403)
	for _, body := range []string{`{}`, `{"url":null,"revision":1}`, `{"url":"javascript:alert(1)","revision":1}`, `{"url":"https://user:secret@example.com","revision":1}`, `{"url":"https://example.com","revision":0}`, `{"url":"https://example.com","revision":1,"unexpected":true}`} {
		request("PUT", "/api/settings/contact", body, session, csrf, 400)
	}
	request("POST", "/api/settings/contact", `{}`, session, csrf, 405)
	w := request("GET", "/api/settings/contact", "", session, csrf, 200)
	var current struct {
		Data contactSettings `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil || current.Data.Revision != 1 || current.Data.URL != "" {
		t.Fatal("bad initial contact settings", err)
	}
	for _, prefix := range []string{"", "/gly"} {
		w = request("GET", prefix+"/api/public/contact", "", "", "", 200)
		if strings.TrimSpace(w.Body.String()) != `{"data":{"url":""}}` {
			t.Fatal("public settings disclosed private fields", w.Body)
		}
	}
	request("POST", "/api/public/contact", `{}`, "", "", 405)
	request("PUT", "/api/settings/contact", `{"url":" https://t.me/example ","revision":1}`, session, csrf, 200)
	request("PUT", "/api/settings/contact", `{"url":"","revision":1}`, session, csrf, 409)
	w = request("GET", "/gly/api/public/contact", "", "", "", 200)
	if !strings.Contains(w.Body.String(), `"url":"https://t.me/example"`) || strings.Contains(w.Body.String(), "revision") {
		t.Fatal("public contact missing or leaked configuration", w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("contact cached after clear")
	}
	request("PUT", "/api/settings/contact", `{"url":"","revision":2}`, session, csrf, 200)
	w = request("GET", "/api/public/contact", "", "", "", 200)
	if strings.TrimSpace(w.Body.String()) != `{"data":{"url":""}}` {
		t.Fatal("contact not cleared", w.Body)
	}
}
