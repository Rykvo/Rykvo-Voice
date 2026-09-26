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

func TestCallRecordReasons(t *testing.T) {
	for code, want := range map[int]string{486: "busy", 600: "busy", 603: "rejected", 403: "carrier_rejected", 480: "peer_unavailable", 488: "unsupported_audio", 503: "carrier_unavailable", 0: "failed", 200: "failed", 487: "failed"} {
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
	if stats["totalMinutes"] != data["totalMinutes"] {
		t.Fatal(stats, data)
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
}
