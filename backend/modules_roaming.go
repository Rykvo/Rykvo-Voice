package main

import (
	"context"
	"errors"
	"time"
)

// A per-card data permission, not an instruction to attach or enable RF.
func (m *moduleManager) loadRoaming(ctx context.Context) {
	if m.db == nil {
		return
	}
	rows, err := m.db.Query(ctx, "SELECT iccid,roaming FROM card_data_policies")
	if err != nil {
		return
	}
	defer rows.Close()
	policies := map[string]bool{}
	for rows.Next() {
		var card string
		var allowed bool
		if rows.Scan(&card, &allowed) != nil {
			return
		}
		policies[card] = allowed
	}
	if rows.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roaming, m.roamingReady = policies, true
}

func (m *moduleManager) roamingView(iccid string) (bool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roaming[iccid], m.roamingReady
}

// Caller holds m.mu. Bind the setting to a fresh, active card identity.
func (m *moduleManager) roamingCard(v moduleRecord, line string) (string, error) {
	sample, exists := m.values[v.ID]
	current, present := m.seen[v.Endpoint]
	if !m.ready || !m.roamingReady || !exists || !present || time.Since(m.lastScan) > 20*time.Second ||
		sample.Candidate.Key != v.Endpoint || !sameEndpoint(sample.Candidate, current) ||
		sample.Reading.SIM != "READY" || sample.Reading.ICCID == "" || wifiLine(sample.Reading) != line ||
		m.jobs[v.ID].active() || time.Now().Before(m.recoveryUntil[v.Endpoint]) {
		return "", errors.New("DEVICE_CHANGED")
	}
	if w := m.wifi[v.ID]; w != nil && (w.Enabled || w.running || w.State == "stopping") {
		return "", errors.New("WIFI_CALLING_ACTIVE")
	}
	return sample.Reading.ICCID, nil
}

func (m *moduleManager) setRoaming(ctx context.Context, v moduleRecord, line, requestID string, allowed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	card, err := m.roamingCard(v, line)
	if err != nil {
		return err
	}
	if m.db == nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	defer tx.Rollback(context.Background())
	result, err := tx.Exec(ctx, `INSERT INTO card_data_policy_requests(id,module_id,iccid,roaming) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, requestID, v.ID, card, allowed)
	if err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	if result.RowsAffected() == 0 {
		var module int64
		var previousCard string
		var previous bool
		if tx.QueryRow(ctx, "SELECT module_id,iccid,roaming FROM card_data_policy_requests WHERE id=$1", requestID).Scan(&module, &previousCard, &previous) != nil {
			return errors.New("DATABASE_UNAVAILABLE")
		}
		if module != v.ID || previousCard != card || previous != allowed {
			return errors.New("REQUEST_CONFLICT")
		}
		// Replaying an older operation must never overwrite a newer preference.
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO card_data_policies(iccid,roaming) VALUES($1,$2) ON CONFLICT(iccid) DO UPDATE SET roaming=$2,updated_at=now()`, card, allowed)
	if err != nil || tx.Commit(ctx) != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	m.roaming[card] = allowed
	return nil
}
