package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

type testWiFiVoiceAdapter struct{ ready bool }

func (*testWiFiVoiceAdapter) WiFi(context.Context, hardware.Candidate, string, string, func(string)) error {
	return nil
}
func (a *testWiFiVoiceAdapter) WiFiVoiceReady(hardware.Candidate, string) bool { return a.ready }
func (*testWiFiVoiceAdapter) OpenWiFiCall(context.Context, hardware.Candidate, string) (*hardware.WiFiCall, error) {
	return nil, errors.New("test adapter")
}

func TestModuleVoiceSelectionDoesNotFallBackFromWiFi(t *testing.T) {
	f := &testWiFiVoiceAdapter{ready: true}
	m := newModuleManagerWithWiFi(nil, nil, f)
	m.ctx = context.Background()
	m.lastScan = time.Now()
	sample := wifiModuleFixture()
	sample.Candidate.Audio = "/dev/fixture-audio"
	sample.Reading.Registration = "home"
	m.values[1] = sample
	m.seen[sample.Candidate.Key] = sample.Candidate
	w := &moduleWiFi{Enabled: true, running: true, Registered: true, State: "connected", RadioOff: true, ICCID: sample.Reading.ICCID, candidate: sample.Candidate}
	m.wifi[1] = w
	assertPath := func(want string) {
		t.Helper()
		got := m.moduleVoiceSamples()
		s, ok := got["module-01"]
		if want == "none" {
			if len(got) != 0 {
				t.Fatal("unexpected fallback", got)
			}
			return
		}
		if !ok || s.wifi != (want == "wifi") {
			t.Fatal("wrong adapter", want, got)
		}
	}
	assertPath("wifi")
	f.ready = false
	assertPath("none")
	f.ready = true
	w.Registered = false
	assertPath("none")
	w.Registered = true
	w.RadioOff = false
	assertPath("none")
	w.RadioOff = true
	w.State = "reconnecting"
	assertPath("none")
	w.State = "connected"
	w.ICCID = "other"
	assertPath("none")
	w.ICCID = sample.Reading.ICCID
	w.candidate.Generation = "replacement"
	assertPath("none")
	w.candidate = sample.Candidate
	w.Enabled = false
	assertPath("none")
	w.running = false
	assertPath("cellular")
	m.lastScan = time.Now().Add(-time.Minute)
	assertPath("none")
}
