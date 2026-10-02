package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *server) developerEventList(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after := int64(0)
	var err error
	if q.Has("after") {
		after, err = strconv.ParseInt(q.Get("after"), 10, 64)
	}
	if err != nil || after < 0 {
		fail(w, 400, "INVALID_CURSOR")
		return
	}
	var floor int64
	if err = s.db.QueryRow(ctx, "SELECT event_floor FROM developer_settings").Scan(&floor); err != nil {
		writeServiceError(w, err)
		return
	}
	if after > 0 && after < floor {
		fail(w, 410, "CURSOR_EXPIRED")
		return
	}
	if after == 0 {
		after = floor
	}
	rows, err := s.db.Query(ctx, "SELECT id,data FROM developer_events WHERE id>$1 ORDER BY id LIMIT 101", after)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer rows.Close()
	items := []json.RawMessage{}
	more := false
	for rows.Next() {
		var id int64
		var data []byte
		if err = rows.Scan(&id, &data); err != nil {
			writeServiceError(w, err)
			return
		}
		if len(items) == 100 {
			more = true
			break
		}
		after = id
		items = append(items, json.RawMessage(data))
	}
	if err = rows.Err(); err != nil {
		writeServiceError(w, err)
		return
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items, "cursor": after, "more": more}})
}
func (s *server) webhookAdminAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/settings/developer/webhook")
	if path == "" && r.Method == "GET" {
		rows, err := s.db.Query(ctx, `SELECT id,event_type,state,attempts,issue,response_status,created_at,next_at FROM developer_events
   WHERE destination<>'' ORDER BY id DESC LIMIT 50`)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id int64
			var kind, state, issue string
			var attempts, status int
			var created, next time.Time
			if err = rows.Scan(&id, &kind, &state, &attempts, &issue, &status, &created, &next); err != nil {
				writeServiceError(w, err)
				return
			}
			items = append(items, map[string]any{"id": strconv.FormatInt(id, 10), "type": kind, "state": state, "attempts": attempts, "issue": issue, "responseStatus": status, "createdAt": created, "nextAt": next})
		}
		if err = rows.Err(); err != nil {
			writeServiceError(w, err)
			return
		}
		reply(w, 200, map[string]any{"data": items})
		return
	}
	if path == "/test" && r.Method == "POST" {
		var body struct{}
		if !decodeBody(w, r, &body) {
			return
		}
		config, err := s.readDeveloper(ctx)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if config.Webhook == "" || config.Secret == "" {
			fail(w, 409, "WEBHOOK_NOT_CONFIGURED")
			return
		}
		if !s.developerLimits.allow("webhook-test", 3) {
			fail(w, 429, "API_RATE_LIMIT")
			return
		}
		var id int64
		err = s.db.QueryRow(ctx, `SELECT developer_emit('webhook.test','test','{"test":true,"message":"连接测试，不发送短信或彩信"}'::jsonb)`).Scan(&id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		reply(w, 202, map[string]any{"data": map[string]any{"id": strconv.FormatInt(id, 10)}})
		return
	}
	if path == "/replay" && r.Method == "POST" {
		var in struct {
			ID string `json:"id"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		id, err := strconv.ParseInt(in.ID, 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "INVALID_INPUT")
			return
		}
		if !s.developerLimits.allow("webhook-replay", 30) {
			fail(w, 429, "API_RATE_LIMIT")
			return
		}
		// Replaying a notification never resubmits the original SMS/MMS task.
		err = s.db.QueryRow(ctx, `UPDATE developer_events SET state='pending',attempts=0,issue='',lease='',next_at=now(),finished_at=NULL
   WHERE id=$1 AND state IN ('failed','cancelled') AND EXISTS(SELECT 1 FROM developer_settings WHERE webhook=destination AND webhook_secret=secret AND webhook<>'') RETURNING id`, id).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 409, "WEBHOOK_REPLAY_UNAVAILABLE")
			return
		}
		if err != nil {
			writeServiceError(w, err)
			return
		}
		reply(w, 202, map[string]any{"data": map[string]any{"id": in.ID}})
		return
	}
	fail(w, 404, "NOT_FOUND")
}
