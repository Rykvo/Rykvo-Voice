package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/entitlement"
	"rykvo.local/auth/internal/hardware"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type emergencyFake struct {
	moduleAPNFake
	calls atomic.Int32
	wait  bool
}

func (f *emergencyFake) EmergencyAddress(ctx context.Context, _ hardware.Candidate, _, _ string) (entitlement.Page, error) {
	f.calls.Add(1)
	if f.wait {
		<-ctx.Done()
		return entitlement.Page{}, ctx.Err()
	}
	return entitlement.Page{URL: "https://attdashboard.wireless.att.com/softphone/primary/test", Token: "fixture-only-page-token"}, nil
}
func TestModuleEmergencyGateAndCancellation(t *testing.T) {
	f := &emergencyFake{wait: true}
	m := newModuleManager(nil, f)
	m.ctx = context.Background()
	m.ready = true
	m.lastScan = time.Now()
	sample := wifiModuleFixture()
	v := moduleRecord{ID: 1, Endpoint: sample.Candidate.Key}
	m.values[1] = sample
	m.seen[v.Endpoint] = sample.Candidate
	if _, err := m.openEmergency(context.Background(), v, "other-card"); err == nil {
		t.Fatal("wrong card accepted")
	}
	m.draining = true
	if _, err := m.openEmergency(context.Background(), v, sample.Reading.ICCID); err == nil {
		t.Fatal("maintenance ignored")
	}
	m.draining = false
	gate := m.gate(v.Endpoint)
	gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.openEmergency(ctx, v, sample.Reading.ICCID); err == nil {
		t.Fatal("gate wait ignored cancellation")
	}
	<-gate
	if f.calls.Load() != 0 || len(m.work) != 0 {
		t.Fatal("work leaked")
	}
	ctx, cancel2 := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = m.openEmergency(ctx, v, sample.Reading.ICCID) }()
	deadline := time.After(time.Second)
	for f.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("not admitted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel2()
	<-done
	if len(m.work) != 0 || len(gate) != 0 {
		t.Fatal("cancellation leaked module resources")
	}
}

func TestModuleEmergencyBeforeWiFiRegistration(t *testing.T) {
	for _, state := range []string{"off", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := &emergencyFake{}
			m := newModuleManager(nil, f)
			m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
			sample := wifiModuleFixture()
			sample.Reading.Number, sample.Reading.Registration = "", "denied"
			m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
			m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: state != "off", State: state}
			before := *m.wifi[1]
			page, err := m.openEmergency(context.Background(), moduleRecord{ID: 1, Endpoint: sample.Candidate.Key}, sample.Reading.ICCID)
			if err != nil || page.URL == "" || f.calls.Load() != 1 {
				t.Fatal("address session required phone number or Wi-Fi registration", err)
			}
			after := m.wifi[1]
			if after.Enabled != before.Enabled || after.State != before.State || after.running || len(m.work) != 0 || len(m.gate(sample.Candidate.Key)) != 0 {
				t.Fatal("address lookup changed Wi-Fi intent or retained module resources")
			}
		})
	}
}

func testEmergencyDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	f := &emergencyFake{}
	m := newModuleManager(s.db, f)
	m.ctx = ctx
	m.ready = true
	m.lastScan = time.Now()
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	m.values[v.ID] = sample
	m.seen[v.Endpoint] = sample.Candidate
	m.carrierConfigs[sample.Reading.ICCID] = carrierconfig.Match(carrierconfig.Identity{MCC: "310", MNC: "280", IMSI: "310280123456789", GID1: "20FF"})
	old := s.modules
	s.modules = m
	card, epoch, generation, err := s.emergencyBinding(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.modules = old
		_, _ = s.db.Exec(ctx, "UPDATE modules SET active_card=$2,card_epoch=$3,endpoint_generation=$4 WHERE id=$1", v.ID, card, epoch, generation)
		s.emergency.Lock()
		for _, item := range s.emergency.items {
			s.emergency.remove(item)
		}
		s.emergency.Unlock()
	}()
	_, err = s.db.Exec(ctx, "UPDATE modules SET active_card=$2,card_epoch=10,endpoint_generation=$3 WHERE id=$1", v.ID, sample.Reading.ICCID, sample.Candidate.Generation)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/modules/" + moduleID(v.ID) + "/lines/" + wifiLine(sample.Reading) + "/emergency-address/session"
	request := func(method, path, csrfToken string, want int) map[string]any {
		r := localRequest(method, path, strings.NewReader(""))
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("X-CSRF-Token", csrfToken)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if w.Code == 204 {
			return nil
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("bad JSON")
		}
		return out.Data
	}
	request("POST", base, "", 403)
	if f.calls.Load() != 0 {
		t.Fatal("CSRF reached SIM")
	}
	request("GET", base, csrf, 405)
	first := request("POST", base, csrf, 202)
	id := first["id"].(string)
	second := request("POST", base, csrf, 202)
	if second["id"] != id {
		t.Fatal("duplicate authentication")
	}
	if first["page"] != nil {
		t.Fatal("token in start response")
	}
	var ready map[string]any
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		ready = request("GET", base+"/"+id, csrf, 200)
		if ready["state"] == "ready" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if ready["state"] != "ready" || f.calls.Load() != 1 {
		t.Fatalf("state=%v calls=%d", ready["state"], f.calls.Load())
	}
	if ready["page"].(map[string]any)["token"] != "fixture-only-page-token" {
		t.Fatal("missing carrier page")
	}
	request("GET", base+"/wrong-session", csrf, 404)
	s.emergency.Lock()
	item := s.emergency.items[id]
	owner := item.owner
	item.owner = "another-login"
	s.emergency.Unlock()
	request("GET", base+"/"+id, csrf, 404)
	request("POST", base, csrf, 409)
	s.emergency.Lock()
	item.owner = owner
	s.emergency.Unlock()
	request("DELETE", base+"/"+id, "", 403)
	// A -> B -> A remains invalidated by the persistent card epoch.
	_, err = s.db.Exec(ctx, "UPDATE modules SET card_epoch=card_epoch+2 WHERE id=$1", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	request("GET", base+"/"+id, csrf, 409)
	request("GET", base+"/"+id, csrf, 404)
	next := request("POST", base, csrf, 202)
	request("DELETE", base+"/"+next["id"].(string), csrf, 204)
	request("GET", base+"/"+next["id"].(string), csrf, 404)
	m.mu.Lock()
	delete(m.carrierConfigs, sample.Reading.ICCID)
	m.mu.Unlock()
	request("POST", base, csrf, 409)
}
