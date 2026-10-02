package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeveloperBusyBeforeDatabase(t *testing.T) {
	s := &server{}
	for range 16 {
		release := s.developerWork.enter(false)
		if release == nil {
			t.Fatal("capacity")
		}
		defer release()
	}
	r := httptest.NewRequest("GET", "http://localhost/api/v1/modules", nil)
	r.Header.Set("X-API-Key", "fixture-key-123456")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), "API_BUSY") {
		t.Fatal(w.Code, w.Body.String())
	}
	// No database was supplied: overload must return before authentication I/O.
}

func TestDeveloperRateRetryHeader(t *testing.T) {
	w := httptest.NewRecorder()
	developerReject(w, http.StatusTooManyRequests, "API_RATE_LIMIT", 1200*time.Millisecond)
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatal(w.Code, w.Header())
	}
}
