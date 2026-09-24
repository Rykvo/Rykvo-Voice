package main

import (
	"context"
	"errors"
	"net/http"
	"rykvo.local/auth/internal/hardware"
	"time"
)

func networkAvailable(r hardware.Reading) bool {
	return r.Responsive && r.Issue == "" && r.SIM == "READY" && r.NetworkMode != nil && r.IMEI != "" && r.ICCID != ""
}

func currentNetworkLine(r hardware.Reading, line string) bool {
	if !networkAvailable(r) {
		return false
	}
	if r.ESIM != nil && r.ESIM.EID != "" {
		for _, p := range r.ESIM.Profiles {
			if p.Enabled && p.ICCID == r.ICCID && hardware.ProfileID(r.ESIM.EID, p.ICCID) == line {
				return true
			}
		}
		return false
	}
	return line == "line-"+hardware.Digest(r.ICCID)[:24]
}

func (s *server) networksAPI(ctx context.Context, w http.ResponseWriter, r *http.Request, v moduleRecord, line string) {
	if s.modules == nil {
		fail(w, 503, "NETWORK_UNAVAILABLE")
		return
	}
	reading, present, _ := s.modules.state(v)
	if !present || !currentNetworkLine(reading, line) {
		fail(w, 409, "DEVICE_CHANGED")
		return
	}
	if r.Method == http.MethodGet {
		j := s.modules.job(v.ID)
		if j.Action != "network-scan" || j.Verification == nil || j.Verification.Network == nil || j.Verification.Network.ICCID != reading.ICCID {
			j = moduleJob{}
		}
		reply(w, 200, map[string]any{"data": j})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var input struct {
		RequestID string `json:"requestId"`
		EID       string `json:"eid,omitempty"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	s.networkControl(ctx, w, r, v, line, input.RequestID, nil)
}

func (s *server) networkControl(ctx context.Context, w http.ResponseWriter, r *http.Request, v moduleRecord, line, id string, request *hardware.NetworkRequest) {
	reading, present, _ := s.modules.state(v)
	if !present || !currentNetworkLine(reading, line) {
		fail(w, 409, "DEVICE_CHANGED")
		return
	}
	action := "network-select"
	if request == nil {
		action = "network-scan"
		request = &hardware.NetworkRequest{IMEI: reading.IMEI, ICCID: reading.ICCID}
	}
	if !jobIDPattern.MatchString(id) || !request.Valid(action == "network-scan") {
		fail(w, 400, "INVALID_NETWORK_REQUEST")
		return
	}
	j, err := s.modules.startJob(ctx, v, moduleRequest{ESIMRequest: hardware.ESIMRequest{Action: action}, Network: request}, id)
	if err != nil {
		status := 409
		if err.Error() == "DATABASE_UNAVAILABLE" {
			status = 503
		}
		fail(w, status, err.Error())
		return
	}
	reply(w, http.StatusAccepted, map[string]any{"data": j})
}

func (m *moduleManager) runNetworkJob(parent context.Context, j moduleJob, r moduleRequest) {
	ctx, cancel := context.WithTimeout(parent, 200*time.Second)
	defer cancel()
	scan := j.Action == "network-scan"
	j.Stage = "selecting"
	if scan {
		j.Stage = "scanning"
	}
	if !m.saveJob(j) {
		j.State = "failed"
		j.Issue = "DATABASE_UNAVAILABLE"
		m.saveJob(j)
		return
	}
	operators, changed, err := hardware.NetworkCall(ctx, r.Candidate, *r.Network, scan)
	j.State, j.Stage = "failed", "done"
	if err != nil {
		j.Issue = "NETWORK_FAILED"
		for _, code := range []string{"DEVICE_CHANGED", "DEVICE_BUSY", "PERMISSION_DENIED", "COMMAND_UNSUPPORTED", "READ_TIMEOUT"} {
			if err.Error() == code {
				j.Issue = code
			}
		}
		if errors.Is(err, context.Canceled) {
			j.Issue = "NETWORK_INTERRUPTED"
		}
	} else if scan {
		j.State, j.Networks = "succeeded", operators
	}
	if !scan && changed {
		j.State, j.Issue = "uncertain", "NETWORK_RESULT_UNKNOWN"
	}
	// Refresh only after the command finishes; never repeat a selection on timeout.
	readCtx, stop := context.WithTimeout(m.ctx, 50*time.Second)
	defer stop()
	reading := m.source.Read(readCtx, r.Candidate)
	if !scan {
		j.confirm(reading)
	}
	m.mu.Lock()
	if current, ok := m.seen[r.Candidate.Key]; ok && sameEndpoint(current, r.Candidate) {
		m.values[j.Module] = moduleSample{r.Candidate, reading}
	}
	m.mu.Unlock()
	m.saveJob(j)
}
