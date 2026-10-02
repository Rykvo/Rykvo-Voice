package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
)

func testMessageSyncDatabase(t *testing.T, s *server) {
	ctx := context.Background()
	var module int64
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('sync-fixture','sync','fixture','Sync','sync') RETURNING id`).Scan(&module); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state) SELECT 'sync-test-'||n,$1,'8986000000000090123','sync-test','+12025550123',true,'sms','fixture','accepted' FROM generate_series(1,240) n`, module); err != nil {
		t.Fatal(err)
	}
	type page struct {
		Items            []messageView
		Cursor, Snapshot int64
		More             bool
	}
	read := func(after, snapshot int64, want int) page {
		t.Helper()
		path := fmt.Sprintf("/api/v1/messages?moduleId=%s&limit=100&after=%d", moduleID(module), after)
		if snapshot > 0 {
			path += fmt.Sprintf("&snapshot=%d", snapshot)
		}
		req := httptest.NewRequest("GET", path, nil)
		out := httptest.NewRecorder()
		s.developerMessageList(ctx, out, req)
		if out.Code != want {
			t.Fatalf("message sync: %d %s", out.Code, out.Body.String())
		}
		var result struct{ Data page }
		if want == 200 && json.Unmarshal(out.Body.Bytes(), &result) != nil {
			t.Fatal(out.Body.String())
		}
		return result.Data
	}
	p := read(0, 0, 200)
	if len(p.Items) != 100 || !p.More || p.Snapshot < p.Cursor {
		t.Fatal("unbounded/invalid first page", p)
	}
	// A change during paging is fetched on the next delta, not silently skipped.
	var changed int64
	if err := s.db.QueryRow(ctx, "UPDATE messages SET body='changed' WHERE id=$1 RETURNING revision", p.Items[0].ID).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	p2 := read(p.Cursor, p.Snapshot, 200)
	p3 := read(p2.Cursor, p.Snapshot, 200)
	if !p2.More || p3.More || p3.Cursor != p.Snapshot || len(p3.Items) != 40 {
		t.Fatal("snapshot paging lost rows", p2, p3)
	}
	delta := read(p3.Cursor, 0, 200)
	if len(delta.Items) != 1 || delta.Cursor != changed {
		t.Fatal("late committed update skipped", delta)
	}
	// Simulate retention's atomic purge watermark; an old delta must reset.
	if _, err := s.db.Exec(ctx, "UPDATE message_sync_state SET floor=$1", changed); err != nil {
		t.Fatal(err)
	}
	read(p.Cursor, 0, 410)
	read(p.Cursor, p.Snapshot, 410)
	fresh := read(0, 0, 200)
	next := read(fresh.Cursor, fresh.Snapshot, 200)
	if len(next.Items) != 100 {
		t.Fatal("fresh bootstrap below purge floor failed")
	}
	read(changed+100000, 0, 400)
}
