package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type rejectedCellularCall struct {
	moduleVoice
	reason string
}

func testCallRecordRecoveryDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	for _, tc := range []struct {
		id, state, outcome string
		ended              bool
	}{
		{"recovery-active", "connected", "", false},
		{"recovery-cleanup", "cleanup_pending", "module_error", true},
		{"recovery-ended", "ended", "completed", true},
	} {
		var end *time.Time
		if tc.ended {
			end = &at
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO sip_call_records(id,account_id,module_id,peer,state,outcome,ended_at) VALUES($1,'recovery-test','module-01','fixture',$2,$3,$4)`, tc.id, tc.state, tc.outcome, end); err != nil {
			t.Fatal(err)
		}
	}
	defer s.db.Exec(ctx, `DELETE FROM sip_call_records WHERE account_id='recovery-test'`)
	for i := 0; i < 2; i++ {
		if err := s.recoverCallRecords(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id, state, outcome string
		ended              bool
	}{
		{"recovery-active", "interrupted", "interrupted", false},
		{"recovery-cleanup", "interrupted", "module_error", true},
		{"recovery-ended", "ended", "completed", true},
	} {
		var state, outcome string
		var end *time.Time
		if err := s.db.QueryRow(ctx, `SELECT state,outcome,ended_at FROM sip_call_records WHERE id=$1`, tc.id).Scan(&state, &outcome, &end); err != nil {
			t.Fatal(err)
		}
		if state != tc.state || outcome != tc.outcome || (end != nil) != tc.ended || end != nil && !end.Equal(at) {
			t.Fatal("restart changed call evidence or left stale cleanup", tc.id, state, outcome, end)
		}
	}
}

func (v rejectedCellularCall) FailureReason() string { return v.reason }

func TestDialCallResultKeepsConfirmedFailure(t *testing.T) {
	for _, reason := range []string{"dial_failed", "carrier_rejected", "not_registered", "busy", "rejected", "no_answer", "remote_cancelled"} {
		if got := dialCallResult(rejectedCellularCall{reason: reason}, errors.New("CALL_ENDED")); got != reason {
			t.Fatal(got, reason)
		}
	}
	if got := dialCallResult(rejectedCellularCall{}, errors.New("READ_TIMEOUT")); got != "failed" {
		t.Fatal("unknown outcome treated as confirmed setup failure", got)
	}
}

func TestCallRecordReasons(t *testing.T) {
	for code, want := range map[int]string{606: "call_not_accepted", 486: "busy", 600: "busy", 603: "rejected", 403: "failed", 404: "peer_unavailable", 480: "peer_unavailable", 488: "unsupported_audio", 503: "service_unavailable", 408: "call_timeout", 487: "failed"} {
		if got := incomingClientResult(code); got != want {
			t.Fatal("incoming APP", code, got, want)
		}
	}
	for reason, want := range map[string]string{"module-ended": "remote_cancelled", "timeout": "no_answer", "answered-elsewhere": "answered_elsewhere", "client-ended": "cancelled", "account-deleted": "account_revoked", "module-unavailable": "module_error", "unknown": "failed"} {
		if got := incomingActionResult(reason); got != want {
			t.Fatal("incoming action", reason, got, want)
		}
	}
	for code, want := range map[int]string{606: "call_not_accepted", 486: "busy", 600: "busy", 603: "rejected", 403: "carrier_rejected", 480: "peer_unavailable", 488: "unsupported_audio", 503: "carrier_unavailable", 0: "failed", 200: "failed", 487: "failed"} {
		if got := carrierCallResult(code); got != want {
			t.Fatal(code, got, want)
		}
	}
	for code, want := range map[string]string{"NO_SIM": "card_error", "VOICE_REMOTE_BUSY": "busy", "VOICE_BUSY": "module_busy", "VOICE_NO_ANSWER": "no_answer", "CALL_ENDED": "failed", "DEVICE_CHANGED": "module_error", "VOICE_RADIO_UNAVAILABLE": "radio_unavailable"} {
		if got := moduleCallResult(errors.New(code)); got != want {
			t.Fatal(code, got, want)
		}
	}
	now := time.Now()
	for _, tc := range []struct {
		v    callRecord
		want string
	}{
		{callRecord{State: "ended"}, "unconnected"},
		{callRecord{State: "ended", Status: "busy"}, "busy"},
		{callRecord{State: "connected", Answered: &now}, "active"},
		{callRecord{State: "ended", Status: "failed", Answered: &now, Ended: &now}, "connected"},
		{callRecord{State: "interrupted"}, "interrupted"},
	} {
		if got := callRecordStatus(tc.v); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}

func TestCallRecordFilter(t *testing.T) {
	q := url.Values{"accountId": {strings.Repeat("a", 43)}, "from": {"2026-09-26T16:00:00Z"}, "to": {"2026-09-27T16:00:00Z"}}
	if _, e := parseCallRecordFilter(q); e != nil {
		t.Fatal(e)
	}
	for k, v := range map[string]string{"accountId": "../other", "from": "bad", "to": "2026-09-29T16:00:00Z", "before": "bad"} {
		c := q.Get(k)
		q.Set(k, v)
		if _, e := parseCallRecordFilter(q); e == nil {
			t.Fatal(k)
		}
		q.Set(k, c)
	}
}

func testCallRecordsDatabase(t *testing.T, s *server, cookie string) {
	t.Helper()
	ctx := context.Background()
	account := token()
	other := token()
	a, b := sipDigests("history-test", "test-only")
	if _, e := s.db.Exec(ctx, `INSERT INTO sip_accounts(id,username,port,digest_md5,digest_sha256,allocation) VALUES($1,'history-test',32001,$2,$3,'all')`, account, a, b); e != nil {
		t.Fatal(e)
	}
	defer s.db.Exec(ctx, `DELETE FROM sip_accounts WHERE id=$1`, account)
	defer s.db.Exec(ctx, `DELETE FROM sip_call_records WHERE account_id IN ($1,$2)`, account, other)
	testIncomingSharedRecordDatabase(t, s, account)
	start := time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC)
	for i := 0; i < 103; i++ {
		id := fmt.Sprintf("%032x", i)
		owner := account
		at := start.Add(time.Minute)
		direction := "outgoing"
		if i == 100 {
			direction = "incoming"
		}
		if i == 101 {
			owner = other
		}
		if i == 102 {
			at = start.Add(-time.Microsecond)
		}
		if _, e := s.db.Exec(ctx, `INSERT INTO sip_call_records(id,account_id,module_id,peer,direction,state,outcome,started_at,answered_at,ended_at) VALUES($1,$2,'module-02','10001',$3,'ended','', $4,$4,$4::timestamptz+interval '61 seconds')`, id, owner, direction, at); e != nil {
			t.Fatal(e)
		}
	}
	base := "/api/call-records?" + url.Values{"accountId": {account}, "from": {start.Format(time.RFC3339Nano)}, "to": {start.Add(24 * time.Hour).Format(time.RFC3339Nano)}}.Encode()
	request := func(path string, auth bool, want int) map[string]any {
		t.Helper()
		r := localRequest("GET", path, nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%d %s want %d", w.Code, w.Body.String(), want)
		}
		var envelope struct{ Data map[string]any }
		if want == 200 {
			if e := json.Unmarshal(w.Body.Bytes(), &envelope); e != nil {
				t.Fatal(e)
			}
		}
		return envelope.Data
	}
	request(base, false, 401)
	data := request(base, true, 200)
	if len(data["items"].([]any)) != 100 || data["totalMinutes"].(float64) != 202 {
		t.Fatal(data)
	}
	seen := map[string]bool{}
	for _, x := range data["items"].([]any) {
		v := x.(map[string]any)
		seen[v["id"].(string)] = true
		if v["status"] != "connected" || v["duration"].(float64) != 61 || v["sipAccountId"] != account {
			t.Fatal(v)
		}
	}
	cursor := data["nextCursor"].(string)
	if cursor == "" {
		t.Fatal("missing cursor")
	}
	next := request(base+"&before="+url.QueryEscape(cursor), true, 200)
	if len(next["items"].([]any)) != 1 || next["nextCursor"] != "" {
		t.Fatal(next)
	}
	if seen[next["items"].([]any)[0].(map[string]any)["id"].(string)] {
		t.Fatal("duplicate row")
	}
	stats := request(strings.Replace(base, "/call-records?", "/call-records/stats?", 1), true, 200)
	for _, values := range []map[string]any{data, next, stats} {
		if values["outgoingMinutes"] != float64(200) || values["incomingMinutes"] != float64(2) || values["totalMinutes"] != float64(202) {
			t.Fatal(values)
		}
	}
	request(strings.Replace(base, account, token(), 1), true, 404)
	request(base+"&before=bogus", true, 400)
	// End timestamp freezes before cleanup and first observed cause survives retries.
	id := fmt.Sprintf("%032x", 0)
	end := start.Add(2 * time.Minute)
	s.endCallRecord(id, "module-02", "cleanup_pending", "busy", end)
	s.endCallRecord(id, "module-02", "ended", "failed", end.Add(time.Hour))
	var result string
	var ended time.Time
	if e := s.db.QueryRow(ctx, `SELECT outcome,ended_at FROM sip_call_records WHERE id=$1`, id).Scan(&result, &ended); e != nil || result != "busy" || !ended.Equal(start.Add(time.Minute+61*time.Second)) {
		t.Fatal(result, ended, e)
	}
	// Incoming rows retain their direction, winner's answer time and losing result.
	winner, e := s.startIncomingRecord(ctx, account, "module-03", "+12025550123")
	if e != nil {
		t.Fatal(e)
	}
	loser, e := s.startIncomingRecord(ctx, other, "module-03", "+12025550123")
	if e != nil {
		t.Fatal(e)
	}
	g := newSIPGateway(s)
	l := &sipIncomingLeg{record: winner, owner: &sipIncoming{owner: g.calls}}
	l.recordConnected()
	s.endCallRecord(winner, "module-03", "ended", "cancelled", time.Now())
	s.endCallRecord(loser, "module-03", "ended", "answered_elsewhere", time.Now())
	var direction string
	var answered *time.Time
	if e = s.db.QueryRow(ctx, `SELECT direction,answered_at,outcome FROM sip_call_records WHERE id=$1`, winner).Scan(&direction, &answered, &result); e != nil || direction != "incoming" || answered == nil {
		t.Fatal(direction, answered, e)
	}
	if e = s.db.QueryRow(ctx, `SELECT answered_at,outcome FROM sip_call_records WHERE id=$1`, loser).Scan(&answered, &result); e != nil || answered != nil || result != "answered_elsewhere" {
		t.Fatal(answered, result, e)
	}
	// Isolated day: completed own calls only, per-call rounding, half-open date bounds.
	day := start.Add(-72 * time.Hour)
	for i, tc := range []struct {
		direction, outcome string
		answered, ended    bool
		duration           float64
		offset             time.Duration
	}{
		{"outgoing", "", true, true, 60.1, 0},
		{"outgoing", "", true, true, 60, time.Second},
		{"incoming", "", true, true, 61, time.Second},
		{"incoming", "", true, true, 0, time.Second},
		{"incoming", "answered_elsewhere", false, true, 600, time.Second},
		{"incoming", "no_answer", false, true, 600, time.Second},
		{"incoming", "rejected", false, true, 600, time.Second},
		{"outgoing", "failed", false, true, 600, time.Second},
		{"outgoing", "", true, false, 600, time.Second},
		{"incoming", "interrupted", true, false, 600, time.Second},
		{"outgoing", "", true, true, 600, 24 * time.Hour},
		{"incoming", "", true, true, 600, -time.Microsecond},
	} {
		at := day.Add(tc.offset)
		var answer, end *time.Time
		if tc.answered {
			answer = &at
		}
		if tc.ended {
			v := at.Add(time.Duration(tc.duration * float64(time.Second)))
			end = &v
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO sip_call_records(id,account_id,module_id,peer,direction,state,outcome,started_at,answered_at,ended_at) VALUES($1,$2,'module-02','10001',$3,'ended',$4,$5,$6,$7)`, fmt.Sprintf("%032x", 200+i), account, tc.direction, tc.outcome, at, answer, end); err != nil {
			t.Fatal(err)
		}
	}
	q := url.Values{"accountId": {account}, "from": {day.Format(time.RFC3339Nano)}, "to": {day.Add(24 * time.Hour).Format(time.RFC3339Nano)}}
	for _, path := range []string{"/api/call-records?", "/api/call-records/stats?"} {
		values := request(path+q.Encode(), true, 200)
		if values["outgoingMinutes"] != float64(3) || values["incomingMinutes"] != float64(3) || values["totalMinutes"] != float64(6) {
			t.Fatal(values)
		}
		q.Set("from", day.Add(-24*time.Hour).Format(time.RFC3339Nano))
		q.Set("to", day.Add(-time.Second).Format(time.RFC3339Nano))
		empty := request(path+q.Encode(), true, 200)
		if empty["outgoingMinutes"] != float64(0) || empty["incomingMinutes"] != float64(0) {
			t.Fatal(empty)
		}
		q.Set("from", day.Format(time.RFC3339Nano))
		q.Set("to", day.Add(24*time.Hour).Format(time.RFC3339Nano))
	}
}
