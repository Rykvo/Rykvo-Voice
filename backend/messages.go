package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/mms"
)

func messageDigest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }

type messageView struct {
	ID       string `json:"id"`
	SenderID string `json:"senderId"`
	LineID   string `json:"lineId"`
	Number   string `json:"number"`
	Text     string `json:"text"`
	Image    string `json:"image"`
	Mine     bool   `json:"mine"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Issue    string `json:"issue"`
	At       int64  `json:"at"`
	Revision int64  `json:"revision"`
	Deleted  bool   `json:"deleted"`
}

const messageColumns = "id,module_id,line_id,peer,body,image<>'',mine,kind,state,issue,created_at,revision,deleted_at IS NOT NULL"

func scanMessage(row interface{ Scan(...any) error }) (messageView, error) {
	var v messageView
	var module int64
	var hasImage bool
	var at time.Time
	e := row.Scan(&v.ID, &module, &v.LineID, &v.Number, &v.Text, &hasImage, &v.Mine, &v.Kind, &v.State, &v.Issue, &at, &v.Revision, &v.Deleted)
	v.SenderID = moduleID(module)
	v.At = at.UnixMilli()
	if hasImage {
		v.Image = v.ID
	}
	return v, e
}
func messageImage(raw string) (*mms.Part, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 1500000 {
		return nil, errors.New("MMS_TOO_LARGE")
	}
	kind, b64, ok := strings.Cut(raw, ";base64,")
	if !ok {
		return nil, errors.New("INVALID_IMAGE")
	}
	kind = strings.TrimPrefix(kind, "data:")
	if kind != "image/png" && kind != "image/jpeg" && kind != "image/gif" {
		return nil, errors.New("INVALID_IMAGE")
	}
	b, e := base64.StdEncoding.DecodeString(b64)
	if e != nil || len(b) > 1024*1024 {
		return nil, errors.New("INVALID_IMAGE")
	}
	config, format, e := image.DecodeConfig(strings.NewReader(string(b)))
	if e != nil || config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 24000000 || "image/"+format != kind {
		return nil, errors.New("INVALID_IMAGE")
	}
	return &mms.Part{Type: kind, Data: b}, nil
}
func (s *server) messagesAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/messages")
	id := strings.Trim(path, "/")
	if strings.HasSuffix(id, "/image") && r.Method == "GET" {
		id = strings.TrimSuffix(id, "/image")
		var raw string
		if s.db.QueryRow(ctx, "SELECT image FROM messages WHERE id=$1 AND deleted_at IS NULL", id).Scan(&raw) != nil {
			fail(w, 404, "NOT_FOUND")
			return
		}
		part, e := messageImage(raw)
		if e != nil || part == nil {
			fail(w, 404, "NOT_FOUND")
			return
		}
		w.Header().Set("Content-Type", part.Type)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(part.Data)
		return
	}
	if id != "" {
		if r.Method == "DELETE" {
			_, e := s.db.Exec(ctx, "UPDATE messages SET deleted_at=now(),state=CASE WHEN state IN ('queued','waiting_network') THEN 'cancelled' ELSE state END WHERE id=$1", id)
			if e != nil {
				fail(w, 503, "DATABASE_UNAVAILABLE")
				return
			}
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" {
			fail(w, 405, "METHOD_NOT_ALLOWED")
			return
		}
		v, e := scanMessage(s.db.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", id))
		if e != nil {
			fail(w, 404, "NOT_FOUND")
			return
		}
		reply(w, 200, map[string]any{"data": v})
		return
	}
	if r.Method == "GET" {
		after, e := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if r.URL.Query().Get("after") == "" {
			after = 0
			e = nil
		}
		if e != nil || after < 0 {
			fail(w, 400, "INVALID_INPUT")
			return
		}
		rows, e := s.db.Query(ctx, "SELECT "+messageColumns+" FROM messages WHERE revision>$1 ORDER BY revision LIMIT 200", after)
		if e != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		defer rows.Close()
		items := []messageView{}
		for rows.Next() {
			v, e := scanMessage(rows)
			if e != nil {
				fail(w, 503, "DATABASE_UNAVAILABLE")
				return
			}
			items = append(items, v)
			after = v.Revision
		}
		if rows.Err() != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{"items": items, "cursor": after, "more": len(items) == 200}})
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "JSON_REQUIRED")
		return
	}
	var in struct {
		RequestID string `json:"requestId"`
		ModuleID  string `json:"moduleId"`
		LineID    string `json:"lineId"`
		To        string `json:"to"`
		Text      string `json:"text"`
		Image     string `json:"image"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1600000))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || !jobIDPattern.MatchString(in.RequestID) || (!hardware.ValidSMS(in.To, in.Text) && !(in.Text == "" && in.Image != "" && hardware.ValidSMS(in.To, "x"))) {
		fail(w, 400, "INVALID_MESSAGE")
		return
	}
	part, e := messageImage(in.Image)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	raw, _ := json.Marshal(in)
	hash := messageDigest(string(raw))
	var oldHash string
	if e = s.db.QueryRow(ctx, "SELECT request_hash FROM messages WHERE id=$1", in.RequestID).Scan(&oldHash); e == nil {
		if hash != oldHash {
			fail(w, 409, "REQUEST_CONFLICT")
			return
		}
		v, e := scanMessage(s.db.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", in.RequestID))
		if e != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		reply(w, 200, map[string]any{"data": v})
		return
	} else if !errors.Is(e, pgx.ErrNoRows) {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if s.modules == nil {
		fail(w, 503, "DEVICE_UNAVAILABLE")
		return
	}
	var module int64
	if _, e = fmt.Sscanf(in.ModuleID, "module-%d", &module); e != nil || moduleID(module) != in.ModuleID {
		fail(w, 400, "INVALID_MESSAGE")
		return
	}
	s.modules.mu.RLock()
	sample, ok := s.modules.values[module]
	current, present := s.modules.seen[sample.Candidate.Key]
	valid := ok && present && sameEndpoint(current, sample.Candidate) && sample.Reading.SIM == "READY" && sample.Reading.ICCID != "" && wifiLine(sample.Reading) == in.LineID && !s.modules.jobs[module].active() && time.Since(s.modules.lastScan) < 20*time.Second
	s.modules.mu.RUnlock()
	if !valid {
		fail(w, 409, "DEVICE_CHANGED")
		return
	}
	kind := "sms"
	if part != nil {
		kind = "mms"
	}
	tx, e := s.db.Begin(ctx)
	if e != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	defer tx.Rollback(context.Background())
	// Serialize enqueue/rate limits per module, including concurrent requests.
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", module); e != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	var count int
	e = tx.QueryRow(ctx, "SELECT count(*) FROM messages WHERE mine AND module_id=$1 AND created_at>now()-interval '1 minute'", module).Scan(&count)
	if e != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if count >= 10 {
		fail(w, 429, "MESSAGE_RATE_LIMIT")
		return
	}
	_, e = tx.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,image,state,request_hash) VALUES($1,$2,$3,$4,$5,true,$6,$7,$8,'queued',$9) ON CONFLICT(id) DO NOTHING`, in.RequestID, module, sample.Reading.ICCID, in.LineID, in.To, kind, in.Text, in.Image, hash)
	if e != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	var stored string
	if tx.QueryRow(ctx, "SELECT request_hash FROM messages WHERE id=$1", in.RequestID).Scan(&stored) != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	if stored != hash {
		fail(w, 409, "REQUEST_CONFLICT")
		return
	}
	if tx.Commit(ctx) != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	v, e := scanMessage(s.db.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", in.RequestID))
	if e != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	reply(w, 202, map[string]any{"data": v})
}
