package main

import (
	"context"
	"encoding/json"
	"rykvo.local/auth/internal/carrierconfig"
	"time"
)

func (m *moduleManager) loadCarrierConfigs(ctx context.Context) error {
	if m.db == nil {
		return nil
	}
	rows, err := m.db.Query(ctx, "SELECT iccid,configuration FROM card_carrier_configs")
	if err != nil {
		return err
	}
	defer rows.Close()
	configs := make(map[string]carrierconfig.Selection)
	for rows.Next() {
		var id string
		var b []byte
		var s carrierconfig.Selection
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		if json.Unmarshal(b, &s) == nil && s.Valid() {
			configs[id] = s
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.carrierConfigs = configs
	m.mu.Unlock()
	return nil
}

// The card's write lock does not block state readers or another module.
func (m *moduleManager) saveCarrierConfig(ctx context.Context, iccid string, s carrierconfig.Selection) {
	if !s.Valid() || iccid == "" {
		return
	}
	unlock, err := m.stateWrites.Lock(ctx, "carrier:"+iccid)
	if err != nil {
		return
	}
	defer unlock()
	b, _ := json.Marshal(s)
	m.mu.RLock()
	old, ok := m.carrierConfigs[iccid]
	m.mu.RUnlock()
	if ok {
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
	m.mu.Lock()
	m.carrierConfigs[iccid] = s
	m.mu.Unlock()
}
func (m *moduleManager) acceptCarrierConfig(ctx context.Context, id int64, w *moduleWiFi, sample moduleSample, raw string) {
	var selection carrierconfig.Selection
	if len(raw) > 3500 || json.Unmarshal([]byte(raw), &selection) != nil || !selection.Valid() || ctx.Err() != nil {
		return
	}
	m.mu.RLock()
	current, present := m.seen[sample.Candidate.Key]
	live, exists := m.values[id]
	valid := m.wifi[id] == w && w.Enabled && w.running && w.ICCID == sample.Reading.ICCID && present && exists && sameEndpoint(current, sample.Candidate) && sameEndpoint(live.Candidate, sample.Candidate) && live.Reading.ICCID == w.ICCID
	card := w.ICCID
	m.mu.RUnlock()
	if valid {
		m.saveCarrierConfig(ctx, card, selection)
	}
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
