package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
)

type moduleRestarter interface {
	RestartModule(context.Context, hardware.Candidate, string) error
}
type hostRestarter interface{ RestartHost(context.Context) error }
type restartTarget struct {
	job    moduleJob
	sample moduleSample
}

func (j *moduleJob) confirmRestart(s moduleSample) bool {
	v := j.Verification
	if j.Action != "restart" || v == nil || v.Generation == "" || s.Candidate.Generation == "" || s.Candidate.Generation == v.Generation || v.IMEI != s.Reading.IMEI || !hardware.CommunicationHealthy(s.Reading) {
		return false
	}
	j.State, j.Stage, j.Issue = "succeeded", "done", ""
	return true
}

// A restart preserves SIM/Wi-Fi intent. Only a new USB generation proves completion.
func (m *moduleManager) restartableLocked(v moduleRecord) bool {
	s, ok := m.values[v.ID]
	c, present := m.seen[v.Endpoint]
	_, supported := m.source.(moduleRestarter)
	return m.restartCleanupIssueLocked(v.ID) == "" && supported && !m.draining && m.work[v.Endpoint] == 0 && !m.controlPending[v.Endpoint] && m.ready && m.ctx != nil && m.ctx.Err() == nil && ok && present &&
		s.Candidate.Key == v.Endpoint && sameEndpoint(s.Candidate, c) && hardware.RecoverySupported(c) &&
		s.Candidate.Identity(s.Reading) == v.Identity && len(s.Reading.IMEI) >= 14 &&
		time.Since(m.lastScan) < 20*time.Second && !m.jobs[v.ID].active() && !time.Now().Before(m.recoveryUntil[v.Endpoint])
}

func (m *moduleManager) restartCleanupIssueLocked(id int64) string {
	if w := m.wifi[id]; w != nil && hardware.WiFiCleanupUnconfirmed(w.Issue) {
		return w.Issue
	}
	return ""
}

func (m *moduleManager) restartModules(ctx context.Context, records []moduleRecord, scope, request string) ([]moduleJob, error) {
	release, err := m.lockControls(ctx, records)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return nil, errors.New("DATABASE_UNAVAILABLE")
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(72859603)"); err != nil {
		return nil, err
	}
	var previous string
	err = tx.QueryRow(ctx, "SELECT scope FROM restart_requests WHERE id=$1", request).Scan(&previous)
	if err == nil {
		if previous != scope {
			return nil, errors.New("REQUEST_CONFLICT")
		}
		rows, e := tx.Query(ctx, "SELECT "+jobColumns+" FROM module_jobs WHERE action='restart' AND id=$1||'-'||module_id::text ORDER BY module_id", request)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		jobs := []moduleJob{}
		for rows.Next() {
			j, e := scanJob(rows)
			if e != nil {
				return nil, e
			}
			jobs = append(jobs, j)
		}
		return jobs, rows.Err()
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("DEVICE_UNAVAILABLE")
	}
	var targets []restartTarget
	previousJobs := make(map[int64]moduleJob, len(records))
	m.mu.Lock()
	for _, v := range records {
		if issue := m.restartCleanupIssueLocked(v.ID); issue != "" {
			m.mu.Unlock()
			return nil, errors.New(issue)
		}
		if !m.restartableLocked(v) {
			m.mu.Unlock()
			return nil, errors.New("DEVICE_BUSY")
		}
		sample := m.values[v.ID]
		j := moduleJob{ID: request + "-" + strconv.FormatInt(v.ID, 10), Module: v.ID, Action: "restart", State: "queued", Stage: "waiting", Verification: &moduleVerification{IMEI: sample.Reading.IMEI, Generation: sample.Candidate.Generation}}
		targets = append(targets, restartTarget{j, sample})
		previousJobs[v.ID] = m.jobs[v.ID]
	}
	for _, target := range targets {
		m.jobs[target.job.Module] = target.job
	}
	m.operations.Add(1)
	m.mu.Unlock()
	started := false
	defer func() {
		if started {
			return
		}
		m.mu.Lock()
		for _, target := range targets {
			if m.jobs[target.job.Module].ID == target.job.ID {
				m.jobs[target.job.Module] = previousJobs[target.job.Module]
			}
		}
		m.mu.Unlock()
		m.operations.Done()
	}()
	for _, v := range records {
		var busy bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM module_jobs WHERE module_id=$1 AND (state IN ('queued','running') OR (action='restart' AND created_at>now()-interval '2 minutes'))) OR EXISTS(SELECT 1 FROM messages WHERE module_id=$1 AND state IN ('sending','downloading'))`, v.ID).Scan(&busy); err != nil {
			return nil, err
		}
		if busy {
			return nil, errors.New("DEVICE_BUSY")
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO restart_requests(id,scope) VALUES($1,$2)", request, scope); err != nil {
		return nil, err
	}
	jobs := make([]moduleJob, 0, len(targets))
	for _, t := range targets {
		j := t.job
		verification, _ := json.Marshal(j.Verification)
		if _, err = tx.Exec(ctx, "INSERT INTO module_jobs(id,module_id,action,state,stage,verification) VALUES($1,$2,$3,$4,$5,$6)", j.ID, j.Module, j.Action, j.State, j.Stage, verification); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	started = true
	go func() { defer m.operations.Done(); m.runRestarts(targets) }()

	return jobs, nil
}

// Administrative writes serialize only their own modules, in a stable lock order.
func (m *moduleManager) lockControls(ctx context.Context, records []moduleRecord) (func(), error) {
	ids := make([]int64, 0, len(records))
	seen := map[int64]bool{}
	for _, v := range records {
		if !seen[v.ID] {
			ids = append(ids, v.ID)
			seen[v.ID] = true
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	releases := make([]func(), 0, len(ids))
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, id := range ids {
		done, err := m.controlWrites.Lock(ctx, id)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, done)
	}
	return release, nil
}
func (m *moduleManager) runRestarts(targets []restartTarget) {
	jobs := make(chan restartTarget, len(targets))
	for _, target := range targets {
		jobs <- target
	}
	close(jobs)
	var workers sync.WaitGroup
	for i := 0; i < min(2, len(targets)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range jobs {
				m.runRestart(target)
			}
		}()
	}
	workers.Wait()
}

func (m *moduleManager) runRestart(t restartTarget) {
	j, s := t.job, t.sample
	ctx, cancel := context.WithTimeout(m.ctx, 100*time.Second)
	defer cancel()
	m.mu.Lock()
	if w := m.wifi[j.Module]; w != nil && w.running {
		w.setRegistered(false, time.Now())
		w.State = "stopping"
		w.cancel()
	}
	m.mu.Unlock()
	finish := func(issue string) {
		j.State, j.Stage, j.Issue = "failed", "done", issue
		m.saveJob(j)
		m.mu.Lock()
		if issue != "DEVICE_BUSY" && issue != "DEVICE_CHANGED" && !hardware.WiFiCleanupUnconfirmed(issue) && ctx.Err() == nil {
			resumeWiFiIntent(m.wifi[j.Module], s.Reading)
		}
		m.mu.Unlock()
	}
	gate := m.gate(s.Candidate.Key)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		finish("RESTART_INTERRUPTED")
		return
	}
	m.mu.RLock()
	c, present := m.seen[s.Candidate.Key]
	w := m.wifi[j.Module]
	unsafe := w != nil && w.running
	cleanupIssue := m.restartCleanupIssueLocked(j.Module)
	m.mu.RUnlock()
	if !present || !sameEndpoint(c, s.Candidate) {
		finish("DEVICE_CHANGED")
		return
	}
	if unsafe {
		finish("DEVICE_BUSY")
		return
	}
	if cleanupIssue != "" {
		finish(cleanupIssue)
		return
	}
	if ctx.Err() != nil {
		finish("RESTART_INTERRUPTED")
		return
	}
	j.State, j.Stage = "running", "restarting"
	if !m.saveJob(j) {
		finish("DATABASE_UNAVAILABLE")
		return
	}
	m.mu.Lock()
	m.recoveryUntil[s.Candidate.Key] = time.Now().Add(90 * time.Second)
	m.mu.Unlock()
	// Recheck endpoint generation and IMEI before the single reset command.
	err := m.source.(moduleRestarter).RestartModule(ctx, c, s.Reading.IMEI)
	j.State, j.Stage, j.Issue = "uncertain", "verifying", "RESTART_UNCONFIRMED"
	if err != nil {
		j.Issue = "RESTART_RESULT_UNKNOWN"
	}
	m.saveJob(j)
}

func (m *moduleManager) restartHost(ctx context.Context, request string) (string, error) {
	release, err := m.controlWrites.Lock(ctx, 0)
	if err != nil {
		return "", err
	}
	defer release()
	var scope, state string
	err = m.db.QueryRow(ctx, "SELECT scope,state FROM restart_requests WHERE id=$1", request).Scan(&scope, &state)
	if err == nil {
		if scope != "host" {
			return "", errors.New("REQUEST_CONFLICT")
		}
		return state, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	m.mu.Lock()
	restarter, ok := m.source.(hostRestarter)
	if !ok || m.draining || !m.ready || m.ctx == nil || m.ctx.Err() != nil || len(m.work) > 0 || len(m.controlPending) > 0 {
		m.mu.Unlock()
		return "", errors.New("DEVICE_BUSY")
	}
	for _, j := range m.jobs {
		if j.active() {
			m.mu.Unlock()
			return "", errors.New("DEVICE_BUSY")
		}
	}
	// Reserve admission, not the state mutex, while persistence and reboot wait.
	m.ready = false
	m.operations.Add(1)
	m.mu.Unlock()
	handed := false
	defer func() {
		if handed {
			return
		}
		m.mu.Lock()
		m.ready = m.ctx.Err() == nil
		m.mu.Unlock()
		m.operations.Done()
	}()
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(72859603)"); err != nil {
		return "", err
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM module_jobs WHERE state IN ('queued','running'))
  OR EXISTS(SELECT 1 FROM messages WHERE state IN ('sending','downloading'))
  OR EXISTS(SELECT 1 FROM sip_call_records WHERE ended_at IS NULL AND state NOT IN ('ended','interrupted'))
  OR EXISTS(SELECT 1 FROM restart_requests WHERE scope='host' AND created_at>now()-interval '2 minutes')`).Scan(&busy)
	if err != nil {
		return "", err
	}
	if busy {
		return "", errors.New("DEVICE_BUSY")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO restart_requests(id,scope) VALUES($1,'host')", request); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	state = "accepted"
	if err = restarter.RestartHost(ctx); err != nil {
		state = "uncertain"
	}
	// An ambiguous response may still reboot. Keep admission closed until checked.
	handed = true
	go func() {
		defer m.operations.Done()
		timer := time.NewTimer(90 * time.Second)
		defer timer.Stop()
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		}
		m.mu.Lock()
		m.ready = m.ctx.Err() == nil
		m.mu.Unlock()
		call, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		m.db.Exec(call, "UPDATE restart_requests SET state='uncertain' WHERE id=$1", request)
	}()
	call, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err = m.db.Exec(call, "UPDATE restart_requests SET state=$2 WHERE id=$1", request, state); err != nil {
		return "uncertain", nil
	}
	return state, nil
}

func (s *server) moduleRestartAPI(ctx context.Context, w http.ResponseWriter, r *http.Request, scope string, records []moduleRecord) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var input struct {
		RequestID string `json:"requestId"`
		Confirm   bool   `json:"confirm"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	if !input.Confirm || !jobIDPattern.MatchString(input.RequestID) || len(input.RequestID) > 40 {
		fail(w, 400, "INVALID_REQUEST")
		return
	}
	if s.modules == nil {
		fail(w, 503, "DEVICE_UNAVAILABLE")
		return
	}
	var data any
	var err error
	if scope == "host" {
		var state string
		state, err = s.modules.restartHost(ctx, input.RequestID)
		data = map[string]string{"state": state}
	} else {
		var jobs []moduleJob
		jobs, err = s.modules.restartModules(ctx, records, scope, input.RequestID)
		entries := make([]map[string]any, 0, len(jobs))
		for _, j := range jobs {
			entries = append(entries, map[string]any{"moduleId": moduleID(j.Module), "job": j})
		}
		data = map[string]any{"jobs": entries}
	}
	if err != nil {
		code := err.Error()
		if code != "DEVICE_BUSY" && code != "DEVICE_UNAVAILABLE" && code != "REQUEST_CONFLICT" && !hardware.WiFiCleanupUnconfirmed(code) {
			code = "DATABASE_UNAVAILABLE"
		}
		fail(w, 409, code)
		return
	}
	reply(w, http.StatusAccepted, map[string]any{"data": data})
}
