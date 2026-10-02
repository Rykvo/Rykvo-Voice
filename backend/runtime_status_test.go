package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkerHealthTracksFailuresAndRecovery(t *testing.T) {
	var state workerStatus
	if state.snapshot(time.Minute).Ready {
		t.Fatal("unstarted worker reported ready")
	}
	state.update("fixture", "")
	if !state.snapshot(time.Minute).Ready {
		t.Fatal("healthy worker not ready")
	}
	state.update("fixture", "DATABASE_UNAVAILABLE")
	if v := state.snapshot(time.Minute); v.Ready || v.Issue != "DATABASE_UNAVAILABLE" {
		t.Fatal(v)
	}
	state.update("fixture", "")
	if !state.snapshot(time.Minute).Ready {
		t.Fatal("worker did not recover")
	}
	state.mu.Lock()
	state.state.CheckedAt = time.Now().Add(-2 * time.Minute)
	state.mu.Unlock()
	if state.snapshot(time.Minute).Ready {
		t.Fatal("stale worker reported ready")
	}
}
func TestBuildVersionUsesRunningBinary(t *testing.T) {
	before := buildVersion
	buildVersion = "1.2.3"
	defer func() { buildVersion = before }()
	w := httptest.NewRecorder()
	versionAPI(w, httptest.NewRequest("GET", "/api/version", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"1.2.3"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	versionAPI(w, httptest.NewRequest("POST", "/api/version", nil))
	if w.Code != 405 {
		t.Fatal("version mutation allowed")
	}
}
