package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
)

type apnSource interface {
	APN(context.Context, hardware.Candidate, string, string, *hardware.APNConfig) ([]hardware.APNContext, error)
}
type apnProfile struct {
	ID string `json:"id"`
	hardware.APNConfig
	HasPassword bool `json:"hasPassword"`
}

func (m *moduleManager) accessAPN(ctx context.Context, v moduleRecord, iccid string, config *hardware.APNConfig) ([]hardware.APNContext, error) {
	source, ok := m.source.(apnSource)
	if !ok {
		return nil, errors.New("COMMAND_UNSUPPORTED")
	}
	gate := m.gate(v.Endpoint)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		return nil, errors.New("DEVICE_BUSY")
	}
	m.mu.RLock()
	sample, exists := m.values[v.ID]
	current, present := m.seen[v.Endpoint]
	valid := m.ready && exists && present && sample.Candidate.Key == v.Endpoint && sameEndpoint(sample.Candidate, current) && sample.Reading.ICCID == iccid && sample.Reading.SIM == "READY" && sample.Reading.Issue == "" && !m.jobs[v.ID].active() && time.Since(m.lastScan) < 20*time.Second && time.Since(sample.Reading.UpdatedAt) < 90*time.Second && !time.Now().Before(m.recoveryUntil[v.Endpoint])
	if w := m.wifi[v.ID]; w != nil && w.running {
		valid = false
	}
	m.mu.RUnlock()
	if !valid {
		return nil, errors.New("DEVICE_CHANGED")
	}
	return source.APN(ctx, sample.Candidate, v.Identity, iccid, config)
}
func (s *server) moduleAPN(ctx context.Context, w http.ResponseWriter, r *http.Request, v moduleRecord, parts []string) {
	// Existing session/CSRF checks run before this route. Resolve the selected
	// line from server state, never accept an arbitrary client-supplied ICCID.
	if len(parts) < 4 || len(parts) > 6 {
		fail(w, 404, "NOT_FOUND")
		return
	}
	var iccid string
	for _, entry := range s.moduleView(v)["sims"].([]any) {
		sim, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if sim["id"] == parts[2] {
			iccid, _ = sim["iccid"].(string)
			break
		}
	}
	if iccid == "" {
		fail(w, 409, "DEVICE_CHANGED")
		return
	}
	if len(parts) == 4 && r.Method == http.MethodGet {
		rows, err := s.db.Query(ctx, "SELECT id,configuration FROM card_apn_profiles WHERE iccid=$1 ORDER BY updated_at,id", iccid)
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		profiles := []apnProfile{}
		for rows.Next() {
			var profile apnProfile
			var data []byte
			if rows.Scan(&profile.ID, &data) != nil || json.Unmarshal(data, &profile.APNConfig) != nil {
				rows.Close()
				fail(w, 503, "DATABASE_UNAVAILABLE")
				return
			}
			profile.HasPassword = profile.Password != ""
			profile.Password = ""
			profiles = append(profiles, profile)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		current := []hardware.APNContext{}
		issue := ""
		if s.modules != nil {
			current, err = s.modules.accessAPN(ctx, v, iccid, nil)
			if err != nil {
				issue = apnIssue(err)
			}
		}
		if current == nil {
			current = []hardware.APNContext{}
		}
		reply(w, 200, map[string]any{"data": map[string]any{"profiles": profiles, "current": current, "issue": issue}})
		return
	}
	if len(parts) < 5 || !jobIDPattern.MatchString(parts[4]) {
		fail(w, 400, "APN_INVALID")
		return
	}
	id := parts[4]
	if len(parts) == 5 && r.Method == http.MethodPut {
		var input struct {
			hardware.APNConfig
			PreservePassword bool `json:"preservePassword"`
		}
		if !decodeBody(w, r, &input) {
			return
		}
		if input.PreservePassword && input.Auth != "NONE" {
			var old []byte
			err := s.db.QueryRow(ctx, "SELECT configuration FROM card_apn_profiles WHERE iccid=$1 AND id=$2", iccid, id).Scan(&old)
			var config hardware.APNConfig
			if err != nil || json.Unmarshal(old, &config) != nil {
				fail(w, 409, "APN_NOT_FOUND")
				return
			}
			input.Password = config.Password
		}
		if !hardware.ValidAPNConfig(input.APNConfig) {
			fail(w, 400, "APN_INVALID")
			return
		}
		encoded, _ := json.Marshal(input.APNConfig)
		_, err := s.db.Exec(ctx, `INSERT INTO card_apn_profiles(iccid,id,configuration) VALUES($1,$2,$3) ON CONFLICT(iccid,id) DO UPDATE SET configuration=$3,updated_at=now()`, iccid, id, encoded)
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{"id": id, "saved": true}})
		return
	}
	if len(parts) == 5 && r.Method == http.MethodDelete {
		if _, err := s.db.Exec(ctx, "DELETE FROM card_apn_profiles WHERE iccid=$1 AND id=$2", iccid, id); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		reply(w, 200, map[string]any{"data": nil})
		return
	}
	if len(parts) == 6 && parts[5] == "apply" && r.Method == http.MethodPost {
		var data []byte
		err := s.db.QueryRow(ctx, "SELECT configuration FROM card_apn_profiles WHERE iccid=$1 AND id=$2", iccid, id).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 404, "APN_NOT_FOUND")
			return
		}
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		var config hardware.APNConfig
		if json.Unmarshal(data, &config) != nil || !hardware.ValidAPNConfig(config) {
			fail(w, 400, "APN_INVALID")
			return
		}
		if s.modules == nil {
			fail(w, 503, "DEVICE_UNAVAILABLE")
			return
		}
		current, err := s.modules.accessAPN(ctx, v, iccid, &config)
		if err != nil {
			fail(w, 409, apnIssue(err))
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{"current": current, "applied": true, "dataEnabled": false}})
		return
	}
	w.Header().Set("Allow", "GET, PUT, DELETE, POST")
	fail(w, 405, "METHOD_NOT_ALLOWED")
}
func apnIssue(err error) string {
	if err == nil {
		return ""
	}
	for _, code := range []string{"APN_READ_FAILED", "APN_INVALID", "APN_SYSTEM_CONTEXT", "APN_APPLY_UNCONFIRMED", "DEVICE_CHANGED", "DEVICE_BUSY", "COMMAND_UNSUPPORTED", "WIFI_DATA_ACTIVE", "WIFI_DATA_STATE_UNKNOWN"} {
		if err.Error() == code {
			return code
		}
	}
	return "APN_READ_FAILED"
}
