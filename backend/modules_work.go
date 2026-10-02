package main

import (
	"sync"
	"time"
)

// Business work reserves admission, not the hardware gate. Wi-Fi media and messages
// may coexist; destructive controls wait until both have finished.
func (m *moduleManager) reserveModuleWork(sample moduleSample) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.moduleWorkAllowedLocked(sample) {
		return nil
	}
	key := sample.Candidate.Key
	m.work[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.work[key]--
			if m.work[key] == 0 {
				delete(m.work, key)
			}
		})
	}
}

func (m *moduleManager) moduleWorkAllowedLocked(sample moduleSample) bool {
	key := sample.Candidate.Key
	if key == "" || m.draining || !m.ready || m.ctx == nil || m.ctx.Err() != nil || m.controlPending[key] || time.Now().Before(m.recoveryUntil[key]) || time.Since(m.lastScan) > 20*time.Second {
		return false
	}
	current, present := m.seen[key]
	if !present || !sameEndpoint(current, sample.Candidate) {
		return false
	}
	found := false
	for id, value := range m.values {
		if value.Candidate.Key != key {
			continue
		}
		if m.networkChanging[id] || (m.wifi[id] != nil && m.wifi[id].rebinding) || m.jobs[id].active() || value.Reading.ICCID != sample.Reading.ICCID || !sameEndpoint(value.Candidate, sample.Candidate) {
			return false
		}
		found = true
		break
	}
	return found
}
