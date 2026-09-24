package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
)

type moduleJob struct {
	ID      string `json:"id"`
	Module  int64  `json:"-"`
	Action  string `json:"action"`
	State   string `json:"state"`
	Stage   string `json:"stage"`
	Issue   string `json:"issue"`
	Warning string `json:"warning"`
}

const jobColumns = "id,module_id,action,state,stage,issue,warning"

var jobIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

func (j moduleJob) active() bool { return j.State == "queued" || j.State == "running" }
func scanJob(row interface{ Scan(...any) error }) (moduleJob, error) {
	var j moduleJob
	err := row.Scan(&j.ID, &j.Module, &j.Action, &j.State, &j.Stage, &j.Issue, &j.Warning)
	return j, err
}
func (m *moduleManager) loadJobs(ctx context.Context) {
	if _, err := m.db.Exec(ctx, "UPDATE module_jobs SET state='uncertain',issue='ESIM_INTERRUPTED',updated_at=now() WHERE state IN ('queued','running')"); err != nil {
		return
	}
	rows, err := m.db.Query(ctx, "SELECT DISTINCT ON(module_id) "+jobColumns+" FROM module_jobs ORDER BY module_id,created_at DESC")
	if err != nil {
		return
	}
	defer rows.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return
		}
		m.jobs[j.Module] = j
	}
	m.ready = rows.Err() == nil
}
func (m *moduleManager) gate(key string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gates[key] == nil {
		m.gates[key] = make(chan struct{}, 1)
	}
	return m.gates[key]
}
func (m *moduleManager) read(ctx context.Context, c hardware.Candidate) hardware.Reading {
	m.mu.RLock()
	busy := false
	for id, j := range m.jobs {
		if j.active() && m.values[id].Candidate.Key == c.Key {
			busy = true
			break
		}
	}
	m.mu.RUnlock()
	gate := m.gate(c.Key)
	if !busy {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			return m.source.Read(ctx, c)
		default:
		}
	}
	return hardware.Reading{Issue: "OPERATION_ACTIVE"}
}
func (m *moduleManager) job(id int64) moduleJob {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}
func (m *moduleManager) saveJob(j moduleJob) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := m.db.Exec(ctx, "UPDATE module_jobs SET state=$2,stage=$3,issue=$4,warning=$5,updated_at=now() WHERE id=$1", j.ID, j.State, j.Stage, j.Issue, j.Warning)
	m.mu.Lock()
	m.jobs[j.Module] = j
	m.mu.Unlock()
	return err == nil
}
func (m *moduleManager) startJob(ctx context.Context, v moduleRecord, request hardware.ESIMRequest, id string) (moduleJob, error) {
	// Retrying the same HTTP action never repeats the card write.
	old, err := scanJob(m.db.QueryRow(ctx, "SELECT "+jobColumns+" FROM module_jobs WHERE id=$1", id))
	if err == nil {
		if old.Module != v.ID || old.Action != request.Action {
			return old, errors.New("REQUEST_CONFLICT")
		}
		return old, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return moduleJob{}, errors.New("DATABASE_UNAVAILABLE")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready || m.ctx == nil || m.ctx.Err() != nil {
		return moduleJob{}, errors.New("ESIM_UNAVAILABLE")
	}
	if m.jobs[v.ID].active() {
		return moduleJob{}, errors.New("DEVICE_BUSY")
	}
	sample, ok := m.values[v.ID]
	current, present := m.seen[v.Endpoint]
	if !ok || !present || !sameEndpoint(sample.Candidate, current) || sample.Candidate.Key != v.Endpoint || time.Since(m.lastScan) > 20*time.Second {
		return moduleJob{}, errors.New("DEVICE_UNAVAILABLE")
	}
	info := sample.Reading.ESIM
	if info == nil || info.EID != request.EID || info.Issue != "" || time.Since(sample.Reading.UpdatedAt) > 90*time.Second {
		return moduleJob{}, errors.New("DEVICE_CHANGED")
	}
	select {
	case m.operationSlots <- struct{}{}:
	default:
		return moduleJob{}, errors.New("DEVICE_BUSY")
	}
	j := moduleJob{ID: id, Module: v.ID, Action: request.Action, State: "queued", Stage: "waiting"}
	_, err = m.db.Exec(ctx, "INSERT INTO module_jobs(id,module_id,action,state,stage) VALUES($1,$2,$3,$4,$5)", j.ID, j.Module, j.Action, j.State, j.Stage)
	if err != nil {
		<-m.operationSlots
		return moduleJob{}, errors.New("DATABASE_UNAVAILABLE")
	}
	request.Candidate = sample.Candidate
	request.ExpectedIMEI = sample.Reading.IMEI
	m.jobs[v.ID] = j
	m.operations.Add(1)
	go m.runJob(j, request)
	return j, nil
}
func (m *moduleManager) runJob(j moduleJob, r hardware.ESIMRequest) {
	defer m.operations.Done()
	defer func() { <-m.operationSlots }()
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Minute)
	defer cancel()
	gate := m.gate(r.Candidate.Key)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		j.State = "failed"
		j.Issue = "ESIM_INTERRUPTED"
		m.saveJob(j)
		return
	}
	m.mu.RLock()
	c, present := m.seen[r.Candidate.Key]
	m.mu.RUnlock()
	if !present || !sameEndpoint(c, r.Candidate) {
		j.State = "failed"
		j.Issue = "DEVICE_CHANGED"
		m.saveJob(j)
		return
	}
	j.State = "running"
	j.Stage = "checking"
	if !m.saveJob(j) {
		j.State = "failed"
		j.Issue = "DATABASE_UNAVAILABLE"
		m.saveJob(j)
		return
	}
	result := hardware.ESIMCall(ctx, r, func(stage string) {
		switch stage {
		case "writing", "authenticating", "downloading", "installing", "verifying", "notifying":
			j.Stage = stage
			m.saveJob(j)
		}
	})
	// The card may have committed before a transport error. Never auto-replay writes.
	j.State = "succeeded"
	j.Stage = "done"
	j.Issue = result.Issue
	j.Warning = result.Warning
	if !result.Verified {
		j.State = "failed"
		if result.Changed || j.Issue == "ESIM_INTERRUPTED" || j.Issue == "ESIM_RESULT_UNKNOWN" {
			j.State = "uncertain"
		}
	}
	readCtx, stop := context.WithTimeout(m.ctx, 50*time.Second)
	reading := m.source.Read(readCtx, r.Candidate)
	stop()
	if !result.Verified && result.Changed && hardware.VerifyESIM(r, result, reading.ESIM) {
		j.State = "succeeded"
		j.Issue = ""
		j.Warning = "ESIM_NOTIFICATION_PENDING"
	}
	m.mu.Lock()
	if current, ok := m.seen[r.Candidate.Key]; ok && sameEndpoint(current, r.Candidate) {
		m.values[j.Module] = moduleSample{r.Candidate, reading}
	}
	m.mu.Unlock()
	m.saveJob(j)
}
func (s *server) moduleControl(ctx context.Context, w http.ResponseWriter, r *http.Request, v moduleRecord, parts []string) {
	if s.modules == nil {
		fail(w, 503, "ESIM_UNAVAILABLE")
		return
	}
	if len(parts) == 2 && parts[1] == "esim" && r.Method == http.MethodGet {
		reply(w, 200, map[string]any{"data": s.moduleView(v)})
		return
	}
	action := ""
	switch {
	case len(parts) == 2 && parts[1] == "esim" && r.Method == http.MethodPost:
		action = "download"
	case len(parts) == 3 && parts[1] == "esim" && parts[2] == "notifications" && r.Method == http.MethodPost:
		action = "notifications"
	case len(parts) == 3 && parts[1] == "lines" && r.Method == http.MethodPatch:
		action = "line"
	case len(parts) == 3 && parts[1] == "lines" && r.Method == http.MethodDelete:
		action = "delete"
	default:
		fail(w, 503, "NOT_CONNECTED")
		return
	}
	var input struct {
		RequestID    string  `json:"requestId"`
		EID          string  `json:"eid"`
		Label        *string `json:"label,omitempty"`
		Enabled      *bool   `json:"enabled,omitempty"`
		Activation   string  `json:"activation,omitempty"`
		Confirmation string  `json:"confirmation,omitempty"`
		IMEI         string  `json:"imei,omitempty"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	if !jobIDPattern.MatchString(input.RequestID) {
		fail(w, 400, "INVALID_ESIM_REQUEST")
		return
	}
	reading, _, _ := s.modules.state(v)
	request := hardware.ESIMRequest{Action: action, EID: input.EID, Activation: strings.TrimSpace(input.Activation), Confirmation: input.Confirmation, IMEI: reading.IMEI}
	if request.IMEI == "" {
		request.IMEI = input.IMEI
	}
	if action == "line" || action == "delete" {
		if reading.ESIM != nil {
			for _, p := range reading.ESIM.Profiles {
				if hardware.ProfileID(input.EID, p.ICCID) == parts[2] {
					request.ICCID = p.ICCID
					break
				}
			}
		}
		if action == "line" {
			if (input.Label == nil) == (input.Enabled == nil) {
				fail(w, 400, "INVALID_ESIM_REQUEST")
				return
			}
			if input.Label != nil {
				request.Action = "rename"
				request.Label = strings.TrimSpace(*input.Label)
			} else if *input.Enabled {
				request.Action = "enable"
			} else {
				request.Action = "disable"
			}
		}
	}
	if !hardware.ValidESIMRequest(request) {
		fail(w, 400, "INVALID_ESIM_REQUEST")
		return
	}
	j, err := s.modules.startJob(ctx, v, request, input.RequestID)
	if err != nil {
		status := 409
		if err.Error() == "DATABASE_UNAVAILABLE" || err.Error() == "ESIM_UNAVAILABLE" {
			status = 503
		}
		fail(w, status, err.Error())
		return
	}
	reply(w, http.StatusAccepted, map[string]any{"data": j})
}
