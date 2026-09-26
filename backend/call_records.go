package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/telephony"
)

type callRecord struct {
	ID        string     `json:"id"`
	Account   string     `json:"sipAccountId"`
	Module    string     `json:"moduleId"`
	Number    string     `json:"number"`
	Direction string     `json:"direction"`
	State     string     `json:"state"`
	Status    string     `json:"status"`
	Started   time.Time  `json:"startedAt"`
	Answered  *time.Time `json:"answeredAt"`
	Ended     *time.Time `json:"endedAt"`
	Duration  *float64   `json:"duration"`
}

type callCursor struct {
	At time.Time
	ID string
}
type callRecordFilter struct {
	Account  string
	From, To time.Time
	Before   callCursor
}

func parseCallRecordFilter(q url.Values) (f callRecordFilter, err error) {
	f.Account = q.Get("accountId")
	if !sipAccountID.MatchString(f.Account) {
		return f, errors.New("account")
	}
	f.From, err = time.Parse(time.RFC3339Nano, q.Get("from"))
	if err != nil {
		return
	}
	f.To, err = time.Parse(time.RFC3339Nano, q.Get("to"))
	if err != nil {
		return
	}
	if !f.To.After(f.From) || f.To.Sub(f.From) > 26*time.Hour {
		return f, errors.New("date range")
	}
	if s := q.Get("before"); s != "" {
		if len(s) > 512 {
			return f, errors.New("cursor")
		}
		b, e := base64.RawURLEncoding.DecodeString(s)
		if e != nil || json.Unmarshal(b, &f.Before) != nil || f.Before.At.Before(f.From) || !f.Before.At.Before(f.To) || len(f.Before.ID) != 32 {
			return f, errors.New("cursor")
		}
		if _, e = hex.DecodeString(f.Before.ID); e != nil {
			return f, errors.New("cursor")
		}
	}
	return
}

// Keep the existing per-call rounded-minute display, not a carrier billing claim.
const callDurationSQL = `CASE WHEN answered_at IS NOT NULL AND ended_at IS NOT NULL THEN GREATEST(0,EXTRACT(EPOCH FROM ended_at-answered_at)) ELSE NULL END`
const callFilterSQL = `account_id=$1 AND started_at >= $2 AND started_at < $3`

func (s *server) callRecordsAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	f, err := parseCallRecordFilter(r.URL.Query())
	if err != nil {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sip_accounts WHERE id=$1)`, f.Account).Scan(&exists); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if !exists {
		fail(w, 404, "NOT_FOUND")
		return
	}
	var minutes int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN answered_at IS NOT NULL AND ended_at IS NOT NULL THEN GREATEST(1,CEIL((`+callDurationSQL+`)/60)) ELSE 0 END),0)::bigint FROM sip_call_records WHERE `+callFilterSQL, f.Account, f.From, f.To).Scan(&minutes); err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if r.URL.Path == "/api/call-records/stats" {
		reply(w, 200, map[string]any{"data": map[string]any{"totalMinutes": minutes}})
		return
	}
	args := []any{f.Account, f.From, f.To}
	where := callFilterSQL
	if f.Before.ID != "" {
		where += ` AND (started_at,id)<($4,$5)`
		args = append(args, f.Before.At, f.Before.ID)
	}
	rows, err := tx.Query(ctx, `SELECT id,account_id,module_id,peer,direction,state,outcome,started_at,answered_at,ended_at,`+callDurationSQL+` FROM sip_call_records WHERE `+where+` ORDER BY started_at DESC,id DESC LIMIT 101`, args...)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer rows.Close()
	items := make([]callRecord, 0, 100)
	for rows.Next() {
		var v callRecord
		if rows.Scan(&v.ID, &v.Account, &v.Module, &v.Number, &v.Direction, &v.State, &v.Status, &v.Started, &v.Answered, &v.Ended, &v.Duration) != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		v.Status = callRecordStatus(v)
		items = append(items, v)
	}
	if rows.Err() != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	cursor := ""
	if len(items) > 100 {
		items = items[:100]
		last := items[99]
		b, _ := json.Marshal(callCursor{last.Started, last.ID})
		cursor = base64.RawURLEncoding.EncodeToString(b)
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items, "nextCursor": cursor, "totalMinutes": minutes}})
}

func callRecordStatus(v callRecord) string {
	if v.Answered != nil {
		if v.State == "connected" && v.Ended == nil {
			return "active"
		}
		return "connected"
	}
	if v.Status != "" {
		return v.Status
	}
	switch v.State {
	case "dialing", "ringing":
		return v.State
	case "interrupted":
		return "interrupted"
	}
	// Older records did not save the reason; never invent one during migration.
	return "unconnected"
}

func (s *server) startCallRecord(ctx context.Context, req *sip.Request, account string) (string, error) {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(seed[:])
	if s.db == nil {
		return id, nil
	}
	_, err := s.db.Exec(ctx, `INSERT INTO sip_call_records(id,account_id,module_id,peer,direction) VALUES($1,$2,'',$3,'outgoing')`, id, account, req.Recipient.User)
	return id, err
}

func (s *server) endCallRecord(id, module, state, outcome string, ended time.Time) {
	if id == "" || s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `UPDATE sip_call_records SET module_id=$2,state=$3,outcome=CASE WHEN outcome='' THEN $4 ELSE outcome END,ended_at=COALESCE(ended_at,$5) WHERE id=$1`, id, module, state, outcome, ended)
	if err != nil {
		log.Print("SIP call record final update failed")
	}
}

func (c *sipOutgoing) result(reason string) {
	c.mu.Lock()
	if c.outcome == "" {
		c.outcome = reason
	}
	c.mu.Unlock()
}

func (c *sipOutgoing) recordState(state string) {
	if c.record == "" || c.owner.g.server.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.owner.g.server.db.Exec(ctx, `UPDATE sip_call_records SET module_id=$3,state=$2,answered_at=CASE WHEN $2='connected' THEN COALESCE(answered_at,now()) ELSE answered_at END WHERE id=$1 AND ended_at IS NULL`, c.record, state, c.call.Module)
	if err != nil {
		log.Print("SIP call record update failed")
	}
}

func (c *sipOutgoing) endRecord(state string) {
	c.mu.Lock()
	if c.endedAt.IsZero() {
		c.endedAt = time.Now()
	}
	outcome := c.outcome
	if outcome == "" {
		outcome = "failed"
	}
	ended := c.endedAt
	c.mu.Unlock()
	c.owner.g.server.endCallRecord(c.record, c.call.Module, state, outcome, ended)
}

func carrierCallResult(code int) string {
	switch code {
	case 486, 600:
		return "busy"
	case 603:
		return "rejected"
	case 403:
		return "carrier_rejected"
	case 404, 604:
		return "number_not_found"
	case 480:
		return "peer_unavailable"
	case 408, 504:
		return "call_timeout"
	case 488, 606:
		return "unsupported_audio"
	case 500, 502, 503:
		return "carrier_unavailable"
	}
	return "failed"
}

func moduleCallResult(err error) string {
	if err == nil {
		return "failed"
	}
	switch err.Error() {
	case "NO_SIM", "VOICE_SIM_ERROR":
		return "card_error"
	case "VOICE_NOT_REGISTERED":
		return "not_registered"
	case "VOICE_RADIO_UNAVAILABLE":
		return "radio_unavailable"
	case "VOICE_BUSY", "VOICE_AUDIO_BUSY", "DEVICE_BUSY":
		return "module_busy"
	case "VOICE_REMOTE_BUSY":
		return "busy"
	case "VOICE_NO_ANSWER":
		return "no_answer"
	case "VOICE_UNSUPPORTED":
		return "unsupported_audio"
	case "DEVICE_CHANGED", "VOICE_STATE_STALE", "VOICE_STREAM_CLOSED", "VOICE_NOT_READY", "VOICE_STATE_UNKNOWN":
		return "module_error"
	}
	return "failed"
}

func actionCallResult(reason string) string {
	switch reason {
	case "replaced", "credentials-changed", "account-deleted", "logout", "permission-changed", "registration-expired":
		return "account_revoked"
	case "module-unavailable":
		return "module_error"
	case "timeout":
		return "call_timeout"
	}
	return ""
}

// A failed pool selection is only attributed to a cause shared by every candidate.
func (c *sipCalls) unavailableResult(ctx context.Context, account string, dialErr error) (module, reason string) {
	if errors.Is(dialErr, telephony.ErrBusy) {
		reason = "account_busy"
	}
	if errors.Is(dialErr, telephony.ErrModuleBusy) {
		reason = "module_busy"
	}
	if c.g.server.db == nil || c.g.server.modules == nil {
		if reason != "" {
			return "", reason
		}
		return "", "no_available_module"
	}
	rows, err := c.g.server.db.Query(ctx, `SELECT m.id FROM modules m JOIN sip_accounts a ON a.id=$1 WHERE a.allocation='all' OR EXISTS(SELECT 1 FROM sip_account_modules am WHERE am.account_id=a.id AND am.module_id=m.id) ORDER BY m.id`, account)
	if err != nil {
		return "", "no_available_module"
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			return "", "no_available_module"
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || len(ids) == 0 {
		return "", "no_available_module"
	}
	if len(ids) == 1 {
		module = moduleID(ids[0])
	}
	if reason != "" {
		return module, reason
	}
	m := c.g.server.modules
	m.mu.RLock()
	defer m.mu.RUnlock()
	if time.Since(m.lastScan) > 20*time.Second {
		return module, "no_available_module"
	}
	for _, id := range ids {
		r := "no_available_module"
		v, ok := m.values[id]
		current, present := m.seen[v.Candidate.Key]
		if ok && present && sameEndpoint(current, v.Candidate) {
			reading := v.Reading
			switch {
			case reading.UpdatedAt.IsZero() || time.Since(reading.UpdatedAt) > 20*time.Second:
				r = "no_available_module"
			case !reading.Responsive:
				r = "module_error"
			case reading.SIM == "absent" || strings.Contains(reading.SIM, "PIN") || strings.Contains(reading.SIM, "PUK"):
				r = "card_error"
			case m.wifi[id] != nil && (m.wifi[id].Enabled || m.wifi[id].running):
				if !m.wifi[id].Registered {
					r = "not_registered"
				}
			case reading.Registration == "not_registered" || reading.Registration == "searching" || reading.Registration == "denied":
				r = "not_registered"
			}
		}
		if reason != "" && reason != r {
			return module, "no_available_module"
		}
		reason = r
	}
	return
}
