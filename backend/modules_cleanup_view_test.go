package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestModuleWiFiRestartsAfterConfirmedNetworkCleanup(t *testing.T) {
	sample := wifiModuleFixture()
	m := newModuleManager(nil, nil)
	m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
	m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
	w := &moduleWiFi{Enabled: true, ICCID: sample.Reading.ICCID, State: "waiting"}
	m.wifi[1] = w
	var calls atomic.Int32
	connected, finish := make(chan struct{}), make(chan struct{})
	defer func() { close(finish); m.operations.Wait() }()
	m.wifiEngine = recoveryWiFiEngine(func(ctx context.Context, _ func() bool, emit func(string)) error {
		if calls.Add(1) == 1 {
			emit("ims-cleaned")
			emit("radio-restored")
			return errors.New("NETWORK_UNAVAILABLE")
		}
		emit("connected")
		close(connected)
		<-finish
		return context.Canceled
	})
	m.mu.Lock()
	m.startWiFiLocked(1, sample)
	m.mu.Unlock()
	m.operations.Wait()
	m.mu.Lock()
	if w.Issue != "NETWORK_UNAVAILABLE" || w.networkRetry.IsZero() || !m.recoveryUntil[sample.Candidate.Key].IsZero() {
		m.mu.Unlock()
		t.Fatal("confirmed cleanup did not schedule ordinary retry")
	}
	w.networkRetry = time.Now().Add(-time.Second)
	m.startWiFiLocked(1, sample)
	m.mu.Unlock()
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("ordinary scan did not reconnect")
	}
	if got := m.wifiView(1, sample.Reading.ICCID); got["registered"] != true || got["issue"] != "" {
		t.Fatal("registration did not clear failure", got)
	}
}

func TestModuleCleanupFailureVisibleWithoutNotification(t *testing.T) {
	sample := wifiModuleFixture()
	m := newModuleManager(nil, &restartFake{})
	m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
	v := moduleRecord{ID: 1, Endpoint: sample.Candidate.Key, Identity: sample.Candidate.Identity(sample.Reading)}
	m.values[1], m.seen[v.Endpoint] = sample, sample.Candidate
	w := &moduleWiFi{Enabled: true, ICCID: sample.Reading.ICCID, State: "failed", Issue: "WIFI_TUNNEL_CLEANUP_UNCONFIRMED"}
	m.wifi[1] = w
	s := &server{modules: m}
	for _, waiting := range []bool{true, false} {
		if waiting {
			m.recoveryUntil[v.Endpoint] = time.Now().Add(time.Minute)
		} else {
			m.recoveryUntil[v.Endpoint] = time.Now().Add(-time.Second)
		}
		view := s.moduleView(v)
		attachModuleAlerts(view, nil)
		if view["status"] != "error" || view["issue"] != w.Issue || view["restartIssue"] != w.Issue {
			t.Fatal("cleanup failure hidden by grace or missing alert", waiting, view)
		}
	}
	w.Enabled = false
	if view := s.moduleView(v); view["status"] != "online" {
		t.Fatal("disabled Wi-Fi marked as registration failure", view)
	}
	w.Enabled, w.State, w.Issue, w.Registered = true, "connected", "", true
	if view := s.moduleView(v); view["status"] != "online" || view["signal"] != "wifi" {
		t.Fatal("verified registration not restored", view)
	}
	w.Registered, w.State = false, "waiting"
	if view := s.moduleView(v); view["status"] != "online" || view["wifi"].(map[string]any)["retrying"] != true {
		t.Fatal("real retry labelled as final failure", view)
	}
}
