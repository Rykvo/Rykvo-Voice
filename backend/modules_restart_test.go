package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/hardware"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type restartFake struct {
	resets, hosts atomic.Int32
	hostError     error
}

func (*restartFake) Discover(context.Context) ([]hardware.Candidate, error) { return nil, nil }
func (*restartFake) Read(context.Context, hardware.Candidate) hardware.Reading {
	return hardware.Reading{}
}
func (f *restartFake) Restart(context.Context, hardware.Candidate) error { f.resets.Add(1); return nil }
func (f *restartFake) RestartHost(context.Context) error                 { f.hosts.Add(1); return f.hostError }

func TestModuleRestartConfirmation(t *testing.T) {
	for _, change := range []string{"new", "same", "wrong-modem", "missing-generation", "unhealthy", "esim"} {
		t.Run(change, func(t *testing.T) {
			s := wifiModuleFixture()
			s.Candidate.Generation = "2:12"
			j := moduleJob{Action: "restart", State: "uncertain", Verification: &moduleVerification{IMEI: s.Reading.IMEI, Generation: "2:11"}}
			switch change {
			case "same":
				s.Candidate.Generation = "2:11"
			case "wrong-modem":
				s.Reading.IMEI = "999999999999999"
			case "missing-generation":
				s.Candidate.Generation = ""
			case "unhealthy":
				s.Reading.Responsive = false
			case "esim":
				j.Action = "enable"
			}
			if j.confirmRestart(s) != (change == "new") {
				t.Fatal("unproven restart result")
			}
		})
	}
}

func testRestartDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &restartFake{}
	m := newModuleManager(s.db, fake)
	m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	sample.Reading.IMEI = strings.TrimPrefix(v.Identity, "imei:")
	m.values[v.ID], m.seen[v.Endpoint] = sample, sample.Candidate
	m.wifi[v.ID] = &moduleWiFi{Enabled: true, State: "off", ICCID: sample.Reading.ICCID}
	previous := s.modules
	s.modules = m
	defer func() {
		cancel()
		m.operations.Wait()
		s.modules = previous
		s.db.Exec(context.Background(), "DELETE FROM module_jobs WHERE action='restart'")
		s.db.Exec(context.Background(), "DELETE FROM restart_requests")
	}()
	call := func(path, body, auth string, want int) {
		t.Helper()
		r := localRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", auth)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	path := "/api/modules/" + moduleID(v.ID) + "/restart"
	body := `{"requestId":"restart-test-single","confirm":true}`
	call(path, body, "wrong", 403)
	call(path, `{"requestId":"restart-test-single","confirm":false}`, csrf, 400)
	call(path, `{"requestId":"restart-test-single","confirm":true,"command":"other"}`, csrf, 400)
	if fake.resets.Load() != 0 {
		t.Fatal("unconfirmed reset")
	}
	m.jobs[v.ID] = moduleJob{State: "running"}
	call(path, body, csrf, 409)
	delete(m.jobs, v.ID)
	call(path, body, csrf, 202)
	m.operations.Wait()
	call(path, body, csrf, 202)
	if fake.resets.Load() != 1 || !m.wifi[v.ID].Enabled {
		t.Fatal("reset replayed or intent lost")
	}
	if m.job(v.ID).State != "uncertain" {
		t.Fatal("ack treated as completion")
	}
	call(path, `{"requestId":"restart-test-second","confirm":true}`, csrf, 409)
	_, err := m.restartModules(ctx, []moduleRecord{v}, "all", "restart-test-single")
	if err == nil || err.Error() != "REQUEST_CONFLICT" {
		t.Fatal("request scope changed")
	}
	// Persisted replay after manager restart does not touch hardware.
	other := newModuleManager(s.db, fake)
	if jobs, err := other.restartModules(ctx, nil, moduleID(v.ID), "restart-test-single"); err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	// A mixed valid/busy batch must reserve nothing and reset nothing.
	delete(m.recoveryUntil, v.Endpoint)
	s.db.Exec(ctx, "DELETE FROM module_jobs WHERE action='restart'")
	bad := v
	bad.ID += 10000
	if _, err = m.restartModules(ctx, []moduleRecord{v, bad}, "all", "restart-test-batch"); err == nil {
		t.Fatal("partial batch accepted")
	}
	var n int
	s.db.QueryRow(ctx, "SELECT count(*) FROM restart_requests WHERE id='restart-test-batch'").Scan(&n)
	if n != 0 || fake.resets.Load() != 1 {
		t.Fatal("partial batch executed")
	}
	m.jobs[v.ID] = moduleJob{State: "running"}
	call("/api/modules/host-restart", `{"requestId":"restart-test-host1","confirm":true}`, csrf, 409)
	delete(m.jobs, v.ID)
	call("/api/modules/host-restart", `{"requestId":"restart-test-host1","confirm":true}`, csrf, 202)
	call("/api/modules/host-restart", `{"requestId":"restart-test-host1","confirm":true}`, csrf, 202)
	if fake.hosts.Load() != 1 || m.ready {
		t.Fatal("host duplicate or work still enabled")
	}
	m.ready = true
	s.db.Exec(ctx, "DELETE FROM restart_requests WHERE scope='host'")
	fake.hostError = errors.New("timeout")
	state, err := m.restartHost(ctx, "restart-test-host2")
	if err != nil || state != "uncertain" {
		t.Fatal(state, err)
	}
	if _, err = m.restartHost(ctx, "restart-test-host2"); err != nil || fake.hosts.Load() != 2 {
		t.Fatal("ambiguous host command replayed")
	}
	b, _ := json.Marshal(m.job(v.ID))
	if strings.Contains(string(b), sample.Reading.IMEI) {
		t.Fatal("private identity leaked")
	}
}
