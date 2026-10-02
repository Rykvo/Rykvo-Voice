package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
)

func testMessageThreadsDatabase(t *testing.T, s *server) {
	ctx := context.Background()
	var module int64
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('history-fixture','history','fixture','History','history') RETURNING id`).Scan(&module); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,created_at)
 SELECT 'history-'||lpad(n::text,4,'0'),$1,'8986000000000090666','history-card','+12025550123',true,'sms','fixture-'||n,'accepted',TIMESTAMPTZ '2026-01-01' FROM generate_series(1,1000) n`, module); err != nil {
		t.Fatal(err)
	}
	type response struct {
		Items                []messageView
		Cursor, Snapshot     int64
		More, Earlier, Newer bool
	}
	read := func(path string, want int) response {
		t.Helper()
		out := httptest.NewRecorder()
		s.messagesAPI(ctx, out, httptest.NewRequest("GET", path, nil))
		if out.Code != want {
			t.Fatalf("%s: %d %s", path, out.Code, out.Body.String())
		}
		var result struct{ Data response }
		if want == 200 && json.Unmarshal(out.Body.Bytes(), &result) != nil {
			t.Fatal(out.Body.String())
		}
		return result.Data
	}
	summary := read("/api/messages/threads", 200)
	var count int
	for _, item := range summary.Items {
		if item.LineID == "history-card" {
			count++
			if item.ID != "history-1000" {
				t.Fatal("summary loaded history", item)
			}
		}
	}
	if count != 1 {
		t.Fatal("one summary expected", count)
	}
	query := url.Values{"moduleId": {moduleID(module)}, "lineId": {"history-card"}, "numbers": {`["+12025550123"]`}}
	path := func(direction, anchor string) string {
		q := query.Clone()
		q.Set("direction", direction)
		q.Set("anchor", anchor)
		return "/api/messages/window?" + q.Encode()
	}
	page := read(path("", ""), 200)
	if len(page.Items) != 80 || page.Items[0].ID != "history-0921" || !page.Earlier || page.Newer {
		t.Fatal("invalid latest page", page)
	}
	seen := map[string]bool{}
	for {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate page boundary", item.ID)
			}
			seen[item.ID] = true
		}
		if !page.Earlier {
			break
		}
		page = read(path("earlier", page.Items[0].ID), 200)
	}
	if len(seen) != 1000 {
		t.Fatal("history skipped", len(seen))
	}
	page = read(path("newer", page.Items[len(page.Items)-1].ID), 200)
	if !page.Newer || !page.Earlier || len(page.Items) != 80 {
		t.Fatal("forward pagination", page)
	}
	page = read(path("current", page.Items[len(page.Items)-1].ID), 200)
	if len(page.Items) != 80 {
		t.Fatal("bounded live refresh", page)
	}
	read(path("earlier", "missing-message"), 410)
	bad := query.Clone()
	bad.Set("lineId", "another-card")
	bad.Set("direction", "earlier")
	bad.Set("anchor", "history-1000")
	read("/api/messages/window?"+bad.Encode(), 410)
	if _, err := s.db.Exec(ctx, `UPDATE messages SET deleted_at=now() WHERE id='history-1000'`); err != nil {
		t.Fatal(err)
	}
	delta := read(fmt.Sprintf("/api/messages/threads?after=%d", summary.Cursor), 200)
	if len(delta.Items) != 1 || delta.Items[0].ID != "history-0999" || delta.Items[0].Deleted {
		t.Fatal("deleting last message lost earlier preview", delta)
	}
	// Revisions on old messages wake the selected window without replacing its preview.
	if _, err := s.db.Exec(ctx, `UPDATE messages SET state='delivered' WHERE id='history-0001'`); err != nil {
		t.Fatal(err)
	}
	delta = read(fmt.Sprintf("/api/messages/threads?after=%d", delta.Cursor), 200)
	if len(delta.Items) != 1 || delta.Items[0].ID != "history-0999" {
		t.Fatal("old state update replaced preview", delta)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state) VALUES('history-report',$1,'8986000000000090666','history-card','+12025550123',false,'mms','mms_report')`, module); err != nil {
		t.Fatal(err)
	}
	reports := read(fmt.Sprintf("/api/messages/threads?after=%d", delta.Cursor), 200)
	if len(reports.Items) != 0 {
		t.Fatal("report appeared as conversation")
	}
	var snapshot int64
	for round := 0; round < 8; round++ {
		body, _ := json.Marshal(map[string]any{"moduleId": moduleID(module), "lineId": "history-card", "numbers": []string{"+12025550123"}, "snapshot": snapshot})
		out := httptest.NewRecorder()
		s.messagesAPI(ctx, out, httptest.NewRequest("DELETE", "/api/messages/threads", bytes.NewReader(body)))
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
		var result struct{ Data response }
		if json.Unmarshal(out.Body.Bytes(), &result) != nil {
			t.Fatal(out.Body.String())
		}
		snapshot = result.Data.Snapshot
		if round == 0 {
			if _, err := s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state) VALUES('history-new',$1,'8986000000000090666','history-card','+12025550123',false,'sms','received')`, module); err != nil {
				t.Fatal(err)
			}
		}
		if !result.Data.More {
			break
		}
		if round == 7 {
			t.Fatal("deletion did not finish")
		}
	}
	page = read(path("", ""), 200)
	if len(page.Items) != 1 || page.Items[0].ID != "history-new" {
		t.Fatal("deletion missed unloaded messages or deleted new arrival", page)
	}
	if _, err := s.db.Exec(ctx, messageThreadsSchema); err != nil {
		t.Fatal("migration not repeatable", err)
	}
	summary = read("/api/messages/threads", 200)
	count = 0
	for _, item := range summary.Items {
		if item.LineID == "history-card" {
			count++
			if item.ID != "history-new" {
				t.Fatal("migration reset summary", item)
			}
		}
	}
	if count != 1 {
		t.Fatal("migration duplicated summary")
	}
	before := summary.Cursor
	if _, err := s.db.Exec(ctx, `UPDATE messages SET peer='+12025550456' WHERE id='history-new'`); err != nil {
		t.Fatal(err)
	}
	delta = read(fmt.Sprintf("/api/messages/threads?after=%d", before), 200)
	if len(delta.Items) != 2 || delta.Items[0].Revision == delta.Items[1].Revision {
		t.Fatal("MMS sender change needs two distinct cursor revisions", delta)
	}
	var oldGone, newPresent bool
	for _, item := range delta.Items {
		oldGone = oldGone || item.Number == "+12025550123" && item.Deleted
		newPresent = newPresent || item.Number == "+12025550456" && !item.Deleted
	}
	if !oldGone || !newPresent {
		t.Fatal("MMS sender change left a stale conversation", delta)
	}
	if _, err := s.db.Exec(ctx, `UPDATE messages SET state='mms_report' WHERE id='history-new'`); err != nil {
		t.Fatal(err)
	}
	delta = read(fmt.Sprintf("/api/messages/threads?after=%d", delta.Cursor), 200)
	if len(delta.Items) != 1 || !delta.Items[0].Deleted {
		t.Fatal("decoded report left a visible message", delta)
	}
	if next := read(fmt.Sprintf("/api/messages/threads?after=%d", delta.Cursor), 200); len(next.Items) != 0 || next.Cursor != delta.Cursor {
		t.Fatal("conversation watermark not stable", next)
	}
	// Recreate a pre-upgrade schema only inside this isolated test transaction.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `ALTER TABLE messages DISABLE TRIGGER message_thread_trigger; DROP TABLE message_threads`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,created_at,deleted_at)
 SELECT 'backfill-'||n,$1,'8986000000000090666','history-backfill','128',false,'sms','received',TIMESTAMPTZ '2026-02-01'+n*interval '1 second',CASE WHEN n=3 THEN now() END FROM generate_series(1,3) n`, module); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, messageThreadsSchema); err != nil {
		t.Fatal("legacy backfill failed", err)
	}
	var last string
	var revision, want int64
	if err = tx.QueryRow(ctx, `SELECT last_id,revision FROM message_threads WHERE line_id='history-backfill'`).Scan(&last, &revision); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT max(revision) FROM messages WHERE line_id='history-backfill'`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if last != "backfill-2" || revision != want {
		t.Fatal("backfill lost deletion revision or latest visible preview", last, revision, want)
	}
}
