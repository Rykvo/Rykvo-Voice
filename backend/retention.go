package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type retentionSettings struct {
	UploadHours int        `json:"uploadHours"`
	Days        int        `json:"days"`
	Revision    int64      `json:"revision"`
	LastRun     *time.Time `json:"lastRun"`
	Issue       string     `json:"issue"`
}

func (s *server) retentionAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var row pgx.Row
	if r.Method == "PUT" {
		var in struct {
			Days        *int  `json:"days"`
			UploadHours *int  `json:"uploadHours"`
			Revision    int64 `json:"revision"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if in.Days == nil || *in.Days < 0 || *in.Days > 36500 || in.Revision < 1 ||
			in.UploadHours != nil && (*in.UploadHours < 1 || *in.UploadHours > 8760) {
			fail(w, 400, "INVALID_RETENTION")
			return
		}
		row = s.db.QueryRow(ctx, `UPDATE retention_settings SET days=$1,upload_hours=COALESCE($2,upload_hours),revision=revision+1
 WHERE revision=$3 RETURNING days,upload_hours,revision,last_run,issue`, *in.Days, in.UploadHours, in.Revision)
	} else if r.Method == "GET" {
		row = s.db.QueryRow(ctx, "SELECT days,upload_hours,revision,last_run,issue FROM retention_settings")
	} else {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var v retentionSettings
	err := row.Scan(&v.Days, &v.UploadHours, &v.Revision, &v.LastRun, &v.Issue)
	if errors.Is(err, pgx.ErrNoRows) && r.Method == "PUT" {
		fail(w, 409, "SETTINGS_CHANGED")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	reply(w, 200, map[string]any{"data": v})
}
func (s *server) runRetention(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	defer s.retentionStatus.update("retention", "WORKER_STOPPED")
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		more, err := s.cleanRecordsBatch(call)
		cancel()
		if ctx.Err() != nil {
			return
		}
		issue := ""
		if err != nil {
			issue = "RETENTION_FAILED"
		}
		s.retentionStatus.update("retention", issue)
		call, cancel = context.WithTimeout(ctx, 3*time.Second)
		s.db.Exec(call, "UPDATE retention_settings SET last_run=now(),issue=$1", issue)
		cancel()
		delay := time.Minute
		if err == nil && more {
			delay = 250 * time.Millisecond
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

const finishedMessages = `state IN ('accepted','delivered','failed','unknown','expired','cancelled','received','partial','unsupported_push','decode_error','mms_report')`

// Short batches leave current work, identities, settings and recovery journals intact.
func (s *server) cleanRecords(ctx context.Context) error {
	_, err := s.cleanRecordsBatch(ctx)
	return err
}
func (s *server) cleanRecordsBatch(ctx context.Context) (bool, error) {
	more := false
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return more, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(-734905)").Scan(&locked); err != nil || !locked {
		return more, err
	}
	execute := func(query string, args ...any) error {
		result, err := tx.Exec(ctx, query, args...)
		more = more || result.RowsAffected() >= 256
		return err
	}
	var days, uploadHours int
	if err = tx.QueryRow(ctx, "SELECT days,upload_hours FROM retention_settings FOR SHARE").Scan(&days, &uploadHours); err != nil {
		return more, err
	}
	if err = execute(`DELETE FROM developer_uploads WHERE id IN (SELECT id FROM developer_uploads WHERE created_at<now()-make_interval(hours=>$1) ORDER BY created_at LIMIT 256)`, uploadHours); err != nil {
		return more, err
	}
	queries := []string{
		`DELETE FROM message_threads WHERE (module_id,line_id,peer) IN (SELECT module_id,line_id,peer FROM message_threads WHERE last_id IS NULL AND revision<(SELECT floor FROM message_sync_state) ORDER BY revision LIMIT 256)`,
		`DELETE FROM message_requests WHERE id IN (SELECT id FROM message_requests WHERE expires_at<now() ORDER BY expires_at LIMIT 256)`,
		`DELETE FROM message_fingerprints WHERE (iccid,fingerprint) IN (SELECT iccid,fingerprint FROM message_fingerprints WHERE expires_at<now() ORDER BY expires_at LIMIT 256)`,
		`DELETE FROM message_reports WHERE (iccid,fingerprint) IN (SELECT iccid,fingerprint FROM message_reports WHERE received_at<now()-interval '7 days' ORDER BY received_at LIMIT 256)`,
		`DELETE FROM alert_events WHERE id IN (SELECT id FROM alert_events WHERE created_at<now()-interval '7 days' ORDER BY created_at LIMIT 256)`,
	}
	for _, q := range queries {
		if err = execute(q); err != nil {
			return more, err
		}
	}
	if days > 0 {
		err = execute(`UPDATE messages SET deleted_at=now(),body='',image='' WHERE id IN (
   SELECT id FROM messages WHERE deleted_at IS NULL AND created_at<now()-make_interval(days=>$1) AND `+finishedMessages+` AND NOT EXISTS(SELECT 1 FROM developer_events e WHERE e.resource_id=messages.id AND e.state IN ('pending','sending'))
   ORDER BY created_at,id LIMIT 256 FOR UPDATE SKIP LOCKED)`, days)
		if err != nil {
			return more, err
		}
		err = execute(`DELETE FROM sip_call_records WHERE id IN (SELECT id FROM sip_call_records WHERE ended_at IS NOT NULL
   AND ended_at<now()-make_interval(days=>$1) ORDER BY ended_at,id LIMIT 256 FOR UPDATE SKIP LOCKED)`, days)
		if err != nil {
			return more, err
		}
		err = execute(`DELETE FROM alert_notifications WHERE id IN (SELECT n.id FROM alert_notifications n
   WHERE state IN ('sent','failed','unknown','cancelled') AND created_at<now()-make_interval(days=>$1)
   AND NOT EXISTS(SELECT 1 FROM alert_counters c WHERE c.module_id=n.module_id AND c.kind=n.kind AND c.epoch=n.epoch AND c.cycle=n.cycle AND c.active)
   ORDER BY id LIMIT 256 FOR UPDATE SKIP LOCKED)`, days)
		if err != nil {
			return more, err
		}
	}
	// Explicit deletion also releases heavy bodies; tombstones remain for synchronization.
	if err = execute(`UPDATE messages SET body='',image='' WHERE id IN (SELECT id FROM messages
  WHERE deleted_at IS NOT NULL AND (body<>'' OR image<>'') AND ` + finishedMessages + ` ORDER BY deleted_at LIMIT 256 FOR UPDATE SKIP LOCKED)`); err != nil {
		return more, err
	}
	rows, err := tx.Query(ctx, `SELECT id,revision FROM messages WHERE deleted_at<now()-interval '7 days' AND `+finishedMessages+`
  AND NOT EXISTS(SELECT 1 FROM developer_events e WHERE e.resource_id=messages.id AND e.state IN ('pending','sending'))
  ORDER BY deleted_at,id LIMIT 256 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return more, err
	}
	type record struct {
		id       string
		revision int64
	}
	var records []record
	for rows.Next() {
		var v record
		if err = rows.Scan(&v.id, &v.revision); err != nil {
			rows.Close()
			return more, err
		}
		records = append(records, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return more, err
	}
	var floor int64
	ids := make([]string, 0, len(records))
	for _, v := range records {
		floor = max(floor, v.revision)
		ids = append(ids, v.id)
	}
	if len(ids) > 0 {
		for _, query := range []string{
			`INSERT INTO message_requests(id,request_hash,expires_at) SELECT id,request_hash,now()+interval '7 days' FROM messages WHERE id=ANY($1) AND request_hash<>'' ON CONFLICT DO NOTHING`,
			`INSERT INTO message_fingerprints(iccid,fingerprint,expires_at) SELECT iccid,fingerprint,now()+interval '7 days' FROM message_parts WHERE message_id=ANY($1) ON CONFLICT DO NOTHING`,
			`DELETE FROM message_parts WHERE message_id=ANY($1)`,
			`DELETE FROM messages WHERE id=ANY($1)`,
		} {
			if _, err = tx.Exec(ctx, query, ids); err != nil {
				return more, err
			}
		}
		more = more || len(ids) == 256
	}
	if floor > 0 {
		if _, err = tx.Exec(ctx, "UPDATE message_sync_state SET floor=GREATEST(floor,$1)", floor); err != nil {
			return more, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return more, err
	}
	if days > 0 {
		next, err := s.cleanDeveloperEventsBatch(ctx, days)
		return more || next, err
	}
	return more, nil
}
func (s *server) cleanDeveloperEvents(ctx context.Context, days int) error {
	_, err := s.cleanDeveloperEventsBatch(ctx, days)
	return err
}
func (s *server) cleanDeveloperEventsBatch(ctx context.Context, days int) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var current int
	if err = tx.QueryRow(ctx, "SELECT days FROM retention_settings FOR SHARE").Scan(&current); err != nil {
		return false, err
	}
	if current == 0 || current != days {
		return false, nil
	}
	var floor int64
	// Match the settings writer lock order before touching notification rows.
	if err = tx.QueryRow(ctx, "SELECT event_floor FROM developer_settings FOR UPDATE").Scan(&floor); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT id,created_at<now()-make_interval(days=>$1),state FROM developer_events ORDER BY id LIMIT 256 FOR UPDATE`, days)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		var expired bool
		var state string
		if err = rows.Scan(&id, &expired, &state); err != nil {
			rows.Close()
			return false, err
		}
		if !expired || state == "pending" || state == "sending" {
			break
		}
		floor = id
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	deleted, err := tx.Exec(ctx, "DELETE FROM developer_events WHERE id<=$1", floor)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, "UPDATE developer_settings SET event_floor=GREATEST(event_floor,$1)", floor); err != nil {
		return false, err
	}
	return deleted.RowsAffected() == 256, tx.Commit(ctx)
}
func expiredRequest(ctx context.Context, db queryer, id, hash string) error {
	var old string
	err := db.QueryRow(ctx, "SELECT request_hash FROM message_requests WHERE id=$1 AND expires_at>now()", id).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if old != hash {
		return rejected(409, "REQUEST_CONFLICT")
	}
	return rejected(410, "REQUEST_EXPIRED")
}
