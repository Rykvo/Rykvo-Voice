package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

type recoveryWiFiEngine func(context.Context, func() bool, func(string)) error

func (f recoveryWiFiEngine) WiFi(ctx context.Context, _ hardware.Candidate, _, _ string, emit func(string)) error {
	return f(ctx, func() bool { return false }, emit)
}
func (f recoveryWiFiEngine) WiFiWithPolicy(ctx context.Context, _ hardware.Candidate, _, _ string, restore func() bool, emit func(string)) error {
	return f(ctx, restore, emit)
}

func TestWiFiFullRecoveryLifecycle(t *testing.T) {
	for _, outcome := range []string{"recovered", "failed", "cleanup-unconfirmed", "user-off"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sample := wifiModuleFixture()
			m := newModuleManager(nil, nil)
			m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
			m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
			w := &moduleWiFi{Enabled: true, ICCID: sample.Reading.ICCID, State: "waiting"}
			w.setRegistered(false, time.Now().Add(-10*time.Minute))
			m.wifi[1], m.wifiRetryLimit = w, 3
			cleaning, finish := make(chan struct{}), make(chan struct{})
			connected, disconnect := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			m.wifiEngine = recoveryWiFiEngine(func(ctx context.Context, restore func() bool, emit func(string)) error {
				if calls.Add(1) == 1 {
					for i := 1; i <= 3; i++ {
						emit("reconnecting")
						emit("diagnostic:ims-reset")
						emit("retry-failed")
						if i < 3 && ctx.Err() != nil {
							t.Error("reset before failure threshold")
						}
					}
					if ctx.Err() == nil || !restore() {
						t.Error("full recovery did not request cellular restoration")
					}
					close(cleaning)
					<-finish
					if outcome == "cleanup-unconfirmed" {
						return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
					}
					emit("radio-restored")
					return context.Canceled
				}
				if outcome == "failed" {
					emit("diagnostic:ims-reset")
					emit("retry-failed")
					return errors.New("WIFI_CONNECTION_FAILED")
				}
				emit("connected")
				close(connected)
				<-disconnect
				return context.Canceled
			})
			m.mu.Lock()
			m.startWiFiLocked(1, sample)
			m.mu.Unlock()
			select {
			case <-cleaning:
			case <-time.After(2 * time.Second):
				t.Fatal("reset was not started")
			}
			if got := m.wifiAlertDelivery(1, time.Now()); got != "pending" {
				t.Fatal("notification before cleanup", got)
			}
			gate := m.gate(sample.Candidate.Key)
			select {
			case gate <- struct{}{}:
				<-gate
				t.Error("gate released before cleanup acknowledgement")
			default:
			}
			if outcome == "user-off" {
				m.mu.Lock()
				w.Enabled, w.recovery = false, wifiRecovery{}
				m.mu.Unlock()
			}
			close(finish)
			m.operations.Wait()
			if outcome == "cleanup-unconfirmed" || outcome == "user-off" {
				m.recoverWiFi(3, time.Now())
				m.mu.Lock()
				m.startWiFiLocked(1, sample)
				m.mu.Unlock()
				if calls.Load() != 1 || w.State == "waiting" || w.running {
					t.Fatal("unsafe or disabled connection restarted")
				}
				return
			}
			if !w.Enabled || w.State != "waiting" || !w.recovery.Used || w.recovery.Stopping {
				t.Fatal("cleanup did not preserve enabled intent", w)
			}
			m.mu.Lock()
			m.startWiFiLocked(1, sample)
			m.mu.Unlock()
			if calls.Load() != 1 || m.wifiAlertDelivery(1, time.Now()) != "pending" {
				t.Fatal("RF settling period bypassed")
			}
			m.mu.Lock()
			w.refreshUntil = time.Time{}
			m.startWiFiLocked(1, sample)
			m.mu.Unlock()
			if outcome == "failed" {
				m.operations.Wait()
				m.recoverWiFi(3, time.Now())
				if calls.Load() != 2 || m.wifiAlertDelivery(1, time.Now()) != "sending" {
					t.Fatal("failed verification did not release notification or reset repeated")
				}
			} else {
				select {
				case <-connected:
				case <-time.After(2 * time.Second):
					t.Fatal("connection was not reopened")
				}
				m.mu.RLock()
				cleared := w.recovery == (wifiRecovery{})
				m.mu.RUnlock()
				if !cleared || m.wifiAlertDelivery(1, time.Now()) != "cancelled" {
					t.Error("recovery did not clear failures and cancel notification")
				}
				close(disconnect)
				m.operations.Wait()
			}
		})
	}
}

func TestWiFiFullRecoveryAdmission(t *testing.T) {
	for _, reason := range []string{"busy", "job", "settings-write", "network-change", "stale", "different-card", "disabled", "registered", "quarantine", "throttled", "limit-zero", "draining"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Now()
			sample := wifiModuleFixture()
			m := newModuleManager(nil, nil)
			m.ctx, m.ready, m.lastScan, m.wifiRetryLimit = context.Background(), true, now, 3
			m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
			cancelled := false
			w := &moduleWiFi{Enabled: true, ICCID: sample.Reading.ICCID, running: true, State: "connecting", cancel: func() { cancelled = true }, recovery: wifiRecovery{Failures: 3, RetryAllowed: true}}
			m.wifi[1] = w
			m.wifiEngine = recoveryWiFiEngine(func(context.Context, func() bool, func(string)) error { return nil })
			switch reason {
			case "busy":
				m.work[sample.Candidate.Key] = 1
			case "job":
				m.jobs[1] = moduleJob{State: "running"}
			case "settings-write":
				m.controlPending[sample.Candidate.Key] = true
			case "network-change":
				m.networkChanging[1] = true
			case "stale":
				m.lastScan = now.Add(-time.Minute)
			case "different-card":
				w.ICCID = "other"
			case "disabled":
				w.Enabled = false
			case "registered":
				w.Registered = true
			case "quarantine":
				m.recoveryUntil[sample.Candidate.Key] = now.Add(time.Minute)
			case "throttled":
				w.recovery.RetryAllowed = false
			case "limit-zero":
				m.wifiRetryLimit = 0
			case "draining":
				m.draining = true
			}
			m.recoverWiFiLocked(1, now)
			if cancelled || w.recovery.Used {
				t.Fatal("protected session reset")
			}
		})
	}
}

type restoreWiFiEngine struct {
	recoveryWiFiEngine
	calls atomic.Int32
	err   error
}

func (f *restoreWiFiEngine) RestoreRadio(context.Context, hardware.Candidate, string, string) error {
	f.calls.Add(1)
	return f.err
}

func TestWiFiFullRecoveryStoppedWorker(t *testing.T) {
	for _, code := range []string{"", "WIFI_RADIO_RESTORE_UNCONFIRMED"} {
		t.Run(code, func(t *testing.T) {
			m := newModuleManager(nil, nil)
			sample := wifiModuleFixture()
			m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
			m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
			engine := &restoreWiFiEngine{}
			if code != "" {
				engine.err = errors.New(code)
			}
			m.wifiEngine = engine
			w := &moduleWiFi{Enabled: true, ICCID: sample.Reading.ICCID, State: "failed", Issue: "NETWORK_UNAVAILABLE", recovery: wifiRecovery{Failures: 3, RetryAllowed: true}}
			m.wifi[1] = w
			m.recoverWiFi(3, time.Now())
			m.operations.Wait()
			m.recoverWiFi(3, time.Now())
			if engine.calls.Load() != 1 || !w.Enabled || w.running || w.recovery.Stopping || !w.recovery.Used {
				t.Fatal("restore repeated or changed user intent")
			}
			if code == "" && (w.State != "waiting" || !time.Now().Before(w.recovery.VerifyUntil)) || code != "" && (w.State != "failed" || w.Issue != code) {
				t.Fatal("unverified restore result", w.State, w.Issue)
			}
		})
	}
}
