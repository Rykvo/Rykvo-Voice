package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func TestWiFiOwnsESIMSnapshot(t *testing.T) {
	for _, state := range []string{"connected", "connecting"} {
		for _, change := range []string{"none", "stopped", "disabled", "card", "endpoint", "generation", "unknown-generation", "unresponsive", "read-error"} {
			t.Run(state+"/"+change, func(t *testing.T) {
				sample := wifiModuleFixture()
				sample.Reading.UpdatedAt = time.Now().Add(-time.Hour)
				w := &moduleWiFi{Enabled: true, running: true, State: state, ICCID: sample.Reading.ICCID, candidate: sample.Candidate}
				switch change {
				case "stopped":
					w.running = false
				case "disabled":
					w.Enabled = false
				case "card":
					w.ICCID = "different-card"
				case "endpoint":
					w.candidate.Key = "usb:other"
				case "generation":
					w.candidate.Generation = "changed"
				case "unknown-generation":
					w.candidate.Generation, sample.Candidate.Generation = "", ""
				case "unresponsive":
					sample.Reading.Responsive = false
				case "read-error":
					sample.Reading.Issue = "READ_TIMEOUT"
				}
				if got := wifiOwnsESIMSnapshot(w, sample); got != (change == "none") {
					t.Fatalf("handoff permitted = %v", got)
				}
				if wifiOwnsESIMSnapshot(nil, sample) {
					t.Fatal("missing owner accepted")
				}
			})
		}
	}
}

// Hold the device gate until cancellation: these API tests never call a card helper.
func testESIMWiFiHandoffDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	for _, action := range []string{"download", "delete"} {
		t.Run("esim-wifi-handoff-"+action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			m := newModuleManager(s.db, nil)
			m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
			sample := wifiModuleFixture()
			sample.Candidate.Key = v.Endpoint
			sample.Reading.UpdatedAt = time.Now().Add(-time.Hour)
			eid := "89049032001001234500012345678901"
			target := "89123456789012345679"
			sample.Reading.ESIM = &hardware.ESIMInfo{EID: eid, Profiles: []hardware.ESIMProfile{
				{ICCID: sample.Reading.ICCID, Enabled: true}, {ICCID: target, CanDelete: true},
			}}
			m.values[v.ID], m.seen[v.Endpoint] = sample, sample.Candidate
			cancelled := 0
			w := &moduleWiFi{Enabled: true, running: true, ICCID: sample.Reading.ICCID, candidate: sample.Candidate, State: "connected", cancel: func() { cancelled++ }}
			m.wifi[v.ID] = w
			gate := m.gate(v.Endpoint)
			gate <- struct{}{}
			previous := s.modules
			s.modules = m
			defer func() {
				cancel()
				m.operations.Wait()
				<-gate
				s.modules = previous
				_, _ = s.db.Exec(context.Background(), "DELETE FROM module_jobs WHERE id=$1", "esim-wifi-handoff-"+action)
			}()
			method, path := http.MethodPost, "/api/modules/"+moduleID(v.ID)+"/esim"
			if action == "delete" {
				method, path = http.MethodDelete, "/api/modules/"+moduleID(v.ID)+"/lines/"+hardware.ProfileID(eid, target)
			}
			request := func(card, csrfValue string, want int) {
				t.Helper()
				input := map[string]string{"requestId": "esim-wifi-handoff-" + action, "eid": card}
				if action == "download" {
					input["activation"] = "LPA:1$carrier.example$fixture-only"
				}
				body, _ := json.Marshal(input)
				r := localRequest(method, path, strings.NewReader(string(body)))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", "http://test.local")
				r.Header.Set("X-CSRF-Token", csrfValue)
				r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
				response := httptest.NewRecorder()
				s.ServeHTTP(response, r)
				if response.Code != want {
					t.Fatalf("HTTP %d, want %d: %s", response.Code, want, response.Body.String())
				}
			}
			request(eid, "", 403)
			wrongCardStatus := 409
			if action == "delete" {
				wrongCardStatus = 400 // The target profile ID is scoped to the original EID.
			}
			request("89049032001001234500012345678999", csrf, wrongCardStatus)
			w.running = false
			if _, err := m.startJob(ctx, v, hardware.ESIMRequest{Action: action, EID: eid, ICCID: target}, "esim-wifi-handoff-"+action); err == nil || err.Error() != "DEVICE_CHANGED" {
				t.Fatalf("ordinary stale snapshot accepted: %v", err)
			}
			w.running = true
			if cancelled != 0 {
				t.Fatal("rejected request stopped Wi-Fi")
			}
			request(eid, csrf, 202)
			request(eid, csrf, 202)
			job := m.job(v.ID)
			if job.State != "queued" || job.Stage != "waiting" || job.Action != action || cancelled != 1 {
				t.Fatalf("handoff/progress/idempotency: %+v cancelled=%d", job, cancelled)
			}
			var count int
			if err := s.db.QueryRow(ctx, "SELECT count(*) FROM module_jobs WHERE id=$1", job.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("job count=%d err=%v", count, err)
			}
		})
	}
}
