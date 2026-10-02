package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/vocat/device"
)

func testIncomingWebhook(t *testing.T, s *server) {
	ctx := context.Background()
	sample := wifiModuleFixture()
	module, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading)
	if err != nil {
		t.Fatal(err)
	}
	m := newModuleManager(s.db, nil)
	store := func(v hardware.SMSDelivery) {
		t.Helper()
		if err := m.storeIncoming(ctx, module.ID, wifiLine(sample.Reading), sample.Reading.ICCID, v); err != nil {
			t.Fatal(err)
		}
	}
	check := func(peer, want string, count int) {
		t.Helper()
		rows, err := s.db.Query(ctx, `SELECT id,data->'data'->>'text' FROM developer_events
		 WHERE event_type='message.received' AND data->'data'->>'number'=$1 ORDER BY id`, peer)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				t.Fatal(err)
			}
			if text != want {
				t.Errorf("premature received event: text=%q, want %q", text, want)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if rows.Err() != nil || len(ids) != count {
			t.Fatalf("received events=%d, want %d: %v", len(ids), count, rows.Err())
		}
		client := &http.Client{Transport: webhookRoundTrip(func(r *http.Request) (*http.Response, error) {
			var event struct {
				Type string `json:"type"`
				Data struct {
					Text string `json:"text"`
				} `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&event); err != nil || event.Type != "message.received" || event.Data.Text != want {
				t.Errorf("incorrect callback payload: %+v %v", event, err)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		for _, id := range ids {
			s.deliverWebhook(ctx, id, client, &webhookBackoff{})
		}
	}
	for _, tc := range []struct{ peer, text, tpdu string }{
		{"10001", "complete text", "0005912143F5000842101021436500020041"},
		{"10002", "", "0005912143F5000842101021436500020042"},
	} {
		v := hardware.SMSDelivery{From: tc.peer, Text: tc.text, TPDU: tc.tpdu}
		store(v)
		store(v) // Re-delivery must not create another callback.
		check(tc.peer, tc.text, 1)
	}
	part := hardware.SMSDelivery{From: "10003", Text: "world", TPDU: "0005912143F5000842101021436500020043", Encoding: "ucs2_pdu", Concat: &device.SMSConcatInfo{Reference: 121, Total: 2, Sequence: 2}}
	store(part)
	check(part.From, "", 0)
	part.Text, part.TPDU, part.Concat.Sequence = "hello ", "0005912143F5000842101021436500020044", 1
	store(part)
	store(part)
	check(part.From, "hello world", 1)
	single := hardware.SMSDelivery{From: "10004", Text: "single UDH", TPDU: "4405912143F5000842102030405000080500037A01010041", Concat: &device.SMSConcatInfo{Reference: 122, Total: 1, Sequence: 1}}
	store(single)
	store(single)
	check(single.From, single.Text, 1)
	// A binary WAP notification is not an empty received SMS.
	ud, _ := hex.DecodeString("0605040b8423f0010601be8c828d9298740083687474703a2f2f6d70632e742d6d6f62696c652e636f6d2f7465737400")
	raw, _ := hex.DecodeString("4005912143F5000442101021436500")
	raw = append(raw, byte(len(ud)))
	raw = append(raw, ud...)
	decoded, err := device.DecodeSMSDeliverTPDU(raw)
	if err != nil || decoded.Encoding != device.SMSEncoding8BitPDU {
		t.Fatal("invalid WAP fixture", err)
	}
	store(hardware.SMSDelivery{From: "10005", TPDU: hex.EncodeToString(raw)})
	check("10005", "", 0)
	var premature int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM developer_events WHERE data->'data'->>'number'='10005' AND data->'data'->>'kind'='sms'`).Scan(&premature); err != nil || premature != 0 {
		t.Fatal("WAP push published as SMS", premature, err)
	}
}
