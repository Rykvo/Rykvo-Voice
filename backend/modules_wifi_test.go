package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/hardware"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type moduleWiFiFake struct {
	started, cleaning, release chan struct{}
	reads                      atomic.Int32
}

func (f *moduleWiFiFake) Discover(context.Context) ([]hardware.Candidate, error) { return nil, nil }
func (f *moduleWiFiFake) Read(context.Context, hardware.Candidate) hardware.Reading {
	f.reads.Add(1)
	return hardware.Reading{}
}
func (f *moduleWiFiFake) WiFi(ctx context.Context, _ hardware.Candidate, _ string, _ string, emit func(string)) error {
	emit("connected")
	close(f.started)
	<-ctx.Done()
	close(f.cleaning)
	<-f.release
	return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
}
func wifiModuleFixture() moduleSample {
	c := recoveryCandidate()
	c.Ports = []hardware.Port{{Path: "/dev/fixture", Interface: 3}}
	return moduleSample{c, hardware.Reading{IMEI: "123456789012345", ICCID: "89123456789012345678", SIM: "READY", Responsive: true, UpdatedAt: time.Now()}}
}
func TestModuleWiFiGateIncludesCleanup(t *testing.T) {
	f := &moduleWiFiFake{started: make(chan struct{}), cleaning: make(chan struct{}), release: make(chan struct{})}
	m := newModuleManager(nil, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	sample := wifiModuleFixture()
	m.values[1] = sample
	m.seen[sample.Candidate.Key] = sample.Candidate
	m.lastScan = time.Now()
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
	m.mu.Lock()
	m.startWiFiLocked(1, sample)
	m.mu.Unlock()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	if m.wifiView(1, sample.Reading.ICCID)["registered"] != true {
		t.Fatal("verified registration missing")
	}
	if m.read(ctx, sample.Candidate).Issue != "OPERATION_ACTIVE" || f.reads.Load() != 0 {
		t.Fatal("reader raced Wi-Fi SIM")
	}
	cancel()
	select {
	case <-f.cleaning:
	case <-time.After(time.Second):
		t.Fatal("cleanup not started")
	}
	gate := m.gate(sample.Candidate.Key)
	select {
	case gate <- struct{}{}:
		<-gate
		t.Fatal("gate released before cleanup")
	default:
	}
	close(f.release)
	m.operations.Wait()
	view := m.wifiView(1, sample.Reading.ICCID)
	if view["registered"] != false || view["issue"] != "WIFI_RADIO_RESTORE_UNCONFIRMED" {
		t.Fatal("cleanup uncertainty hidden", view)
	}
	select {
	case gate <- struct{}{}:
		<-gate
	default:
		t.Fatal("gate leaked")
	}
}
func TestModuleWiFiCardIsolation(t *testing.T) {
	m := newModuleManager(nil, nil)
	sample := wifiModuleFixture()
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, Registered: true, State: "connected"}
	for _, id := range []string{"", "89000000000000000000"} {
		v := m.wifiView(1, id)
		if v["enabled"] != false || v["registered"] != false {
			t.Fatal("old card state leaked")
		}
	}
	physical := wifiLine(sample.Reading)
	if physical == "" {
		t.Fatal("physical SIM lost")
	}
	sample.Reading.ESIM = &hardware.ESIMInfo{EID: "89049032001001234500012345678901", Profiles: []hardware.ESIMProfile{{ICCID: sample.Reading.ICCID, Enabled: true}}}
	if wifiLine(sample.Reading) == physical || wifiLine(sample.Reading) == "" {
		t.Fatal("eSIM identity")
	}
	sample.Reading.ESIM.Profiles[0].Enabled = false
	if wifiLine(sample.Reading) != "" {
		t.Fatal("inactive profile accepted")
	}
}
func TestModuleWiFiNeverStartsForWrongOrBusyCard(t *testing.T) {
	for _, reason := range []string{"card", "job", "unsupported", "notready", "disabled"} {
		f := &moduleWiFiFake{}
		m := newModuleManager(nil, f)
		m.ctx = context.Background()
		sample := wifiModuleFixture()
		w := &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
		m.wifi[1] = w
		switch reason {
		case "card":
			w.ICCID = "89000000000000000000"
		case "job":
			m.jobs[1] = moduleJob{State: "running"}
		case "unsupported":
			sample.Candidate.Ports = nil
		case "notready":
			sample.Reading.SIM = "absent"
		case "disabled":
			w.Enabled = false
		}
		m.mu.Lock()
		m.startWiFiLocked(1, sample)
		m.mu.Unlock()
		if w.running {
			t.Fatal("unexpected start", reason)
		}
	}
}

func testWiFiDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	f := &moduleWiFiFake{started: make(chan struct{}), cleaning: make(chan struct{}), release: make(chan struct{})}
	m := newModuleManager(s.db, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.ready = true
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	m.values[v.ID] = sample
	m.seen[v.Endpoint] = sample.Candidate
	m.lastScan = time.Now()
	old := s.modules
	s.modules = m
	defer func() { s.modules = old }()
	gate := m.gate(v.Endpoint)
	gate <- struct{}{}
	defer func() { cancel(); <-gate; m.operations.Wait() }()
	line := wifiLine(sample.Reading)
	request := func(body, csrfHeader, lineID string, want int) {
		r := localRequest(http.MethodPatch, "/api/modules/"+moduleID(v.ID)+"/lines/"+lineID, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("X-CSRF-Token", csrfHeader)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("Wi-Fi API %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	on := `{"requestId":"wifi-enable-test-request","wifiCalling":true}`
	request(on, "", line, 403)
	request(on, csrf, "line-wrong", 409)
	request(on, csrf, line, 202)
	request(on, csrf, line, 202)
	m.mu.RLock()
	ptr := m.wifi[v.ID]
	m.mu.RUnlock()
	if ptr == nil {
		t.Fatal("runtime missing")
	}
	other := newModuleManager(s.db, nil)
	other.loadWiFi(ctx)
	if other.wifi[v.ID] == nil || !other.wifi[v.ID].Enabled || other.wifi[v.ID].State != "waiting" {
		t.Fatal("desired state not restored independently of registration")
	}
	request(`{"requestId":"wifi-invalid-test-request","wifiCalling":true,"enabled":true}`, csrf, line, 400)
	request(`{"requestId":"wifi-disable-test-request","wifiCalling":false}`, csrf, line, 202)
	var enabled bool
	if e := s.db.QueryRow(ctx, "SELECT enabled FROM module_wifi WHERE module_id=$1", v.ID).Scan(&enabled); e != nil || enabled {
		t.Fatal("off state not persisted", e)
	}
	if f.reads.Load() != 0 {
		t.Fatal("hardware touched while queued")
	}
	if _, e := s.db.Exec(ctx, "DELETE FROM module_wifi WHERE module_id=$1", v.ID); e != nil {
		t.Fatal(e)
	}
}

func TestModuleWiFiKeepsControlWhileConnecting(t *testing.T) {
	m := newModuleManager(nil, nil)
	sample := wifiModuleFixture()
	sample.Reading.UpdatedAt = time.Now().Add(-3 * time.Minute)
	sample.Reading.Registration, sample.Reading.Operator = "registered", "fixture"
	m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
	m.lastScan = time.Now()
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, running: true, State: "connecting"}
	v := moduleRecord{ID: 1, Endpoint: sample.Candidate.Key}
	r, present, issue := m.state(v)
	if !present || issue != "" || wifiLine(r) == "" || r.Operator != "" || r.Registration != "unknown" {
		t.Fatal("connection hid controls or reused cellular state", issue)
	}
	m.wifi[1].running = false
	if _, _, issue = m.state(v); issue != "STATE_STALE" {
		t.Fatal("ordinary stale data accepted")
	}
}
