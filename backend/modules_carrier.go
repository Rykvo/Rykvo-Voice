package main

import (
	"context"
	"encoding/json"
	"rykvo.local/auth/internal/carrierconfig"
	"time"
)

func (m *moduleManager) loadCarrierConfigs(ctx context.Context) {
	if m.db == nil {
		return
	}
	rows, err := m.db.Query(ctx, "SELECT iccid,configuration FROM card_carrier_configs")
	if err != nil {
		return
	}
	defer rows.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var id string
		var b []byte
		var s carrierconfig.Selection
		if rows.Scan(&id, &b) == nil && json.Unmarshal(b, &s) == nil && s.Valid() {
			m.carrierConfigs[id] = s
		}
	}
}

// Caller holds the manager lock and has verified the currently inserted card.
func (m *moduleManager) saveCarrierConfig(ctx context.Context, iccid string, s carrierconfig.Selection) {
	if !s.Valid() || iccid == "" {
		return
	}
	b, _ := json.Marshal(s)
	if old, ok := m.carrierConfigs[iccid]; ok {
		previous, _ := json.Marshal(old)
		if string(previous) == string(b) {
			return
		}
	}
	if m.db != nil {
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, err := m.db.Exec(call, `INSERT INTO card_carrier_configs(iccid,configuration) VALUES($1,$2) ON CONFLICT(iccid) DO UPDATE SET configuration=$2,updated_at=now()`, iccid, b); err != nil {
			return
		}
	}
	m.carrierConfigs[iccid] = s
}
func (m *moduleManager) acceptCarrierConfig(ctx context.Context, id int64, w *moduleWiFi, sample moduleSample, raw string) {
	var selection carrierconfig.Selection
	if len(raw) > 3500 || json.Unmarshal([]byte(raw), &selection) != nil || !selection.Valid() || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, present := m.seen[sample.Candidate.Key]
	live, exists := m.values[id]
	if m.wifi[id] != w || !w.Enabled || !w.running || w.ICCID != sample.Reading.ICCID || !present || !exists || !sameEndpoint(current, sample.Candidate) || !sameEndpoint(live.Candidate, sample.Candidate) || live.Reading.ICCID != w.ICCID {
		return
	}
	m.saveCarrierConfig(ctx, w.ICCID, selection)
}
func (m *moduleManager) carrierView(iccid string) any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.carrierConfigs[iccid]; ok {
		return s.Public()
	}
	return map[string]string{"status": "identity_pending"}
}

func (m *moduleManager) mmsConfigured(card string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := m.carrierConfigs[card]
	return c.MMS.Status == "matched" && c.MMS.Profile != nil
}
