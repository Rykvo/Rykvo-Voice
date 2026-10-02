package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
)

type messageInput struct {
	bulk         bool
	RequestID    string `json:"requestId"`
	ModuleID     string `json:"moduleId"`
	LineID       string `json:"lineId,omitempty"`
	To           string `json:"to"`
	Text         string `json:"text"`
	Image        string `json:"image"`
	AttachmentID string `json:"attachmentId,omitempty"`
	CardVersion  int64  `json:"cardVersion,omitempty"`
	ExpiresIn    int    `json:"expiresIn,omitempty"`
}

type serviceError struct {
	Status int
	Code   string
}

func (e *serviceError) Error() string        { return e.Code }
func rejected(status int, code string) error { return &serviceError{status, code} }

// Both entry points persist the same task. A timeout never causes a network resend.
func (s *server) enqueueMessage(ctx context.Context, in messageInput, developerRevision int64) (messageView, bool, error) {
	var empty messageView
	if !jobIDPattern.MatchString(in.RequestID) || in.ExpiresIn < 0 || in.ExpiresIn > 86400 || in.CardVersion < 0 ||
		!hardware.ValidSMS(in.To, in.Text) && !(in.Text == "" && (in.Image != "" || in.AttachmentID != "") && hardware.ValidSMS(in.To, "x")) ||
		in.Image != "" && in.AttachmentID != "" {
		return empty, false, rejected(400, "INVALID_MESSAGE")
	}
	raw, _ := json.Marshal(in)
	hash := messageDigest(string(raw))
	// Resolve retries before looking at today's card or expiring temporary uploads.
	old, found, err := s.previousMessage(ctx, s.db, in.RequestID, hash)
	if err != nil || found {
		return old, false, err
	}
	if in.AttachmentID != "" {
		if developerRevision == 0 {
			return empty, false, rejected(400, "INVALID_MESSAGE")
		}
		err = s.db.QueryRow(ctx, "SELECT content FROM developer_uploads WHERE id=$1 AND created_at>now()-make_interval(hours=>(SELECT upload_hours FROM retention_settings))", in.AttachmentID).Scan(&in.Image)
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, false, rejected(404, "ATTACHMENT_EXPIRED")
		}
		if err != nil {
			return empty, false, err
		}
	}
	part, err := messageImage(in.Image)
	if err != nil {
		return empty, false, rejected(400, err.Error())
	}
	var module int64
	if _, err = fmt.Sscanf(in.ModuleID, "module-%d", &module); err != nil || module < 1 || moduleID(module) != in.ModuleID {
		return empty, false, rejected(400, "INVALID_MODULE")
	}
	if s.modules == nil {
		return empty, false, rejected(503, "DEVICE_UNAVAILABLE")
	}
	s.modules.mu.RLock()
	sample, ok := s.modules.values[module]
	current, present := s.modules.seen[sample.Candidate.Key]
	line := wifiLine(sample.Reading)
	valid := ok && present && sameEndpoint(current, sample.Candidate) && sample.Reading.SIM == "READY" && validICCID(sample.Reading.ICCID) &&
		(in.LineID == line || developerRevision > 0 && in.LineID == "") && !s.modules.jobs[module].active() && time.Since(s.modules.lastScan) < 20*time.Second
	s.modules.mu.RUnlock()
	if !valid {
		return empty, false, rejected(409, "DEVICE_CHANGED")
	}
	kind := "sms"
	if part != nil {
		kind = "mms"
	}
	expires := in.ExpiresIn
	if expires == 0 {
		expires = 600
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(context.Background())
	// Only the short enqueue transaction is global, never hardware or HTTP I/O.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(-734902)"); err != nil {
		return empty, false, err
	}
	if developerRevision > 0 {
		var revision int64
		if err = tx.QueryRow(ctx, "SELECT revision FROM developer_settings WHERE octet_length(key_hash)>0 FOR SHARE").Scan(&revision); err != nil || revision != developerRevision {
			if err == nil || errors.Is(err, pgx.ErrNoRows) {
				err = rejected(401, "API_UNAUTHENTICATED")
			}
			return empty, false, err
		}
	}
	old, found, err = s.previousMessage(ctx, tx, in.RequestID, hash)
	if err != nil || found {
		return old, false, err
	}
	var card string
	var epoch int64
	if err = tx.QueryRow(ctx, "SELECT active_card,card_epoch FROM modules WHERE id=$1 FOR SHARE", module).Scan(&card, &epoch); err != nil {
		return empty, false, err
	}
	if card != sample.Reading.ICCID || epoch == 0 || in.CardVersion > 0 && epoch != in.CardVersion {
		return empty, false, rejected(409, "DEVICE_CHANGED")
	}
	var count, queued, total int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM messages WHERE mine AND module_id=$1 AND created_at>now()-interval '1 minute'", module).Scan(&count); err != nil {
		return empty, false, err
	}
	if count >= 10 {
		return empty, false, rejected(429, "MESSAGE_RATE_LIMIT")
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE module_id=$1),count(*) FROM messages WHERE mine AND deleted_at IS NULL AND state IN ('queued','waiting_network','sending')`, module).Scan(&queued, &total); err != nil {
		return empty, false, err
	}
	if queued >= 100 || total >= 2000 {
		return empty, false, rejected(429, "MESSAGE_QUEUE_FULL")
	}
	_, err = tx.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,image,state,request_hash,expires_at,metadata)
 VALUES($1,$2,$3,$4,$5,true,$6,$7,$8,'queued',$9,now()+make_interval(secs=>$10),jsonb_build_object('bulk',$11::boolean))`, in.RequestID, module, card, line, in.To, kind, in.Text, in.Image, hash, expires, in.bulk)
	if err != nil {
		return empty, false, err
	}
	v, err := scanMessage(tx.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", in.RequestID))
	if err != nil {
		return empty, false, err
	}
	return v, true, tx.Commit(ctx)
}

func (s *server) previousMessage(ctx context.Context, db queryer, id, hash string) (messageView, bool, error) {
	var stored string
	err := db.QueryRow(ctx, "SELECT request_hash FROM messages WHERE id=$1", id).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return messageView{}, false, expiredRequest(ctx, db, id, hash)
	}
	if err != nil {
		return messageView{}, false, err
	}
	if stored != hash {
		return messageView{}, true, rejected(409, "REQUEST_CONFLICT")
	}
	v, err := scanMessage(db.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", id))
	return v, true, err
}
