package main

import (
	"context"
	"log"
	"time"

	"rykvo.local/auth/internal/hardware"
)

type recoveryStreak struct {
	generation  string
	since, last time.Time
	count       int
}

func (m *moduleManager) loadRecovery(ctx context.Context) {
	rows, err := m.db.Query(ctx, "SELECT DISTINCT module_id FROM module_recoveries WHERE result<>'recovered'")
	if err != nil {
		return
	}
	defer rows.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			return
		}
		m.recoveryPending[id] = true
	}
	m.recoveryReady = rows.Err() == nil
}

func (m *moduleManager) verifyRecovery(ctx context.Context, id int64, r hardware.Reading) {
	if !m.recoveryPending[id] || !hardware.CommunicationHealthy(r) {
		return
	}
	if _, err := m.db.Exec(ctx, "UPDATE module_recoveries SET result='recovered' WHERE module_id=$1 AND result<>'recovered'", id); err == nil {
		delete(m.recoveryPending, id)
		log.Printf("module recovery: module=%d result=recovered", id)
	}
}

func (s *recoveryStreak) observe(c hardware.Candidate, r hardware.Reading, now time.Time) bool {
	if !hardware.RecoverySupported(c) || !hardware.CommunicationStalled(r) {
		*s = recoveryStreak{}
		return false
	}
	if s.generation != c.Generation || s.since.IsZero() || now.Sub(s.last) > 2*time.Minute {
		*s = recoveryStreak{generation: c.Generation, since: now}
	}
	s.last = now
	s.count++
	return s.count >= 3 && now.Sub(s.since) >= 90*time.Second
}

// Called with the device gate held; writes and reads cannot overlap a restart.
func (m *moduleManager) recoverModule(ctx context.Context, c hardware.Candidate, r hardware.Reading) {
	restarter, ok := m.source.(interface {
		Restart(context.Context, hardware.Candidate) error
	})
	if !ok || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	now := time.Now()
	s := m.recovery[c.Key]
	ready := s.observe(c, r, now)
	m.recovery[c.Key] = s
	current, present := m.seen[c.Key]
	proof, proven := m.recoveryProof[c.Key]
	if !ready || !proven || !sameEndpoint(proof.Candidate, c) || !m.ready || !m.recoveryReady || !present || !sameEndpoint(c, current) || now.Sub(m.lastScan) > 20*time.Second {
		m.mu.Unlock()
		return
	}
	for id, job := range m.jobs {
		if job.active() && m.values[id].Candidate.Key == c.Key {
			delete(m.recovery, c.Key)
			m.mu.Unlock()
			return
		}
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	id, module, err := m.reserveRecovery(call, c, proof.Candidate.Identity(proof.Reading))
	cancel()
	if err != nil || id == 0 {
		m.mu.Unlock()
		return
	}
	m.recoveryUntil[c.Key] = now.Add(90 * time.Second)
	m.recoveryPending[module] = true
	delete(m.recovery, c.Key)
	m.mu.Unlock()

	log.Printf("module recovery: module=%d phase=request", module)
	err = restarter.Restart(ctx, c)
	result := "requested"
	if err != nil {
		result = "unconfirmed"
	}
	// A command acknowledgement is not proof that the module recovered.
	log.Printf("module recovery: module=%d result=%s", module, result)
	call, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := m.db.Exec(call, "UPDATE module_recoveries SET result=$2 WHERE id=$1", id, result); err != nil {
		log.Printf("module recovery: module=%d result=record_failed", module)
	}
}

// Persist before sending, so a process restart or lost reply cannot replay a reset.
func (m *moduleManager) reserveRecovery(ctx context.Context, c hardware.Candidate, identity string) (int64, int64, error) {
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(72859602)"); err != nil {
		return 0, 0, err
	}
	var id, module int64
	err = tx.QueryRow(ctx, `INSERT INTO module_recoveries(module_id)
		SELECT id FROM modules m WHERE endpoint=$1 AND endpoint_generation=$2 AND hardware_key=$3 AND hardware_key LIKE 'imei:%'
		AND NOT EXISTS(SELECT 1 FROM module_jobs WHERE module_id=m.id AND state IN ('queued','running'))
		AND NOT EXISTS(SELECT 1 FROM module_recoveries WHERE attempted_at>now()-interval '90 seconds')
		AND NOT EXISTS(SELECT 1 FROM module_recoveries WHERE module_id=m.id AND attempted_at>now()-interval '10 minutes')
		AND NOT EXISTS(SELECT 1 FROM module_recoveries WHERE module_id=m.id AND result='unconfirmed')
		AND (SELECT count(*) FROM module_recoveries WHERE module_id=m.id AND attempted_at>now()-interval '1 hour')<2
		RETURNING id,module_id`, c.Key, c.Generation, identity).Scan(&id, &module)
	if err != nil {
		return 0, 0, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM module_recoveries WHERE result='recovered' AND attempted_at<now()-interval '7 days'"); err != nil {
		return 0, 0, err
	}
	return id, module, tx.Commit(ctx)
}
