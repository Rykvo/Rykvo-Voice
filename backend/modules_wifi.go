package main

import (
	"context"
	"errors"
	"log"
	"rykvo.local/auth/internal/hardware"
	"time"
)

type moduleWiFi struct {
	ICCID, RequestID string
	Enabled          bool
	State, Issue     string
	Registered       bool
	candidate        hardware.Candidate
	running          bool
	cancel           context.CancelFunc
}
type wifiSource interface {
	WiFi(context.Context, hardware.Candidate, string, string, func(string)) error
}

func (m *moduleManager) loadWiFi(ctx context.Context) {
	rows, err := m.db.Query(ctx, "SELECT module_id,iccid,enabled,request_id FROM module_wifi")
	if err != nil {
		return
	}
	defer rows.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var id int64
		w := &moduleWiFi{State: "off"}
		if rows.Scan(&id, &w.ICCID, &w.Enabled, &w.RequestID) != nil {
			return
		}
		if w.Enabled {
			w.State = "waiting"
		}
		m.wifi[id] = w
	}
}
func wifiLine(reading hardware.Reading) string {
	if reading.ICCID == "" {
		return ""
	}
	if reading.ESIM != nil && reading.ESIM.EID != "" {
		for _, p := range reading.ESIM.Profiles {
			if p.Enabled && p.ICCID == reading.ICCID {
				return hardware.ProfileID(reading.ESIM.EID, p.ICCID)
			}
		}
		return ""
	}
	return "line-" + hardware.Digest(reading.ICCID)[:24]
}
func (m *moduleManager) wifiView(id int64, iccid string) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := map[string]any{"enabled": false, "registered": false, "state": "off", "issue": ""}
	w := m.wifi[id]
	if w == nil || w.ICCID != iccid {
		return v
	}
	v["enabled"], v["registered"], v["state"], v["issue"] = w.Enabled, w.Registered, w.State, w.Issue
	return v
}
func (m *moduleManager) setWiFi(ctx context.Context, v moduleRecord, line, id string, enabled bool) error {
	if _, ok := m.source.(wifiSource); !ok {
		return errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready || m.ctx == nil || m.ctx.Err() != nil {
		return errors.New("DEVICE_UNAVAILABLE")
	}
	w := m.wifi[v.ID]
	if w != nil && w.RequestID == id {
		return nil
	}
	sample, ok := m.values[v.ID]
	current, present := m.seen[v.Endpoint]
	if !ok || !present || !sameEndpoint(sample.Candidate, current) || sample.Candidate.Key != v.Endpoint || time.Since(m.lastScan) > 20*time.Second {
		return errors.New("DEVICE_UNAVAILABLE")
	}
	if line != wifiLine(sample.Reading) || sample.Reading.SIM != "READY" {
		return errors.New("DEVICE_CHANGED")
	}
	if enabled && (!hardware.WiFiSupported(current) || sample.Reading.Issue != "" || !sample.Reading.Responsive) {
		return errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	if enabled && (m.jobs[v.ID].active() || time.Now().Before(m.recoveryUntil[v.Endpoint])) {
		return errors.New("DEVICE_BUSY")
	}
	if enabled && w != nil && w.running && (!w.Enabled || w.ICCID != sample.Reading.ICCID) {
		return errors.New("DEVICE_BUSY")
	}
	if _, err := m.db.Exec(ctx, `INSERT INTO module_wifi(module_id,iccid,enabled,request_id) VALUES($1,$2,$3,$4) ON CONFLICT(module_id) DO UPDATE SET iccid=$2,enabled=$3,request_id=$4`, v.ID, sample.Reading.ICCID, enabled, id); err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	if w == nil {
		w = &moduleWiFi{}
		m.wifi[v.ID] = w
	}
	w.ICCID, w.RequestID, w.Enabled = sample.Reading.ICCID, id, enabled
	if !enabled {
		w.Registered = false
		if w.running {
			w.State = "stopping"
			w.cancel()
		} else {
			w.State, w.Issue = "off", ""
		}
		return nil
	}
	if !w.running {
		w.State, w.Issue = "waiting", ""
		m.startWiFiLocked(v.ID, sample)
	}
	return nil
}

// Called under m.mu after a verified hardware sample.
func (m *moduleManager) startWiFiLocked(id int64, sample moduleSample) {
	w := m.wifi[id]
	if w == nil || !w.Enabled || w.running || w.State != "waiting" || w.ICCID != sample.Reading.ICCID || sample.Reading.SIM != "READY" || !sample.Reading.Responsive || sample.Reading.Issue != "" || m.jobs[id].active() || time.Now().Before(m.recoveryUntil[sample.Candidate.Key]) {
		return
	}
	source, ok := m.source.(wifiSource)
	if !ok || !hardware.WiFiSupported(sample.Candidate) {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w.cancel, w.running, w.candidate = cancel, true, sample.Candidate
	w.State, w.Registered, w.Issue = "connecting", false, ""
	m.operations.Add(1)
	go m.runWiFi(ctx, id, w, sample, source)
}
func (m *moduleManager) runWiFi(ctx context.Context, id int64, w *moduleWiFi, sample moduleSample, source wifiSource) {
	defer m.operations.Done()
	gate := m.gate(sample.Candidate.Key)
	var err error
	acquired := false
	select {
	case gate <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	if acquired {
		defer func() { <-gate }()
	}
	if acquired && ctx.Err() == nil {
		err = source.WiFi(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), sample.Reading.ICCID, func(stage string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.wifi[id] != w {
				return
			}
			switch stage {
			case "connected", "renewed":
				if w.Enabled && ctx.Err() == nil {
					w.State, w.Registered, w.Issue = "connected", true, ""
				}
				value := m.values[id]
				if sameEndpoint(value.Candidate, sample.Candidate) {
					value.Reading.UpdatedAt = time.Now().UTC()
					m.values[id] = value
				}
			case "reconnecting":
				w.State, w.Registered = "connecting", false
			case "ims-cleaned", "radio-restored":
				w.Registered = false
			}
		})
	}
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wifi[id] != w {
		return
	}
	w.running, w.Registered = false, false
	w.cancel = nil
	current, present := m.seen[sample.Candidate.Key]
	changed := !present || !sameEndpoint(current, sample.Candidate)
	code := hardware.WiFiIssue(err)
	if err != nil && !errors.Is(err, context.Canceled) {
		w.State, w.Issue = "failed", code
		log.Printf("module %d Wi-Fi stopped: %s", id, code)
	} else {
		w.State, w.Issue = "off", ""
	}
	if w.Enabled && changed {
		w.State, w.Issue = "waiting", ""
	}
}

// eSIM jobs wait for the existing module gate; no write races RF cleanup.
func (m *moduleManager) stopWiFiLocked(ctx context.Context, id int64) error {
	w := m.wifi[id]
	if w == nil || !w.Enabled && !w.running {
		return nil
	}
	if _, err := m.db.Exec(ctx, "UPDATE module_wifi SET enabled=false WHERE module_id=$1", id); err != nil {
		return err
	}
	w.Enabled, w.Registered = false, false
	if w.running {
		w.State = "stopping"
		w.cancel()
	} else {
		w.State = "off"
	}
	return nil
}
