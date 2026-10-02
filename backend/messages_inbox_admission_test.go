package main

import (
	"context"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/vocat/vowifi"
)

type inboxAdmissionFake struct {
	restartFake
	started chan struct{}
}

func (*inboxAdmissionFake) SendCellularSMS(context.Context, hardware.Candidate, string, string, string, string) (vowifi.SMSSubmitResult, error) {
	panic("inbox poll must not send")
}

func (f *inboxAdmissionFake) ReadCellularSMS(ctx context.Context, _ hardware.Candidate, _, _ string, _ func(context.Context, hardware.SMSDelivery) error) error {
	close(f.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestCellularInboxDoesNotDisableRestart(t *testing.T) {
	f := &inboxAdmissionFake{started: make(chan struct{})}
	m := newModuleManager(nil, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
	sample := wifiModuleFixture()
	sample.Reading.Registration = "home"
	m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
	v := moduleRecord{ID: 1, Endpoint: sample.Candidate.Key, Identity: sample.Candidate.Identity(sample.Reading)}
	done := make(chan struct{})
	go func() { defer close(done); m.pollCellularInbox(ctx, 1) }()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	m.mu.RLock()
	restartable := m.restartableLocked(v)
	m.mu.RUnlock()
	if !restartable {
		t.Error("background inbox read disabled restart")
	}
	gate := m.gate(sample.Candidate.Key)
	select {
	case gate <- struct{}{}:
		<-gate
		t.Error("hardware was not protected during inbox read")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not release on cancellation")
	}
	select {
	case gate <- struct{}{}:
		<-gate
	default:
		t.Fatal("poll leaked the hardware gate")
	}
}

func TestCellularInboxHonorsMaintenanceAndControl(t *testing.T) {
	for _, state := range []string{"draining", "control", "job", "recovery", "wifi", "changed", "stale"} {
		t.Run(state, func(t *testing.T) {
			f := &inboxAdmissionFake{started: make(chan struct{})}
			m := newModuleManager(nil, f)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
			sample := wifiModuleFixture()
			sample.Reading.Registration = "home"
			key := sample.Candidate.Key
			m.values[1], m.seen[key] = sample, sample.Candidate
			switch state {
			case "draining":
				m.draining = true
			case "control":
				m.controlPending[key] = true
			case "job":
				m.jobs[1] = moduleJob{State: "queued"}
			case "recovery":
				m.recoveryUntil[key] = time.Now().Add(time.Minute)
			case "wifi":
				m.wifi[1] = &moduleWiFi{Enabled: true}
			case "changed":
				c := sample.Candidate
				c.Generation = "replacement"
				m.seen[key] = c
			case "stale":
				m.lastScan = time.Now().Add(-time.Minute)
			}
			m.pollCellularInbox(ctx, 1)
			select {
			case <-f.started:
				t.Fatal("poll ignored admission guard")
			default:
			}
		})
	}
}
