package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rykvo.local/auth/internal/hardware"
)

func testRetentionDatabase(t *testing.T, s *server, session, csrf string) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err, query)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err, query)
		}
		return n
	}
	request := func(method string, body any, want int) retentionSettings {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := localRequest(method, "/api/settings/retention", bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: cookieName, Value: session})
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("retention %s: %d %s", method, w.Code, w.Body.String())
		}
		var result struct{ Data retentionSettings }
		if want == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result.Data
	}
	if count("SELECT count(*) FROM retention_settings WHERE days=3 AND upload_hours=2 AND revision=1") != 1 {
		t.Fatal("new installations must default to three days")
	}
	for _, days := range []int{0, 30} {
		exec("UPDATE retention_settings SET days=$1,upload_hours=72", days)
		exec(retentionSchema)
		if count("SELECT count(*) FROM retention_settings WHERE days=$1 AND upload_hours=72 AND revision=1", days) != 1 {
			t.Fatal("upgrade overwrote an existing retention setting", days)
		}
	}
	exec("ALTER TABLE retention_settings DROP COLUMN upload_hours")
	exec(retentionSchema)
	if count("SELECT count(*) FROM retention_settings WHERE days=30 AND upload_hours=2 AND revision=1") != 1 {
		t.Fatal("legacy settings migration changed existing history or missed upload default")
	}
	exec("UPDATE retention_settings SET days=3")
	exec("UPDATE developer_settings SET key_hash=decode(repeat('ab',32),'hex')")
	defer exec("UPDATE developer_settings SET key_hash=''::bytea")
	request("GET", nil, 200)
	request("PUT", map[string]any{"days": -1, "revision": 1}, 400)
	request("PUT", map[string]any{"revision": 1}, 400)
	for _, hours := range []any{-1, 0, 1.5, 8761, "2"} {
		request("PUT", map[string]any{"days": 3, "uploadHours": hours, "revision": 1}, 400)
	}
	request("PUT", map[string]any{"days": 0, "revision": 1}, 200)
	request("PUT", map[string]any{"days": 1, "revision": 1}, 409)
	var mid int64
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('retention','fixture','fixture','Retention','retention') RETURNING id`).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	const card = "8986000000000090011"
	exec("UPDATE modules SET active_card=$2,card_epoch=1 WHERE id=$1", mid, card)
	add := func(id, state string) {
		exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,request_hash,created_at)
 VALUES($1,$2,$3,'fixture','+12025550111',true,'sms','keep until expiry',$4,'retention-hash',now()-interval '10 days')`, id, mid, card, state)
	}
	add("retention-finished", "accepted")
	add("retention-pending", "queued")
	add("retention-webhook", "accepted")
	exec("INSERT INTO developer_uploads(id,content,created_at) VALUES('retention-old','fixture',now()-interval '3 hours'),('retention-new','fixture',now()-interval '119 minutes')")
	exec("INSERT INTO sip_call_records(id,account_id,module_id,peer,state,started_at,ended_at) VALUES('retention-ended','fixture','', 'fixture','ended',now()-interval '10 days',now()-interval '10 days'),('retention-active','fixture','','fixture','dialing',now()-interval '10 days',NULL)")
	exec("UPDATE messages SET image='retention-image-copy' WHERE id IN ('retention-finished','retention-pending','retention-webhook')")
	// Completed notification history obeys the same setting, not a second 30-day timer.
	exec(`INSERT INTO alert_notifications(module_id,kind,epoch,cycle,revision,failures,reason,state,created_at)
 SELECT $1,'sms',1,n,1,1,'retention-test',CASE WHEN n=1 THEN 'pending' WHEN n=2 THEN 'sending' ELSE 'sent' END,now()-interval '45 days' FROM generate_series(1,4) n`, mid)
	exec("INSERT INTO alert_counters(module_id,kind,epoch,cycle,active) VALUES($1,'sms',1,4,true) ON CONFLICT(module_id,kind) DO UPDATE SET epoch=1,cycle=4,active=true", mid)
	clean := func() {
		t.Helper()
		if err := s.cleanRecords(ctx); err != nil {
			t.Fatal(err)
		}
	}
	clean()
	if count("SELECT count(*) FROM messages WHERE id='retention-finished' AND deleted_at IS NULL AND body<>''") != 1 || count("SELECT count(*) FROM sip_call_records WHERE id='retention-ended'") != 1 {
		t.Fatal("zero removed business history")
	}
	if count("SELECT count(*) FROM developer_uploads WHERE id='retention-old'") != 0 || count("SELECT count(*) FROM developer_uploads WHERE id='retention-new'") != 1 {
		t.Fatal("temporary upload lifetime")
	}
	if count("SELECT count(*) FROM alert_notifications WHERE reason='retention-test'") != 4 {
		t.Fatal("zero removed notification history")
	}
	// Expiring a staging upload must not remove a message's independent image.
	exec("UPDATE developer_uploads SET created_at=now()-interval '121 minutes' WHERE id='retention-new'")
	clean()
	if count("SELECT count(*) FROM developer_uploads WHERE id='retention-new'") != 0 || count("SELECT count(*) FROM messages WHERE id IN ('retention-finished','retention-pending','retention-webhook') AND image='retention-image-copy'") != 3 {
		t.Fatal("upload expiry affected message attachments")
	}
	// Old callbacks awaiting delivery protect referenced images and records.
	exec("UPDATE developer_events SET state='pending' WHERE resource_id='retention-webhook'")
	request("PUT", map[string]any{"days": 1, "revision": 2}, 200)
	clean()
	if count("SELECT count(*) FROM messages WHERE id='retention-finished' AND deleted_at IS NOT NULL AND body=''") != 1 {
		t.Fatal("finished record not pruned")
	}
	if count("SELECT count(*) FROM messages WHERE id IN ('retention-pending','retention-webhook') AND deleted_at IS NULL AND body<>''") != 2 {
		t.Fatal("active work or callback data pruned")
	}
	if count("SELECT count(*) FROM sip_call_records WHERE id='retention-active'") != 1 || count("SELECT count(*) FROM sip_call_records WHERE id='retention-ended'") != 0 {
		t.Fatal("active call not protected")
	}
	if count("SELECT count(*) FROM messages WHERE id='retention-finished' AND image=''") != 1 || count("SELECT count(*) FROM messages WHERE id IN ('retention-pending','retention-webhook') AND image='retention-image-copy'") != 2 {
		t.Fatal("history expiry lost an active or callback image")
	}
	if count("SELECT count(*) FROM alert_notifications WHERE reason='retention-test' AND cycle=3") != 0 || count("SELECT count(*) FROM alert_notifications WHERE reason='retention-test' AND cycle IN (1,2,4)") != 3 {
		t.Fatal("notification retention lost pending, sending or active alert state")
	}
	exec("DELETE FROM alert_notifications WHERE reason='retention-test'")
	exec("DELETE FROM alert_counters WHERE module_id=$1", mid)
	// Tombstones prevent stale retries from retransmitting a cleared task.
	exec("UPDATE messages SET deleted_at=now()-interval '8 days' WHERE id='retention-finished'")
	clean()
	if count("SELECT count(*) FROM messages WHERE id='retention-finished'") != 0 {
		t.Fatal("expired tombstone retained")
	}
	_, _, err := s.previousMessage(ctx, s.db, "retention-finished", "retention-hash")
	var issue *serviceError
	if !errors.As(err, &issue) || issue.Status != 410 {
		t.Fatal("expired request could resend", err)
	}
	_, _, err = s.previousMessage(ctx, s.db, "retention-finished", "different")
	if !errors.As(err, &issue) || issue.Status != 409 {
		t.Fatal("expired request hash not protected", err)
	}
	// Incoming fingerprints survive physical history deletion.
	incoming := hardware.SMSDelivery{From: "+12025550113", Text: "fixture", TPDU: "0005912143F5000842101021436500020063"}
	m := newModuleManager(s.db, nil)
	if err = m.storeIncoming(ctx, mid, "fixture", card, incoming); err != nil {
		t.Fatal(err)
	}
	rid := "rx-" + messageDigest(card+"\x00"+incoming.TPDU)
	exec("UPDATE messages SET deleted_at=now()-interval '8 days' WHERE id=$1", rid)
	clean()
	if err = m.storeIncoming(ctx, mid, "fixture", card, incoming); err != nil {
		t.Fatal(err)
	}
	if count("SELECT count(*) FROM messages WHERE id=$1", rid) != 0 {
		t.Fatal("deleted receive redelivered")
	}
	// Keep a contiguous catch-up floor and stop at the first unfinished event.
	exec("UPDATE developer_events SET created_at=now()-interval '10 days',state='disabled'")
	var first, second int64
	if err = s.db.QueryRow(ctx, "SELECT min(id) FROM developer_events").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(ctx, "SELECT min(id) FROM developer_events WHERE id>$1", first).Scan(&second); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE developer_events SET state='pending' WHERE id=$1", second)
	if err = s.cleanDeveloperEvents(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if count("SELECT count(*) FROM developer_events WHERE id=$1", first) != 0 || count("SELECT count(*) FROM developer_events WHERE id=$1", second) != 1 {
		t.Fatal("event cleanup crossed active event")
	}
	var floor int64
	s.db.QueryRow(ctx, "SELECT event_floor FROM developer_settings").Scan(&floor)
	if floor != first {
		t.Fatal("noncontiguous cursor floor", floor, first)
	}
	exec("UPDATE developer_events SET state='disabled' WHERE id=$1", second)
	if err = s.cleanDeveloperEvents(ctx, 1); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.developerEventList(ctx, w, localRequest("GET", "/api/v1/events?after=1", nil))
	if w.Code != 410 || !strings.Contains(w.Body.String(), "CURSOR_EXPIRED") {
		t.Fatal("stale event cursor not rejected", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.developerEventList(ctx, w, localRequest("GET", "/api/v1/events?after=0", nil))
	if w.Code != 200 {
		t.Fatal("catch-up restart failed", w.Code)
	}
	request("PUT", map[string]any{"days": 0, "revision": 3}, 200)
	// Large expired backlogs continue in small commits instead of 256 rows/minute.
	exec(`INSERT INTO developer_uploads(id,content,created_at) SELECT 'retention-batch-'||n,'fixture',now()-interval '3 hours' FROM generate_series(1,600) n`)
	exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,request_hash,created_at,deleted_at)
 SELECT 'retention-batch-'||n,$1,$2,'fixture','+12025550111',true,'sms','expired','accepted','batch-hash',now()-interval '10 days',now()-interval '8 days' FROM generate_series(1,600) n`, mid, card)
	before := 600
	for step := 0; step < 8; step++ {
		more, err := s.cleanRecordsBatch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		remaining := count("SELECT count(*) FROM messages WHERE id LIKE 'retention-batch-%'")
		if before-remaining > 256 {
			t.Fatal("unbounded cleanup transaction", before, remaining)
		}
		before = remaining
		if step == 0 && !more {
			t.Fatal("full batch did not request continuation")
		}
		if !more {
			break
		}
	}
	if before != 0 || count("SELECT count(*) FROM developer_uploads WHERE id LIKE 'retention-batch-%'") != 0 {
		t.Fatal("cleanup backlog remained")
	}
	if count("SELECT count(*) FROM message_requests WHERE id LIKE 'retention-batch-%'") != 600 {
		t.Fatal("batched deletion lost request protection")
	}
	settings := request("GET", nil, 200)
	settings = request("PUT", map[string]any{"days": 0, "uploadHours": 4, "revision": settings.Revision}, 200)
	if settings.UploadHours != 4 || settings.Days != 0 {
		t.Fatal(settings)
	}
	exec("INSERT INTO developer_uploads(id,content,created_at) VALUES('retention-custom-keep','fixture',now()-interval '3 hours'),('retention-custom-expire','fixture',now()-interval '5 hours')")
	clean()
	if count("SELECT count(*) FROM developer_uploads WHERE id='retention-custom-keep'") != 1 || count("SELECT count(*) FROM developer_uploads WHERE id='retention-custom-expire'") != 0 {
		t.Fatal("custom upload hours were not used by cleanup")
	}
	settings = request("PUT", map[string]any{"days": 0, "uploadHours": 1, "revision": settings.Revision}, 200)
	clean()
	if count("SELECT count(*) FROM developer_uploads WHERE id='retention-custom-keep'") != 0 {
		t.Fatal("shorter upload retention ignored")
	}
	settings = request("PUT", map[string]any{"days": 0, "revision": settings.Revision}, 200)
	if settings.UploadHours != 1 || request("GET", nil, 200).UploadHours != 1 {
		t.Fatal("older client reset upload retention")
	}
	settings = request("PUT", map[string]any{"days": 0, "uploadHours": 8760, "revision": settings.Revision}, 200)
	if settings.UploadHours != 8760 {
		t.Fatal("maximum upload retention rejected")
	}
	request("PUT", map[string]any{"days": 0, "uploadHours": 2, "revision": settings.Revision}, 200)
}
