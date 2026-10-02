package main

import (
	"context"
	"errors"
	"time"

	"rykvo.local/auth/internal/hardware"
)

// Retain the device gate until card epoch and job result are committed together.
// Wi-Fi may then own the gate indefinitely; a later poll is not a commit barrier.
func (m *moduleManager) finishESIMJob(j moduleJob, r hardware.ESIMRequest, reading hardware.Reading) {
	m.mu.RLock()
	current, present := m.seen[r.Candidate.Key]
	live := m.jobs[j.Module]
	m.mu.RUnlock()
	if live.ID != j.ID || !live.active() {
		return
	}
	if !present || !sameEndpoint(current, r.Candidate) {
		j.State, j.Stage, j.Issue = "uncertain", "done", "DEVICE_CHANGED"
		m.saveJob(j)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := m.persistESIMCompletion(ctx, j, r, reading)
	if err != nil {
		j.State, j.Stage, j.Issue = "uncertain", "done", "DATABASE_UNAVAILABLE"
		if err.Error() == "DEVICE_CHANGED" || err.Error() == "ESIM_RESULT_UNKNOWN" {
			j.Issue = err.Error()
		}
		// Prevent an uncommitted card from entering Wi-Fi or sending new messages.
		// Normal read recovery retries persistence, never the eSIM write.
		reading.Issue = j.Issue
		_ = m.persistJob(ctx, j)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs[j.Module].ID != j.ID {
		return
	}
	if current, ok := m.seen[r.Candidate.Key]; ok && sameEndpoint(current, r.Candidate) {
		m.values[j.Module] = moduleSample{r.Candidate, reading}
	}
	m.jobs[j.Module] = j
}

func (m *moduleManager) persistESIMCompletion(ctx context.Context, j moduleJob, r hardware.ESIMRequest, reading hardware.Reading) error {
	if j.State == "succeeded" && r.Action == "enable" && !esimActivationReady(reading, r.ICCID) {
		return errors.New("ESIM_RESULT_UNKNOWN")
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if reading.Responsive && reading.SIM == "READY" && validICCID(reading.ICCID) {
		if (r.ExpectedIMEI == "" && r.Candidate.Kind != "reader") || (r.ExpectedIMEI != "" && reading.IMEI != r.ExpectedIMEI) || reading.ESIM == nil ||
			reading.ESIM.EID != r.EID || reading.ESIM.Issue != "" {
			return errors.New("DEVICE_CHANGED")
		}
		active := wifiLine(reading) != ""
		if j.State == "succeeded" && r.Action == "enable" && !active {
			return errors.New("ESIM_RESULT_UNKNOWN")
		}
		// Empty/disabled inventories have no active profile, not a new device.
		tag, err := tx.Exec(ctx, `UPDATE modules SET active_card=CASE WHEN $6 THEN $2 ELSE active_card END
			WHERE id=$1 AND hardware_key=$3 AND endpoint=$4 AND endpoint_generation=$5`,
			j.Module, reading.ICCID, r.Candidate.Identity(reading), r.Candidate.Key, r.Candidate.Generation, active)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("DEVICE_CHANGED")
		}
	}
	if err := persistModuleJob(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
