package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
)

type moduleJob struct {
	ID           string              `json:"id"`
	Module       int64               `json:"-"`
	Action       string              `json:"action"`
	State        string              `json:"state"`
	Stage        string              `json:"stage"`
	Issue        string              `json:"issue"`
	Warning      string              `json:"warning"`
	Verification *moduleVerification `json:"-"`
}

// Retain only the identity and expected result, never activation credentials.
type moduleVerification struct {
	EID        string `json:"eid"`
	ICCID      string `json:"iccid"`
	Label      string `json:"label,omitempty"`
	IMEI       string `json:"imei,omitempty"`
	Generation string `json:"generation,omitempty"`
}

func (j moduleJob) confirmed(reading hardware.Reading) bool {
	v := j.Verification
	if v == nil || v.EID == "" || !reading.Responsive || (v.IMEI != "" && reading.IMEI != v.IMEI) {
		return false
	}
	return hardware.VerifyESIM(hardware.ESIMRequest{Action: j.Action, EID: v.EID, ICCID: v.ICCID, Label: v.Label}, hardware.ESIMResult{TargetICCID: v.ICCID}, reading.ESIM)
}

func (j *moduleJob) confirm(reading hardware.Reading) bool {
	if !j.confirmed(reading) {
		return false
	}
	j.State, j.Stage, j.Issue, j.Warning = "succeeded", "done", "", ""
	if reading.ESIM != nil && reading.ESIM.Pending > 0 {
		j.Warning = "ESIM_NOTIFICATION_PENDING"
	}
	return true
}

const jobColumns = "id,module_id,action,state,stage,issue,warning,verification"

var jobIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

func (j moduleJob) active() bool { return j.State == "queued" || j.State == "running" }
func scanJob(row interface{ Scan(...any) error }) (moduleJob, error) {
	var j moduleJob
	var verification []byte
	err := row.Scan(&j.ID, &j.Module, &j.Action, &j.State, &j.Stage, &j.Issue, &j.Warning, &verification)
	if err == nil {
		err = json.Unmarshal(verification, &j.Verification)
	}
	return j, err
}
func (m *moduleManager) loadJobs(ctx context.Context) {
	if _, err := m.db.Exec(ctx, "UPDATE module_jobs SET state='uncertain',issue=CASE WHEN action LIKE 'network-%' THEN 'OPERATION_RETIRED' WHEN action='restart' THEN 'RESTART_INTERRUPTED' ELSE 'ESIM_INTERRUPTED' END,updated_at=now() WHERE state IN ('queued','running')"); err != nil {
		return
	}
	rows, err := m.db.Query(ctx, "SELECT "+jobColumns+" FROM (SELECT DISTINCT ON(module_id) "+jobColumns+" FROM module_jobs ORDER BY module_id,created_at DESC) latest WHERE action NOT LIKE 'network-%'")
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
	m.mu.Lock()
	busy := time.Now().Before(m.recoveryUntil[c.Key])
	for id, j := range m.jobs {
		if j.active() && m.values[id].Candidate.Key == c.Key {
			busy = true
			break
		}
	}
	if busy {
		delete(m.recovery, c.Key)
	}
	m.mu.Unlock()
	gate := m.gate(c.Key)
	if !busy {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
			r := m.source.Read(ctx, c)
			m.recoverModule(ctx, c, r)
			return r
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
	err := m.persistJob(ctx, j)
	m.mu.Lock()
	m.jobs[j.Module] = j
	m.mu.Unlock()
	return err == nil
}
func (m *moduleManager) persistJob(ctx context.Context, j moduleJob) error {
	verification, err := json.Marshal(j.Verification)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(ctx, "UPDATE module_jobs SET state=$2,stage=$3,issue=$4,warning=$5,verification=$6,updated_at=now() WHERE id=$1", j.ID, j.State, j.Stage, j.Issue, j.Warning, verification)
	return err
}

// Wi-Fi owns the card gate, so its matching snapshot can queue a handoff even
// when polling is paused. The helper rechecks IMEI/EID before any card write.
func wifiOwnsESIMSnapshot(w *moduleWiFi, sample moduleSample) bool {
	return w != nil && w.Enabled && w.running && w.ICCID != "" &&
		w.ICCID == sample.Reading.ICCID && w.candidate.Key == sample.Candidate.Key &&
		w.candidate.Generation != "" && sameEndpoint(w.candidate, sample.Candidate) &&
		sample.Reading.Responsive && sample.Reading.Issue == ""
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
	if m.jobs[v.ID].active() || time.Now().Before(m.recoveryUntil[v.Endpoint]) {
		return moduleJob{}, errors.New("DEVICE_BUSY")
	}
	sample, ok := m.values[v.ID]
	current, present := m.seen[v.Endpoint]
	if !ok || !present || !sameEndpoint(sample.Candidate, current) || sample.Candidate.Key != v.Endpoint || time.Since(m.lastScan) > 20*time.Second {
		return moduleJob{}, errors.New("DEVICE_UNAVAILABLE")
	}
	if time.Since(sample.Reading.UpdatedAt) > 90*time.Second && !wifiOwnsESIMSnapshot(m.wifi[v.ID], sample) {
		return moduleJob{}, errors.New("DEVICE_CHANGED")
	}
	info := sample.Reading.ESIM
	if info == nil || info.EID != request.EID || info.Issue != "" {
		return moduleJob{}, errors.New("DEVICE_CHANGED")
	}
	select {
	case m.operationSlots <- struct{}{}:
	default:
		return moduleJob{}, errors.New("DEVICE_BUSY")
	}
	j := moduleJob{ID: id, Module: v.ID, Action: request.Action, State: "queued", Stage: "waiting", Verification: &moduleVerification{EID: request.EID, ICCID: request.ICCID, Label: request.Label, IMEI: sample.Reading.IMEI}}
	verification, _ := json.Marshal(j.Verification)
	_, err = m.db.Exec(ctx, "INSERT INTO module_jobs(id,module_id,action,state,stage,verification) VALUES($1,$2,$3,$4,$5,$6)", j.ID, j.Module, j.Action, j.State, j.Stage, verification)
	if err != nil {
		<-m.operationSlots
		return moduleJob{}, errors.New("DATABASE_UNAVAILABLE")
	}
	if err = m.stopWiFiLocked(ctx, v.ID); err != nil {
		<-m.operationSlots
		_, _ = m.db.Exec(ctx, "UPDATE module_jobs SET state='failed',issue='DATABASE_UNAVAILABLE',updated_at=now() WHERE id=$1", j.ID)
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
	unsafe := m.wifi[j.Module] != nil && (m.wifi[j.Module].Issue == "WIFI_RADIO_RESTORE_UNCONFIRMED" || m.wifi[j.Module].Issue == "WIFI_SIM_CLEANUP_UNCONFIRMED")
	m.mu.RUnlock()
	if unsafe {
		j.State, j.Issue = "failed", "DEVICE_BUSY"
		m.saveJob(j)
		return
	}
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
	if r.Action == "download" {
		verification := *j.Verification
		verification.ICCID = result.TargetICCID
		j.Verification = &verification
	}
	j.Stage = "verifying"
	m.saveJob(j)
	// Read after SIM refresh; never replay a write to resolve an uncertain result.
	readCtx, stop := context.WithTimeout(m.ctx, 90*time.Second)
	defer stop()
	var reading hardware.Reading
	confirmed := false
	for attempt := 0; attempt < 4; attempt++ {
		m.mu.RLock()
		current, present := m.seen[r.Candidate.Key]
		m.mu.RUnlock()
		if !present || !sameEndpoint(current, r.Candidate) {
			break
		}
		call, cancelRead := context.WithTimeout(readCtx, 30*time.Second)
		reading = m.source.Read(call, r.Candidate)
		cancelRead()
		confirmed = j.confirm(reading)
		if confirmed || readCtx.Err() != nil || !result.Changed || (reading.ESIM != nil && reading.ESIM.EID != "" && reading.ESIM.EID != r.EID) {
			break
		}
		if attempt < 3 {
			select {
			case <-readCtx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}
	if !confirmed {
		j.State, j.Stage, j.Issue, j.Warning = "failed", "done", result.Issue, result.Warning
		if result.Verified {
			j.State, j.Issue = "succeeded", ""
		} else if result.Changed || result.Issue == "ESIM_INTERRUPTED" || result.Issue == "ESIM_RESULT_UNKNOWN" {
			j.State, j.Issue = "uncertain", "ESIM_RESULT_UNKNOWN"
		}
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
		fail(w, 404, "NOT_FOUND")
		return
	}
	var input struct {
		Roaming      *bool   `json:"roaming,omitempty"`
		WiFiCalling  *bool   `json:"wifiCalling,omitempty"`
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
	if input.Roaming != nil {
		if action != "line" || input.WiFiCalling != nil || input.Label != nil || input.Enabled != nil || input.Activation != "" || input.Confirmation != "" || input.IMEI != "" {
			fail(w, 400, "INVALID_REQUEST")
			return
		}
		if err := s.modules.setRoaming(ctx, v, parts[2], input.RequestID, *input.Roaming); err != nil {
			fail(w, 409, err.Error())
			return
		}
		reply(w, http.StatusOK, map[string]any{"data": s.moduleView(v)})
		return
	}
	if input.WiFiCalling != nil {
		if action != "line" || input.Label != nil || input.Enabled != nil || input.Activation != "" || input.Confirmation != "" || input.IMEI != "" {
			fail(w, 400, "INVALID_REQUEST")
			return
		}
		if err := s.modules.setWiFi(ctx, v, parts[2], input.RequestID, *input.WiFiCalling); err != nil {
			fail(w, 409, err.Error())
			return
		}
		reply(w, http.StatusAccepted, map[string]any{"data": s.moduleView(v)})
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
