package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRoamingWiFiInterlockAndCardBinding(t *testing.T) {
	m, w, sample, v := phoneFixture()
	m.ready = true
	m.roamingReady = true
	line := wifiLine(sample.Reading)
	for _, state := range []struct {
		enabled, running bool
		state            string
	}{{true, true, "connected"}, {true, false, "failed"}, {false, true, "stopping"}} {
		w.Enabled, w.running, w.State = state.enabled, state.running, state.state
		if _, err := m.roamingCard(v, line); err == nil || err.Error() != "WIFI_CALLING_ACTIVE" {
			t.Fatalf("Wi-Fi interlock absent: %v", err)
		}
	}
	w.Enabled, w.running, w.State = false, false, "off"
	if card, err := m.roamingCard(v, line); err != nil || card != sample.Reading.ICCID {
		t.Fatalf("closed Wi-Fi not editable: %v", err)
	}
	if _, err := m.roamingCard(v, "another-card"); err == nil {
		t.Fatal("accepted wrong card")
	}
	m.roaming[sample.Reading.ICCID] = true
	w.Enabled = true
	value, _ := m.roamingView(sample.Reading.ICCID)
	if !value {
		t.Fatal("Wi-Fi cleared saved preference")
	}
	m.lastScan = time.Now().Add(-time.Minute)
	if _, err := m.roamingCard(v, line); err == nil {
		t.Fatal("accepted stale identity")
	}
}

func testRoamingDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	m := newModuleManager(s.db, nil)
	m.ready = true
	m.lastScan = time.Now()
	m.loadRoaming(ctx)
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	m.values[v.ID] = sample
	m.seen[v.Endpoint] = sample.Candidate
	old := s.modules
	s.modules = m
	defer func() { s.modules = old }()
	path := "/api/modules/" + moduleID(v.ID) + "/lines/" + wifiLine(sample.Reading)
	request := func(body, header string, want int) {
		t.Helper()
		r := localRequest("PATCH", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("X-CSRF-Token", header)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("roaming: %d %s", w.Code, w.Body.String())
		}
	}
	on := `{"roaming":true,"requestId":"12345678-1234-4234-8234-123456789001"}`
	off := `{"roaming":false,"requestId":"12345678-1234-4234-8234-123456789002"}`
	request(on, "", 403)
	request(on, csrf, 200)
	if value, _ := m.roamingView(sample.Reading.ICCID); !value {
		t.Fatal("not saved")
	}
	fresh := newModuleManager(s.db, nil)
	fresh.loadRoaming(ctx)
	if value, ready := fresh.roamingView(sample.Reading.ICCID); !ready || !value {
		t.Fatal("restart lost roaming")
	}
	request(off, csrf, 200)
	request(on, csrf, 200)
	if value, _ := m.roamingView(sample.Reading.ICCID); value {
		t.Fatal("replayed old request overwrote latest choice")
	}
	request(strings.Replace(on, `"roaming":true`, `"roaming":false`, 1), csrf, 409)
	m.wifi[v.ID] = &moduleWiFi{Enabled: true, State: "connected", running: true}
	request(`{"roaming":true,"requestId":"12345678-1234-4234-8234-123456789003"}`, csrf, 409)
	if value, _ := m.roamingView(sample.Reading.ICCID); value {
		t.Fatal("Wi-Fi guard mutated preference")
	}
	request(`{"roaming":true,"wifiCalling":false,"requestId":"12345678-1234-4234-8234-123456789004"}`, csrf, 400)
}
