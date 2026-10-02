package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *server) messageSearchAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("text"))
	if query == "" || len(query) > 512 {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	rows, err := s.db.Query(ctx, `SELECT module_id,line_id,peer FROM messages WHERE deleted_at IS NULL AND state<>'mms_report'
 AND strpos(lower(body),lower($1))>0 GROUP BY module_id,line_id,peer ORDER BY max(created_at) DESC,module_id,line_id,peer LIMIT 201`, query)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer rows.Close()
	items := []messageView{}
	for rows.Next() {
		var item messageView
		var module int64
		if err = rows.Scan(&module, &item.LineID, &item.Number); err != nil {
			writeServiceError(w, err)
			return
		}
		item.SenderID = moduleID(module)
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		writeServiceError(w, err)
		return
	}
	more := len(items) > 200
	if more {
		items = items[:200]
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items, "more": more}})
}

func (s *server) messageThreadsAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.deleteMessageThreads(ctx, w, r)
		return
	}
	if r.Method != http.MethodGet {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	q := r.URL.Query()
	contactAfter, err := strconv.ParseInt("0"+q.Get("contactAfter"), 10, 64)
	if err != nil || contactAfter < 0 {
		fail(w, 400, "INVALID_CURSOR")
		return
	}
	page, err := s.beginMessageSync(ctx, q)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer page.tx.Rollback(ctx)
	rows, err := page.tx.Query(ctx, `SELECT COALESCE(m.id,''),t.module_id,t.line_id,t.peer,COALESCE(m.body,''),COALESCE(m.image<>'',false),COALESCE(m.mine,false),COALESCE(m.kind,'sms'),COALESCE(m.state,''),COALESCE(m.issue,''),COALESCE(m.created_at,TIMESTAMPTZ 'epoch'),t.revision,m.id IS NULL
 FROM message_threads t LEFT JOIN messages m ON m.id=t.last_id AND m.deleted_at IS NULL
 WHERE t.revision>$1 AND t.revision<=$2 ORDER BY t.revision LIMIT 201`, page.after, page.snapshot)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	items := []messageView{}
	for rows.Next() {
		item, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			writeServiceError(w, e)
			return
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	more := len(items) > 200
	after := page.after
	if more {
		items = items[:200]
		after = items[len(items)-1].Revision
	}
	contacts, err := messageContacts(ctx, page.tx, contactAfter)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if len(contacts) > 0 {
		contactAfter = contacts[len(contacts)-1].Revision
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items, "cursor": page.cursor(after, more), "snapshot": page.snapshot, "more": more || len(contacts) == 200, "contacts": contacts, "contactCursor": contactAfter}})
}

type messageThreadScope struct {
	ModuleID string   `json:"moduleId"`
	LineID   string   `json:"lineId"`
	Numbers  []string `json:"numbers"`
}

func (v messageThreadScope) valid() bool {
	if _, err := parseModuleID(v.ModuleID); err != nil || v.LineID == "" || len(v.LineID) > 128 || len(v.Numbers) < 1 || len(v.Numbers) > 32 {
		return false
	}
	for _, n := range v.Numbers {
		if n == "" || len(n) > 128 {
			return false
		}
	}
	return true
}

// Keyset pagination keeps the current conversation bounded without hiding old messages.
func (s *server) messageWindowAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	q := r.URL.Query()
	var scope messageThreadScope
	scope.ModuleID = q.Get("moduleId")
	scope.LineID = q.Get("lineId")
	if json.Unmarshal([]byte(q.Get("numbers")), &scope.Numbers) != nil || !scope.valid() {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	module, _ := parseModuleID(scope.ModuleID)
	direction, anchor := q.Get("direction"), q.Get("anchor")
	if direction != "" && direction != "earlier" && direction != "newer" && direction != "current" || len(anchor) > 128 || direction != "" && anchor == "" {
		fail(w, 400, "INVALID_CURSOR")
		return
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var at time.Time
	if direction != "" {
		err = tx.QueryRow(ctx, `SELECT created_at FROM messages WHERE id=$1 AND module_id=$2 AND line_id=$3 AND peer=ANY($4)`, anchor, module, scope.LineID, scope.Numbers).Scan(&at)
		if err == pgx.ErrNoRows {
			fail(w, 410, "HISTORY_EXPIRED")
			return
		}
		if err != nil {
			writeServiceError(w, err)
			return
		}
	}
	clause, order := "", "DESC"
	args := []any{module, scope.LineID, scope.Numbers}
	if direction != "" {
		op := "<"
		if direction == "newer" {
			op = ">"
			order = "ASC"
		}
		if direction == "current" {
			op = "<="
		}
		clause = " AND (created_at,id)" + op + "($4,$5)"
		args = append(args, at, anchor)
	}
	base := ` FROM messages WHERE module_id=$1 AND line_id=$2 AND peer=ANY($3) AND deleted_at IS NULL AND state<>'mms_report'`
	rows, err := tx.Query(ctx, "SELECT "+messageColumns+base+clause+" ORDER BY created_at "+order+",id "+order+" LIMIT 80", args...)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	items := []messageView{}
	for rows.Next() {
		v, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			writeServiceError(w, e)
			return
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if order == "DESC" {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	earlier, newer := false, false
	if len(items) > 0 {
		// Use stored microsecond timestamps, not the display's millisecond values.
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1`+base+` AND (created_at,id)<(SELECT created_at,id FROM messages WHERE id=$4)),EXISTS(SELECT 1`+base+` AND (created_at,id)>(SELECT created_at,id FROM messages WHERE id=$5))`, module, scope.LineID, scope.Numbers, items[0].ID, items[len(items)-1].ID).Scan(&earlier, &newer)
		if err != nil {
			writeServiceError(w, err)
			return
		}
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items, "earlier": earlier, "newer": newer}})
}

func (s *server) deleteMessageThreads(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var in struct {
		messageThreadScope
		All      bool  `json:"all"`
		Snapshot int64 `json:"snapshot"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF || in.Snapshot < 0 || !in.All && !in.valid() || in.All && (in.ModuleID != "" || in.LineID != "" || len(in.Numbers) > 0) {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(-734901)"); err != nil {
		writeServiceError(w, err)
		return
	}
	if in.Snapshot == 0 {
		if err = tx.QueryRow(ctx, "SELECT COALESCE(max(revision),0) FROM messages").Scan(&in.Snapshot); err != nil {
			writeServiceError(w, err)
			return
		}
	}
	args := []any{in.Snapshot}
	filter := ""
	if !in.All {
		module, _ := parseModuleID(in.ModuleID)
		args = append(args, module, in.LineID, in.Numbers)
		filter = " AND module_id=$2 AND line_id=$3 AND peer=ANY($4)"
	}
	result, err := tx.Exec(ctx, `UPDATE messages SET deleted_at=now(),state=CASE WHEN state IN ('queued','waiting_network') THEN 'cancelled' ELSE state END WHERE id IN
 (SELECT id FROM messages WHERE deleted_at IS NULL AND revision<=$1 AND state<>'mms_report'`+filter+` ORDER BY revision LIMIT 256)`, args...)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeServiceError(w, err)
		return
	}
	reply(w, 200, map[string]any{"data": map[string]any{"snapshot": in.Snapshot, "more": result.RowsAffected() == 256}})
}
