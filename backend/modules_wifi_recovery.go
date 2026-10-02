package main

import (
	"context"
	"log"
	"time"

	"rykvo.local/auth/internal/hardware"
)

type wifiPolicySource interface {
	WiFiWithPolicy(context.Context, hardware.Candidate, string, string, func() bool, func(string)) error
}

type wifiRecovery struct {
	Failures     int
	RetryAllowed bool
	Used         bool
	Stopping     bool
	VerifyUntil  time.Time
}

// Count completed transport failures, not scans or reconnecting status updates.
func (m *moduleManager) wifiAttemptFailedLocked(id int64, w *moduleWiFi, now time.Time) {
	w.recovery.Failures = min(w.recovery.Failures+1, 100)
	w.recovery.RetryAllowed = true
	if w.recovery.Used && !w.recovery.Stopping {
		w.recovery.VerifyUntil = time.Time{}
	}
	m.recoverWiFiLocked(id, now)
}

func (m *moduleManager) recoverWiFi(limit int, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wifiRetryLimit = limit
	for id := range m.wifi {
		m.recoverWiFiLocked(id, now)
	}
}

// One full RF restore per outage; user intent stays enabled in the database.
func (m *moduleManager) recoverWiFiLocked(id int64, now time.Time) {
	w := m.wifi[id]
	if w == nil || !w.Enabled || w.Registered || !w.recovery.RetryAllowed || w.recovery.Used || m.wifiRetryLimit <= 0 || w.recovery.Failures < m.wifiRetryLimit {
		return
	}
	sample, exists := m.values[id]
	current, present := m.seen[sample.Candidate.Key]
	key := sample.Candidate.Key
	if m.draining || !m.ready || m.ctx == nil || m.ctx.Err() != nil || now.Sub(m.lastScan) > 20*time.Second || !exists || !present || !sameEndpoint(current, sample.Candidate) || w.ICCID != sample.Reading.ICCID || sample.Reading.SIM != "READY" {
		return
	}
	if m.work[key] > 0 || m.jobs[id].active() || m.controlPending[key] || m.networkChanging[id] || now.Before(m.recoveryUntil[key]) || now.Before(w.refreshUntil) {
		return
	}
	if w.running {
		if _, ok := m.wifiEngine.(wifiPolicySource); !ok || w.cancel == nil || w.State == "stopping" {
			return
		}
		w.recovery.Used, w.recovery.Stopping = true, true
		w.State = "stopping"
		w.cancel() // Same stop-cellular handshake as the administrator's off switch.
	} else {
		if w.Issue != "NETWORK_UNAVAILABLE" && !retryWiFiFailure(w) {
			return // Never reset through an operator rejection or unconfirmed cleanup.
		}
		restorer, ok := m.wifiEngine.(radioRestorer)
		if !ok {
			return
		}
		call, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		w.recovery.Used, w.recovery.Stopping = true, true
		w.cancel, w.running, w.candidate, w.State = cancel, true, sample.Candidate, "stopping"
		m.operations.Add(1)
		go func() { defer cancel(); m.runWiFi(call, id, w, sample, radioRestoreSource{restorer}) }()
	}
	log.Printf("module %d Wi-Fi full recovery after %d failed attempts", id, w.recovery.Failures)
}

func (w *moduleWiFi) recoveryPending(limit int, now time.Time) bool {
	if w.recovery.Stopping || now.Before(w.recovery.VerifyUntil) {
		return true
	}
	return limit > 0 && !w.recovery.Used && w.recovery.Failures > 0 && (w.running || w.Issue == "NETWORK_UNAVAILABLE" || retryWiFiFailure(w))
}
