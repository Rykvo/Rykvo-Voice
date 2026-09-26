package main

import (
	"context"
	"fmt"
	"testing"
)

func testSMSReportMatching(t *testing.T, ctx context.Context, m *moduleManager, module int64) {
	t.Helper()
	const card = "89000000000000000123"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := m.db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	add := func(id, peer, state, result string) {
		exec("INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,result) VALUES($1,$2,$3,'report-fixture',$4,true,'sms',$5,$6::jsonb)", id, module, card, peer, state, result)
	}
	report := func(peer string, ref, status int) {
		exec("INSERT INTO message_reports(iccid,fingerprint,peer,reference,status) VALUES($1,$2,$3,$4,$5)", card, fmt.Sprintf("%s-%d-%d", peer, ref, status), peer, ref, status)
	}
	one := func(ref int) string {
		return fmt.Sprintf(`{"partsTotal":1,"partResults":[{"reference":%d,"accepted":true}]}`, ref)
	}
	check := func(id, want string) {
		t.Helper()
		var state string
		if e := m.db.QueryRow(ctx, "SELECT state FROM messages WHERE id=$1", id).Scan(&state); e != nil || state != want {
			t.Fatalf("%s: %s != %s (%v)", id, state, want, e)
		}
	}
	// A null result from an interrupted worker must not break all receipts.
	add("report-null", "+12025550200", "unknown", `{"partResults":null}`)
	add("report-plus", "+12025550201", "accepted", one(1))
	report("12025550201", 1, 0)
	add("report-failed", "+12025550202", "accepted", one(2))
	report("+12025550202", 2, 69)
	add("report-temporary", "+12025550203", "accepted", one(3))
	report("+12025550203", 3, 32)
	add("report-forwarded", "+12025550204", "accepted", one(4))
	report("+12025550204", 4, 1)
	add("report-uncertain", "+12025550205", "unknown", one(5))
	report("+12025550205", 5, 0)
	add("report-old", "+12025550206", "delivered", one(6))
	add("report-ambiguous", "+12025550206", "accepted", one(6))
	report("+12025550206", 6, 0)
	exec("UPDATE message_reports SET scts=now() WHERE iccid=$1 AND reference=6", card)
	add("report-national", "+12025550207", "accepted", one(7))
	report("2025550207", 7, 0)
	add("report-multipart", "+12025550208", "accepted", `{"partsTotal":2,"partResults":[{"reference":8,"accepted":true},{"reference":9,"accepted":true}]}`)
	report("+12025550208", 8, 0)
	// A later reuse must not match a receipt to every older identical TP-MR.
	add("report-reused-old", "+12025550209", "accepted", one(10))
	add("report-reused-new", "+12025550209", "accepted", one(10))
	exec("UPDATE messages SET created_at=now()-interval '1 hour' WHERE id='report-reused-old'")
	report("+12025550209", 10, 0)
	exec("UPDATE message_reports SET scts=now() WHERE iccid=$1 AND reference=10", card)
	// Use actual part submission time, not the time it entered the queue.
	add("report-queued", "+12025550210", "accepted", one(11))
	exec(`UPDATE messages SET created_at=now()-interval '1 hour',
 result=jsonb_set(result,'{partResults,0,submittedAt}',to_jsonb(now())) WHERE id='report-queued'`)
	report("+12025550210", 11, 0)
	exec("UPDATE message_reports SET scts=now() WHERE iccid=$1 AND reference=11", card)
	// An old SC timestamp must not confirm a newer message either.
	add("report-stale-time", "+12025550211", "accepted", one(12))
	report("+12025550211", 12, 0)
	exec("UPDATE message_reports SET scts=now()-interval '1 hour' WHERE iccid=$1 AND reference=12", card)
	m.applySMSReports(ctx)
	for id, want := range map[string]string{"report-null": "unknown", "report-plus": "delivered", "report-failed": "failed", "report-temporary": "accepted", "report-forwarded": "accepted", "report-uncertain": "delivered", "report-ambiguous": "accepted", "report-national": "accepted", "report-multipart": "accepted"} {
		check(id, want)
	}
	check("report-reused-old", "accepted")
	check("report-reused-new", "delivered")
	check("report-queued", "delivered")
	check("report-stale-time", "accepted")
	report("+12025550208", 9, 0)
	m.applySMSReports(ctx)
	check("report-multipart", "delivered")
	add("report-wait-timeout", "+12025550212", "accepted", one(13))
	// Only the isolated fixture may bypass the revision trigger to age a row.
	exec("ALTER TABLE messages DISABLE TRIGGER message_revision_trigger")
	exec("UPDATE messages SET updated_at=now()-interval '3 minutes' WHERE id='report-wait-timeout'")
	exec("ALTER TABLE messages ENABLE TRIGGER message_revision_trigger")
	m.applySMSReports(ctx)
	check("report-wait-timeout", "unknown")
	report("+12025550212", 13, 0)
	m.applySMSReports(ctx)
	check("report-wait-timeout", "delivered")
	// Remove only test rows before the rest of this transaction's assertions.
	exec("DELETE FROM message_reports WHERE iccid=$1", card)
	exec("DELETE FROM messages WHERE iccid=$1", card)
}
