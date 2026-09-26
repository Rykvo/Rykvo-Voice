package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testSIPNetworkAuthorization(t *testing.T, s *server, cookie, csrf string) {
	previous := s.sipNetwork
	s.sipNetwork = &sipNetworkManager{busy: true}
	defer func() { s.sipNetwork = previous }()
	for _, action := range []string{"connect", "logout"} {
		for _, tc := range []struct {
			origin, token string
			want          int
		}{
			{"http://evil.test", csrf, 403}, {s.origin, "", 403}, {s.origin, csrf, 409},
		} {
			body := `{}`
			if action == "connect" {
				body = `{"address":"https://sip.example.com/api/connect","accessCode":"` + strings.Repeat("A", 43) + `"}`
			}
			r := localRequest("POST", "/api/settings/sip-server/"+action, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CSRF-Token", tc.token)
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("SIP %s: %d %s", action, w.Code, w.Body.String())
			}
		}
	}
}

func TestSIPNetworkSingleOperationAndVerifiedStatus(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m := &sipNetworkManager{call: func(ctx context.Context, input map[string]string) (sipNetworkStatus, error) {
		if input["action"] != "status" {
			close(entered)
			<-release
			return sipNetworkStatus{}, errors.New("SIP_INVALID_CODE")
		}
		return sipNetworkStatus{State: "disconnected"}, nil
	}}
	if !m.start(map[string]string{"action": "connect", "accessCode": "secret"}) {
		t.Fatal("not started")
	}
	<-entered
	if m.start(map[string]string{"action": "disconnect"}) {
		t.Fatal("overlapping mutation")
	}
	state, err := m.status(context.Background())
	if err != nil || state.State != "configuring" || state.Capabilities["calls"] {
		t.Fatal(state, err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		busy := m.busy
		m.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	state, err = m.status(context.Background())
	if err != nil || state.State != "disconnected" || state.Issue != "SIP_INVALID_CODE" {
		t.Fatal(state, err)
	}
}

func TestSIPNetworkAPIInputAndNoSecretResponse(t *testing.T) {
	m := &sipNetworkManager{busy: true}
	s := &server{sipNetwork: m}
	for _, tc := range []struct {
		path, method, body string
		want               int
	}{
		{"/api/settings/sip-server/connect", "GET", "", 405},
		{"/api/settings/sip-server/connect", "POST", `{"address":"http://host/api/connect","accessCode":"bad"}`, 400},
		{"/api/settings/sip-server/connect", "POST", `{"address":"https://host/api/connect","accessCode":"` + strings.Repeat("A", 43) + `","command":"shell"}`, 400},
		{"/api/settings/sip-server/logout", "POST", `{}`, 409},
		{"/api/settings/sip-server", "GET", "", 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.sipNetworkAPI(context.Background(), w, r)
		if w.Code != tc.want || strings.Contains(w.Body.String(), "accessCode") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
