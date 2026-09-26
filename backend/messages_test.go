package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/vocat/device"
	"rykvo.local/auth/internal/vocat/vowifi"
	"strings"
	"testing"
	"time"
)

type fixtureSMS struct {
	calls   int
	unknown bool
}

func (f *fixtureSMS) WiFi(context.Context, hardware.Candidate, string, string, func(string)) error {
	return nil
}
func (f *fixtureSMS) SendSMS(ctx context.Context, c hardware.Candidate, card, id, to, text string) (vowifi.SMSSubmitResult, error) {
	f.calls++
	if f.unknown {
		return vowifi.SMSSubmitResult{}, errors.New("SMS_OUTCOME_UNKNOWN")
	}
	return vowifi.SMSSubmitResult{PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true, PartResults: []vowifi.SMSSubmitPart{{Reference: 7, Accepted: true}}}, nil
}
func testMessagesDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	old := s.modules
	defer func() { s.modules = old }()
	f := &fixtureSMS{}
	m := newModuleManagerWithWiFi(s.db, nil, f)
	m.ready = true
	m.lastScan = time.Now()
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	m.values[v.ID] = sample
	m.seen[v.Endpoint] = sample.Candidate
	m.wifi[v.ID] = &moduleWiFi{Enabled: true, Registered: true, SMSReady: true, running: true, ICCID: sample.Reading.ICCID}
	s.modules = m
	call := func(method, path, body, header string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := localRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("X-CSRF-Token", header)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	id := "12345678-1234-4234-8234-123456785001"
	payload := map[string]string{"requestId": id, "moduleId": moduleID(v.ID), "lineId": wifiLine(sample.Reading), "to": "+12025550123", "text": "fixture only"}
	b, _ := json.Marshal(payload)
	call("POST", "/api/messages", string(b), "", 403)
	call("POST", "/api/messages", string(b), csrf, 202)
	call("POST", "/api/messages", string(b), csrf, 200)
	payload["text"] = "different"
	changed, _ := json.Marshal(payload)
	call("POST", "/api/messages", string(changed), csrf, 409)
	m.processMessage(ctx)
	m.processMessage(ctx)
	if f.calls != 1 {
		t.Fatal("duplicate send", f.calls)
	}
	w := call("GET", "/api/messages/"+id, "", "", 200)
	if !strings.Contains(w.Body.String(), `"state":"accepted"`) || strings.Contains(w.Body.String(), `"delivered"`) {
		t.Fatal("acceptance confused with delivery")
	}
	report := hardware.SMSDelivery{ID: "report", From: payload["to"], TPDU: "020001", Status: &hardware.SMSReport{Reference: 7, Code: 0}}
	if e := m.storeIncoming(ctx, v.ID, wifiLine(sample.Reading), sample.Reading.ICCID, report); e != nil {
		t.Fatal(e)
	}
	m.applySMSReports(ctx)
	w = call("GET", "/api/messages/"+id, "", "", 200)
	if !strings.Contains(w.Body.String(), `"state":"delivered"`) {
		t.Fatal("receipt not applied", w.Body.String())
	}
	payload["requestId"] = "12345678-1234-4234-8234-123456785002"
	b, _ = json.Marshal(payload)
	call("POST", "/api/messages", string(b), csrf, 202)
	f.unknown = true
	m.processMessage(ctx)
	m.processMessage(ctx)
	if f.calls != 2 {
		t.Fatal("unknown send retried")
	}
	testSMSReportMatching(t, ctx, m, v.ID)
	testMMSDownloadResume(t, ctx, m, v.ID)
	testMMSReportMatching(t, ctx, m, v.ID)
	card := sample.Reading.ICCID
	line := wifiLine(sample.Reading)
	part := hardware.SMSDelivery{ID: "part2", From: "12345", Text: "world", TPDU: "0005912143F5000842101021436500020042", Encoding: "ucs2_pdu", Concat: &device.SMSConcatInfo{Reference: 12, Total: 2, Sequence: 2}}
	if e := m.storeIncoming(ctx, v.ID, line, card, part); e != nil {
		t.Fatal(e)
	}
	if e := m.storeIncoming(ctx, v.ID, line, card, part); e != nil {
		t.Fatal(e)
	}
	part.ID = "part1"
	part.Text = "hello "
	part.TPDU = "0005912143F5000842101021436500020041"
	part.Concat.Sequence = 1
	if e := m.storeIncoming(ctx, v.ID, line, card, part); e != nil {
		t.Fatal(e)
	}
	var text, state string
	if e := s.db.QueryRow(ctx, "SELECT body,state FROM messages WHERE peer='12345' ORDER BY created_at LIMIT 1").Scan(&text, &state); e != nil || text != "hello world" || state != "received" {
		t.Fatal(text, state, e)
	}
	w = call("GET", "/api/messages?after=0", "", "", 200)
	if strings.Contains(w.Body.String(), card) || strings.Contains(w.Body.String(), "raw_tpdu") {
		t.Fatal("internal identifiers exposed")
	}
	var page struct {
		Data struct {
			Cursor int64 `json:"cursor"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Data.Cursor < 1 {
		t.Fatal("revision missing")
	}

	contact := map[string]string{"lineId": line, "number": "+12025550123", "name": "测试备注"}
	note, _ := json.Marshal(contact)
	call("PUT", "/api/messages/contacts", string(note), "", 403)
	call("PUT", "/api/messages/contacts", string(note), csrf, 200)
	w = call("GET", "/api/messages?after=0", "", "", 200)
	if !strings.Contains(w.Body.String(), `"name":"测试备注"`) {
		t.Fatal("contact missing", w.Body.String())
	}
	contact["name"] = strings.Repeat("字", 25)
	note, _ = json.Marshal(contact)
	call("PUT", "/api/messages/contacts", string(note), csrf, 400)
	contact["name"] = ""
	note, _ = json.Marshal(contact)
	call("PUT", "/api/messages/contacts", string(note), csrf, 200)
	var name string
	if e := s.db.QueryRow(ctx, "SELECT name FROM message_contacts WHERE line_id=$1 AND peer=$2", line, contact["number"]).Scan(&name); e != nil || name != "" {
		t.Fatal("clear note", name, e)
	}
	contact["lineId"] = "not-a-real-line"
	note, _ = json.Marshal(contact)
	call("PUT", "/api/messages/contacts", string(note), csrf, 404)
	call("DELETE", "/api/messages/"+id, "", csrf, 204)
	call("GET", "/api/messages/"+id+"/image", "", "", 404)
}
