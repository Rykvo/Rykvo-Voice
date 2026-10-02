package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func TestModuleWiFiDiagnosticLifecycle(t *testing.T) {
	for _, outcome := range []string{"failed", "recovered", "off"} {
		t.Run(outcome, func(t *testing.T) {
			sample := wifiModuleFixture()
			m := newModuleManagerWithWiFi(nil, &moduleWiFiFake{}, nil)
			m.ctx, m.ready = context.Background(), true
			m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
			m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
			m.wifiEngine = moduleWiFiEngineFunc(func(ctx context.Context, _ hardware.Candidate, _, _ string, emit func(string)) error {
				emit("diagnostic:ims-refresh-503")
				emit("diagnostic:network-retry-scheduled")
				if got := m.wifiView(1, sample.Reading.ICCID)["diagnostic"]; got != "ims-refresh-503" {
					t.Errorf("retry notice replaced failure: %v", got)
				}
				if outcome == "recovered" {
					emit("connected")
					if got := m.wifiView(1, sample.Reading.ICCID)["diagnostic"]; got != "" {
						t.Errorf("recovery retained error: %v", got)
					}
				}
				if outcome == "failed" {
					return errors.New("WIFI_CONNECTION_FAILED")
				}
				return nil
			})
			m.mu.Lock()
			m.startWiFiLocked(1, sample)
			m.mu.Unlock()
			m.operations.Wait()
			want := ""
			if outcome == "failed" {
				want = "ims-refresh-503"
			}
			if got := m.wifiView(1, sample.Reading.ICCID)["diagnostic"]; got != want {
				t.Fatalf("diagnostic %v, want %v", got, want)
			}
		})
	}
}

// This engine deliberately has no Discover or Read methods.
type moduleWiFiEngineFunc func(context.Context, hardware.Candidate, string, string, func(string)) error

func (f moduleWiFiEngineFunc) WiFi(ctx context.Context, c hardware.Candidate, identity, iccid string, emit func(string)) error {
	return f(ctx, c, identity, iccid, emit)
}

func TestModuleWiFiIndependentEngine(t *testing.T) {
	reader := &moduleWiFiFake{}
	engine := &moduleWiFiFake{started: make(chan struct{}), cleaning: make(chan struct{}), release: make(chan struct{})}
	sample := wifiModuleFixture()
	called := make(chan moduleSample, 1)
	bridge := moduleWiFiEngineFunc(func(ctx context.Context, c hardware.Candidate, identity, iccid string, emit func(string)) error {
		called <- moduleSample{Candidate: c, Reading: hardware.Reading{ICCID: iccid, IMEI: identity}}
		return engine.WiFi(ctx, c, identity, iccid, emit)
	})
	m := newModuleManagerWithWiFi(nil, reader, bridge)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		close(engine.release)
		m.operations.Wait()
	})
	m.ctx = ctx
	m.ready = true
	m.values[1] = sample
	m.seen[sample.Candidate.Key] = sample.Candidate
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
	m.mu.Lock()
	m.startWiFiLocked(1, sample)
	m.startWiFiLocked(1, sample) // Repeated enable must not create a second session.
	m.mu.Unlock()
	select {
	case <-engine.started:
	case <-time.After(time.Second):
		t.Fatal("independent engine did not start")
	}
	got := <-called
	if got.Candidate.Key != sample.Candidate.Key || !sameEndpoint(got.Candidate, sample.Candidate) || got.Reading.ICCID != sample.Reading.ICCID || got.Reading.IMEI != sample.Candidate.Identity(sample.Reading) {
		t.Fatal("bridge lost device/card binding")
	}
	if m.wifiView(1, sample.Reading.ICCID)["registered"] != true {
		t.Fatal("engine evidence not reflected in existing UI state")
	}
	if m.read(ctx, sample.Candidate).Issue != "OPERATION_ACTIVE" || reader.reads.Load() != 0 {
		t.Fatal("reader raced independent engine")
	}
	cancel()
	select {
	case <-engine.cleaning:
	case <-time.After(time.Second):
		t.Fatal("engine cancellation not delivered")
	}
	gate := m.gate(sample.Candidate.Key)
	select {
	case gate <- struct{}{}:
		<-gate
		t.Fatal("gate released before independent engine cleanup")
	default:
	}
}

func TestModuleWiFiExplicitNilEngineDoesNotFallBack(t *testing.T) {
	reader := &moduleWiFiFake{}
	m := newModuleManagerWithWiFi(nil, reader, nil)
	m.ctx = context.Background()
	m.ready = true
	sample := wifiModuleFixture()
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
	m.startWiFiLocked(1, sample)
	if m.wifi[1].running {
		t.Fatal("disabled engine silently fell back to old protocol")
	}
	if err := m.setWiFi(context.Background(), moduleRecord{}, "", "", true); err == nil || err.Error() != "WIFI_MODEM_UNSUPPORTED" {
		t.Fatal("API accepted enable without engine", err)
	}
}

func TestModuleWiFiLegacyConstructorKeepsEngine(t *testing.T) {
	reader := &moduleWiFiFake{}
	if newModuleManager(nil, reader).wifiEngine != reader {
		t.Fatal("default engine changed before replacement is ready")
	}
}
