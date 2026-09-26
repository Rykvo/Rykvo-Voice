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
	add("report-national", "+12025550207", "accepted", one(7))
	report("2025550207", 7, 0)
	add("report-multipart", "+12025550208", "accepted", `{"partsTotal":2,"partResults":[{"reference":8,"accepted":true},{"reference":9,"accepted":true}]}`)
	report("+12025550208", 8, 0)
	m.applySMSReports(ctx)
	for id, want := range map[string]string{"report-null": "unknown", "report-plus": "delivered", "report-failed": "failed", "report-temporary": "accepted", "report-forwarded": "accepted", "report-uncertain": "delivered", "report-ambiguous": "accepted", "report-national": "accepted", "report-multipart": "accepted"} {
		check(id, want)
	}
	report("+12025550208", 9, 0)
	m.applySMSReports(ctx)
	check("report-multipart", "delivered")
	// Remove only test rows before the rest of this transaction's assertions.
	exec("DELETE FROM message_reports WHERE iccid=$1", card)
	exec("DELETE FROM messages WHERE iccid=$1", card)
}
