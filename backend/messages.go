package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	v.Deleted = v.Deleted || v.State == "mms_report"
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
	if len(b) > 1024*1024 {
		return nil, errors.New("MMS_TOO_LARGE")
	}
	if e != nil {
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
	if id == "threads" {
		s.messageThreadsAPI(ctx, w, r)
		return
	}
	if id == "window" {
		s.messageWindowAPI(ctx, w, r)
		return
	}
	if id == "search" {
		s.messageSearchAPI(ctx, w, r)
		return
	}
	if id == "contacts" {
		s.messageContactAPI(ctx, w, r)
		return
	}
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
		contactAfter := int64(0)
		if r.URL.Query().Has("contactAfter") {
			n, err := strconv.ParseInt(r.URL.Query().Get("contactAfter"), 10, 64)
			if err != nil || n < 0 {
				fail(w, 400, "INVALID_CURSOR")
				return
			}
			contactAfter = n
		}
		page, e := s.beginMessageSync(ctx, r.URL.Query())
		if e != nil {
			writeServiceError(w, e)
			return
		}
		defer page.tx.Rollback(ctx)
		after := page.after
		rows, e := page.tx.Query(ctx, "SELECT "+messageColumns+" FROM messages WHERE revision>$1 AND revision<=$2 ORDER BY revision LIMIT 201", after, page.snapshot)
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
		rows.Close()
		more := len(items) > 200
		if more {
			items = items[:200]
			after = items[len(items)-1].Revision
		}
		after = page.cursor(after, more)
		contacts, e := messageContacts(ctx, page.tx, contactAfter)
		if e != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		if len(contacts) > 0 {
			contactAfter = contacts[len(contacts)-1].Revision
		}
		reply(w, 200, map[string]any{"data": map[string]any{"items": items, "cursor": after, "more": more || len(contacts) == 200, "snapshot": page.snapshot, "contacts": contacts, "contactCursor": contactAfter}})
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var in messageInput
	if !decodeMessageBody(w, r, &in) {
		return
	}
	v, created, err := s.enqueueMessage(ctx, in, 0)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := 200
	if created {
		status = 202
	}
	reply(w, status, map[string]any{"data": v})
}
