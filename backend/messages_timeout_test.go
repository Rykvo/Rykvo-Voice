package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func testMessageTimeoutDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	sample := wifiModuleFixture()
	sample.Candidate.Key = "result-timeout-fixture"
	sample.Reading.IMEI = "990000008880999"
	sample.Reading.ICCID = "8900000000000880999"
	v, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(ctx, "DELETE FROM modules WHERE id=$1", v.ID)
	defer s.db.Exec(ctx, "DELETE FROM messages WHERE line_id='result-timeout-fixture'")
	m := newModuleManager(s.db, nil)
	now := time.Now().UTC()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	add := func(id, state string, mine bool, age time.Duration) {
		exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,operation_at,expires_at)
 VALUES($1,$2,$3,'result-timeout-fixture','+12025550777',$4,'sms',$5,$6,$7)`,
			id, v.ID, sample.Reading.ICCID, mine, state, now.Add(-age), now.Add(-time.Second))
	}
	for _, state := range []string{"queued", "waiting_network", "sending", "unknown", "accepted", "delivered"} {
		add("timeout-"+state, state, true, messageResultTimeout)
	}
	add("timeout-young", "unknown", true, messageResultTimeout-time.Second)
	add("timeout-inbound", "unknown", false, time.Hour)
	add("timeout-deleted", "unknown", true, time.Hour)
	exec("UPDATE messages SET deleted_at=now() WHERE id='timeout-deleted'")
	if err := m.expireMessageResults(ctx, now); err != nil {
		t.Fatal(err)
	}
	check := func(id, state, issue string) {
		t.Helper()
		var gotState, gotIssue string
		if err := s.db.QueryRow(ctx, "SELECT state,issue FROM messages WHERE id=$1", id).Scan(&gotState, &gotIssue); err != nil || gotState != state || gotIssue != issue {
			t.Fatalf("%s: %s/%s, want %s/%s (%v)", id, gotState, gotIssue, state, issue, err)
		}
	}
	for _, state := range []string{"queued", "waiting_network"} {
		check("timeout-"+state, "failed", "MESSAGE_EXPIRED")
	}
	for _, state := range []string{"sending", "unknown"} {
		check("timeout-"+state, "failed", "RESULT_TIMEOUT")
	}
	for _, state := range []string{"accepted", "delivered"} {
		check("timeout-"+state, state, "")
	}
	for _, id := range []string{"young", "inbound", "deleted"} {
		check("timeout-"+id, "unknown", "")
	}
	// A delayed local durable outcome may confirm success, never re-open sending.
	if err := m.applyMessageOutcome(ctx, []byte(`{"id":"timeout-unknown","state":"unknown","issue":"SMS_OUTCOME_UNKNOWN"}`)); err != nil {
		t.Fatal(err)
	}
	check("timeout-unknown", "failed", "RESULT_TIMEOUT")
	if err := m.applyMessageOutcome(ctx, []byte(`{"id":"timeout-unknown","state":"accepted","issue":""}`)); err != nil {
		t.Fatal(err)
	}
	check("timeout-unknown", "accepted", "")
	if err := m.expireMessageResults(ctx, now); err != nil {
		t.Fatal(err)
	}
	check("timeout-unknown", "accepted", "")
	// Bounded maintenance drains across calls without starting any transport.
	for i := 0; i < 201; i++ {
		add(fmt.Sprintf("timeout-bounded-%03d", i), "unknown", true, time.Hour)
	}
	if err := m.expireMessageResults(ctx, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM messages WHERE id LIKE 'timeout-bounded-%' AND state='failed'").Scan(&count); err != nil || count != 200 {
		t.Fatal(count, err)
	}
	if err := m.expireMessageResults(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM messages WHERE id LIKE 'timeout-bounded-%' AND state='failed'").Scan(&count); err != nil || count != 201 {
		t.Fatal(count, err)
	}
}
