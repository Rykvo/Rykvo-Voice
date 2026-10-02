package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostnameValidation(t *testing.T) {
	for _, value := range []string{"", "-abc", "abc-", "a.b", "localhost", "LOCALHOST", "LocalHost", "123", "主机", "İ01", "x\n", "a;b", strings.Repeat("a", 64)} {
		if validHostname(value) {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"rykvo-01", "a", "A", "A01", "a01", "Host-01", "1-host", strings.Repeat("a", 63), strings.Repeat("A", 63)} {
		if !validHostname(value) {
			t.Fatalf("rejected %q", value)
		}
	}
}

func TestHostnameAPI(t *testing.T) {
	for _, tc := range []struct {
		method, body, errorCode string
		status                  int
	}{
		{"GET", "", "", 200},
		{"PUT", `{"hostname":" RYKVO-02 ","expected":"rykvo"}`, "", 200},
		{"PUT", `{"hostname":"a;b","expected":"rykvo"}`, "", 400},
		{"PUT", `{"hostname":"valid"}`, "", 400},
		{"PUT", `{"hostname":"valid","expected":"old","extra":true}`, "", 400},
		{"POST", `{}`, "", 405},
		{"GET", "", "HOSTNAME_BUSY", 409},
		{"GET", "", "HOSTNAME_CONFLICT", 409},
		{"GET", "", "HOST_NETWORK_UNSUPPORTED", 503},
		{"GET", "", "private error /etc/something", 503},
	} {
		calls := 0
		s := &server{hostnameCall: func(_ context.Context, request map[string]string) (hostnameStatus, error) {
			calls++
			if tc.errorCode != "" {
				return hostnameStatus{}, errors.New(tc.errorCode)
			}
			if tc.method == "PUT" && (request["hostname"] != "RYKVO-02" || request["expected"] != "rykvo") {
				t.Fatal(request)
			}
			return hostnameStatus{Hostname: "RYKVO-02", Editable: true}, nil
		}}
		r := httptest.NewRequest(tc.method, "/api/settings/hostname", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.hostnameAPI(context.Background(), w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
		if (tc.status == 400 || tc.status == 405) && calls != 0 {
			t.Fatal("invalid input reached helper")
		}
		if strings.Contains(w.Body.String(), "private error") {
			t.Fatal("internal error leaked")
		}
		if tc.status == 200 && !strings.Contains(w.Body.String(), `"hostname":"RYKVO-02"`) {
			t.Fatal("hostname spelling changed", w.Body)
		}
	}
}

func TestHostnameCaseOnlyChange(t *testing.T) {
	s := &server{hostnameCall: func(_ context.Context, input map[string]string) (hostnameStatus, error) {
		if input["hostname"] != "A01" || input["expected"] != "a01" {
			t.Fatal(input)
		}
		return hostnameStatus{Hostname: input["hostname"], Editable: true}, nil
	}}
	r := httptest.NewRequest("PUT", "/api/settings/hostname", strings.NewReader(`{"hostname":"A01","expected":"a01"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.hostnameAPI(context.Background(), w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hostname":"A01"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func testHostnameAuthorization(t *testing.T, s *server, cookie, csrf string) {
	previous := s.hostnameCall
	defer func() { s.hostnameCall = previous }()
	calls := 0
	s.hostnameCall = func(context.Context, map[string]string) (hostnameStatus, error) {
		calls++
		return hostnameStatus{Hostname: "rykvo-02", Editable: true}, nil
	}
	for _, tc := range []struct {
		origin, token string
		want          int
	}{
		{"http://evil.test", csrf, 403}, {s.origin, "", 403}, {s.origin, csrf, 200},
	} {
		r := localRequest("PUT", "/api/settings/hostname", strings.NewReader(`{"hostname":"rykvo-02","expected":"rykvo"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CSRF-Token", tc.token)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("hostname auth: %d %s", w.Code, w.Body)
		}
	}
	if calls != 1 {
		t.Fatal("unauthorized mutation reached helper", calls)
	}
}
