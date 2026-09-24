package main

import (
	"context"
	"rykvo.local/auth/internal/hardware"
	"sync/atomic"
	"testing"
	"time"
)

func TestResumeWiFiIntentPreservesUserChoiceAndCard(t *testing.T) {
	for _, reason := range []string{"recovered", "disabled", "changed_card", "not_ready", "unhealthy", "active", "stopping"} {
		t.Run(reason, func(t *testing.T) {
			r := wifiModuleFixture().Reading
			w := &moduleWiFi{Enabled: true, ICCID: r.ICCID, State: "failed", Issue: "READ_TIMEOUT"}
			switch reason {
			case "disabled":
				w.Enabled = false
			case "changed_card":
				r.ICCID = "89123456789012345679"
			case "not_ready":
				r.SIM = "SIM PIN"
			case "unhealthy":
				r.Warnings = []string{"AT:READ_TIMEOUT"}
			case "active":
				w.running = true
			case "stopping":
				w.State = "stopping"
			}
			before := *w
			got := resumeWiFiIntent(w, r)
			if got != (reason == "recovered") {
				t.Fatal("unexpected resume", got)
			}
			if !got && (w.Enabled != before.Enabled || w.State != before.State || w.Issue != before.Issue || w.ICCID != before.ICCID || w.running != before.running) {
				t.Fatal("blocked recovery changed user intent")
			}
			if got && (!w.Enabled || w.State != "waiting" || w.Issue != "" || resumeWiFiIntent(w, r)) {
				t.Fatal("resume not one shot")
			}
		})
	}
}

type resumeWiFiSource struct {
	calls   atomic.Int32
	started chan struct{}
}

func (s *resumeWiFiSource) WiFi(ctx context.Context, _ hardware.Candidate, _, _ string, emit func(string)) error {
	s.calls.Add(1)
	emit("connected")
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestWiFiNewGenerationAutomaticallyConnectsOnce(t *testing.T) {
	for _, reason := range []string{"new_generation", "unchanged", "disabled", "card_changed", "grace", "job", "unhealthy", "uncertain_same_generation", "uncertain_grace", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			source := &resumeWiFiSource{started: make(chan struct{})}
			m := newModuleManagerWithWiFi(nil, nil, source)
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); m.operations.Wait() }()
			m.ctx = ctx
			sample := wifiModuleFixture()
			old := sample.Candidate
			old.Generation = "old-boot"
			w := &moduleWiFi{Enabled: true, ICCID: sample.Reading.ICCID, State: "failed", Issue: "READ_TIMEOUT", candidate: old}
			switch reason {
			case "unchanged":
				w.candidate = sample.Candidate
			case "disabled":
				w.Enabled = false
			case "card_changed":
				w.ICCID = "89123456789012345679"
			case "grace":
				m.recoveryUntil[sample.Candidate.Key] = time.Now().Add(time.Minute)
			case "job":
				m.jobs[1] = moduleJob{State: "running"}
			case "unhealthy":
				sample.Reading.Warnings = []string{"AT:READ_TIMEOUT"}
			case "uncertain_same_generation":
				w.Issue = "WIFI_RADIO_RESTORE_UNCONFIRMED"
				w.candidate = sample.Candidate
			case "uncertain_grace":
				w.Issue = "WIFI_RADIO_RESTORE_UNCONFIRMED"
				m.recoveryUntil[sample.Candidate.Key] = time.Now().Add(time.Minute)
			case "cancelled":
				cancel()
			}
			m.values[1] = sample
			m.seen[sample.Candidate.Key] = sample.Candidate
			m.wifi[1] = w
			m.mu.Lock()
			m.startWiFiLocked(1, sample)
			m.startWiFiLocked(1, sample)
			running := w.running
			m.mu.Unlock()
			if reason != "new_generation" {
				if running || source.calls.Load() != 0 {
					t.Fatal("unexpected connection")
				}
				return
			}
			select {
			case <-source.started:
			case <-time.After(time.Second):
				t.Fatal("no auto connection")
			}
			if source.calls.Load() != 1 || m.wifiView(1, sample.Reading.ICCID)["registered"] != true {
				t.Fatal("not connected once")
			}
			// A failure after this one attempt must not be restarted by every status poll.
			cancel()
			m.operations.Wait()
			fresh, stop := context.WithCancel(context.Background())
			defer stop()
			m.ctx = fresh
			m.mu.Lock()
			w.State = "failed"
			w.Issue = "WIFI_CONNECTION_FAILED"
			m.startWiFiLocked(1, sample)
			running = w.running
			m.mu.Unlock()
			if running || source.calls.Load() != 1 {
				t.Fatal("same generation retry loop")
			}
		})
	}
}

func TestConfirmedRebootCanResumeAfterCleanupQuarantine(t *testing.T) {
	r := wifiModuleFixture().Reading
	w := &moduleWiFi{Enabled: true, ICCID: r.ICCID, State: "failed", Issue: "WIFI_RADIO_RESTORE_UNCONFIRMED"}
	// Callers prove a recovered device/new generation; startWiFiLocked separately enforces quarantine.
	if !resumeWiFiIntent(w, r) || w.Issue != "" || w.State != "waiting" {
		t.Fatal("verified reboot stuck on old cleanup")
	}
}
