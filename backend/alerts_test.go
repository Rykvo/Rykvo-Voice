package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/hardware"
	"strings"
	"sync"
	"testing"
	"time"
)

type alertTransport func(*http.Request) (*http.Response, error)

func (f alertTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAlertProxyAndSecrets(t *testing.T) {
	for _, value := range []string{"", "127.0.0.1:1080::", "[::1]:1080:u:p:a", "192.0.2.1:65535:user:pass@#"} {
		if _, err := parseTelegramProxy(value); err != nil {
			t.Fatal(value, err)
		}
	}
	for _, value := range []string{"socks5://127.0.0.1:1080", "host:1080::", "0.0.0.0:1080::", "224.0.0.1:1080::", "1.2.3.4:0::", "1.2.3.4:1080:u:", "[::1]:1080::p", "1.2.3.4:1080:u:p\n"} {
		if _, err := parseTelegramProxy(value); err == nil {
			t.Fatal("accepted invalid proxy", value)
		}
	}
	for _, tc := range []struct {
		raw  json.RawMessage
		want string
	}{{nil, "old"}, {json.RawMessage("null"), ""}, {json.RawMessage(`"new"`), "new"}} {
		got, e := secretPatch(tc.raw, "old")
		if e != nil || got != tc.want {
			t.Fatal(got, e)
		}
	}
	client, e := telegramClient("127.0.0.1:1080:u:p")
	if e != nil {
		t.Fatal(e)
	}
	u, e := client.Transport.(*http.Transport).Proxy(&http.Request{})
	if e != nil || u.Scheme != "socks5" || u.User.Username() != "u" {
		t.Fatal("proxy not bound")
	}
	t.Setenv("HTTPS_PROXY", "http://must-not-use.invalid")
	client, _ = telegramClient("")
	u, e = client.Transport.(*http.Transport).Proxy(&http.Request{})
	if e != nil || u != nil {
		t.Fatal("environment proxy leaked")
	}
	if strings.Contains(alertReason("sms", "secret-token"), "secret-token") {
		t.Fatal("raw issue exposed")
	}
	for kind, want := range map[string]string{"sip": "SIP呼出失败", "sms": "短信发送异常", "mms": "彩信发送异常"} {
		for _, code := range []string{"dial_failed", "SMS_REJECTED", "SMS_OUTCOME_UNKNOWN", "MMS_REJECTED", "secret-token"} {
			if reason := alertReason(kind, code); reason != want {
				t.Fatal(kind, code, reason)
			}
		}
	}
}

func TestAlertTelegramOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status      int
		body, state string
		err         error
	}{
		{200, `{"ok":true,"result":{"message_id":42}}`, "sent", nil},
		{429, `{"ok":false,"parameters":{"retry_after":12}}`, "pending", nil},
		{401, `{"ok":false,"description":"secret"}`, "failed", nil},
		{403, `{"ok":false}`, "failed", nil}, {503, `{}`, "unknown", nil},
		{200, `invalid`, "unknown", nil}, {0, "", "unknown", errors.New("after write")},
		{0, "", "pending", &net.OpError{Op: "dial", Err: errors.New("proxy secret")}},
	} {
		client := &http.Client{Transport: alertTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.telegram.org" || r.URL.Scheme != "https" || r.Method != "POST" {
				t.Fatal("wrong endpoint")
			}
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["chat_id"] != "-123" || b["text"] != "fixture" || len(b) != 2 {
				t.Fatal("bad request", b)
			}
			if tc.err != nil {
				return nil, tc.err
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
		})}
		result := sendTelegram(context.Background(), client, telegramSettings{Token: "123:fixture", Chat: "-123"}, "fixture")
		if result.State != tc.state || strings.Contains(result.Issue, "secret") {
			t.Fatal(tc, result)
		}
		if tc.status == 429 && result.Retry != 12*time.Second {
			t.Fatal(result)
		}
	}
}

func TestAlertSOCKSRemoteDNSAndNoFallback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			result <- e
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		header := make([]byte, 2)
		if _, e = io.ReadFull(conn, header); e != nil {
			result <- e
			return
		}
		methods := make([]byte, int(header[1]))
		if _, e = io.ReadFull(conn, methods); e != nil {
			result <- e
			return
		}
		conn.Write([]byte{5, 0})
		request := make([]byte, 5)
		if _, e = io.ReadFull(conn, request); e != nil {
			result <- e
			return
		}
		if request[0] != 5 || request[1] != 1 || request[3] != 3 {
			result <- fmt.Errorf("expected SOCKS domain connect: %v", request)
			return
		}
		target := make([]byte, int(request[4])+2)
		if _, e = io.ReadFull(conn, target); e != nil {
			result <- e
			return
		}
		if string(target[:len(target)-2]) != "api.telegram.org" {
			result <- fmt.Errorf("wrong SOCKS destination")
			return
		}
		conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		result <- nil
	}()
	client, e := telegramClient(listener.Addr().String() + "::")
	if e != nil {
		t.Fatal(e)
	}
	defer client.CloseIdleConnections()
	got := sendTelegram(context.Background(), client, telegramSettings{Token: "123:fixture", Chat: "1"}, "fixture")
	if got.State == "sent" || got.State == "unknown" {
		t.Fatal("proxy rejection should be pre-send failure", got)
	}
	if e = <-result; e != nil {
		t.Fatal(e)
	}
}

func TestAlertHealthGraceAndRecovery(t *testing.T) {
	now := time.Now()
	c := hardware.Candidate{Key: "fixture", Kind: "usb"}
	m := newModuleManager(nil, nil)
	m.ready = true
	m.lastScan = now
	m.seen[c.Key] = c
	m.values[1] = moduleSample{c, hardware.Reading{Responsive: true, ICCID: "89123456789012345678", UpdatedAt: now}}
	windows := map[int64]healthWindow{}
	if o := m.alertHealth(now, windows); len(o) != 1 || !o[0].Good {
		t.Fatal(o)
	}
	m.wifi[1] = &moduleWiFi{Enabled: true, ICCID: "89123456789012345678", running: true}
	if o := m.alertHealth(now, windows); len(o) != 0 {
		t.Fatal("missing reconnect grace", o)
	}
	later := now.Add(wifiAlertGrace + time.Second)
	m.lastScan = later
	o := m.alertHealth(later, windows)
	if len(o) != 1 || o[0].Good || o[0].Reason != "WIFI_REGISTRATION_FAILED" {
		t.Fatal(o)
	}
	m.wifi[1].Issue = "WIFI_ROAMING_RESTRICTED"
	if o = m.alertHealth(later, windows); len(o) != 1 || o[0].Reason != "WIFI_ROAMING_RESTRICTED" {
		t.Fatal("lost explicit operator response", o)
	}
	m.wifi[1].Issue = ""
	m.wifi[1].Registered = true
	if o = m.alertHealth(later, windows); len(o) != 1 || !o[0].Good {
		t.Fatal("wifi recovery", o)
	}
	m.jobs[1] = moduleJob{State: "running"}
	if len(m.alertHealth(later, windows)) != 0 {
		t.Fatal("counted busy job")
	}
	delete(m.jobs, 1)
	m.wifi[1].refreshUntil = later.Add(time.Minute)
	if len(m.alertHealth(later, windows)) != 0 {
		t.Fatal("counted refresh")
	}
	delete(m.seen, c.Key)
	if len(m.alertHealth(later, windows)) != 0 {
		t.Fatal("counted offline")
	}
}

func TestWiFiAlertRecoveryEvidence(t *testing.T) {
	now := time.Now()
	c := hardware.Candidate{Key: "wifi-alert", Kind: "usb"}
	m := newModuleManager(nil, nil)
	m.ready, m.lastScan = true, now
	m.seen[c.Key] = c
	m.values[1] = moduleSample{c, hardware.Reading{Responsive: true, ICCID: "89123456789012345678", UpdatedAt: now}}
	w := &moduleWiFi{Enabled: true, ICCID: "89123456789012345678", running: true}
	w.setRegistered(false, now)
	m.wifi[1] = w
	windows := map[int64]healthWindow{}
	m.alertHealth(now, windows)
	for _, elapsed := range []time.Duration{90 * time.Second, wifiAlertGrace - time.Second} {
		at := now.Add(elapsed)
		m.lastScan = at
		if got := m.alertHealth(at, windows); len(got) != 0 {
			t.Fatal("transient disconnect counted", got)
		}
		if got := m.wifiAlertDelivery(1, at); got != "pending" {
			t.Fatal("transient notification", got)
		}
	}
	at := now.Add(wifiAlertGrace)
	m.lastScan = at
	if got := m.alertHealth(at, windows); len(got) != 1 || got[0].Good {
		t.Fatal("sustained failure hidden", got)
	}
	if got := m.wifiAlertDelivery(1, at); got != "sending" {
		t.Fatal("sustained notification blocked", got)
	}
	w.setRegistered(true, at.Add(time.Second))
	if got := m.wifiAlertDelivery(1, at.Add(time.Second)); got != "cancelled" {
		t.Fatal("recovered notification sent", got)
	}
	// Connected then lost again before the next 30-second observation.
	w.setRegistered(false, at.Add(5*time.Second))
	at = at.Add(30 * time.Second)
	m.lastScan = at
	if got := m.alertHealth(at, windows); len(got) != 1 || !got[0].Good || !got[0].At.Equal(w.registeredAt) {
		t.Fatal("short recovery lost", got)
	}
	if got := m.wifiAlertDelivery(1, at); got != "pending" {
		t.Fatal("new outage inherited old grace", got)
	}
	w.Enabled = false
	if got := m.wifiAlertDelivery(1, at); got != "cancelled" {
		t.Fatal("disabled Wi-Fi notification", got)
	}
	m.lastScan = at.Add(-time.Minute)
	if got := m.wifiAlertDelivery(1, at); got != "pending" {
		t.Fatal("stale state treated as evidence", got)
	}
}

func TestWiFiAlertViewUsesLiveRegistration(t *testing.T) {
	for _, tc := range []struct{ enabled, registered, active, wantError bool }{
		{true, true, true, false}, {false, false, true, false},
		{true, false, false, false}, {true, false, true, true},
	} {
		view := map[string]any{"id": "module-1", "status": "online", "wifi": map[string]any{"enabled": tc.enabled, "registered": tc.registered}}
		all := map[string][]moduleAlert{"module-1": {{Kind: "module", Active: tc.active, Reason: "WIFI_REGISTRATION_FAILED"}, {Kind: "sms", Active: true}}}
		attachModuleAlerts(view, all)
		if (view["status"] == "error") != tc.wantError {
			t.Fatal(tc, view)
		}
		if len(all["module-1"]) != 2 {
			t.Fatal("shared alert cache mutated")
		}
	}
	view := map[string]any{"id": "module-1", "status": "online", "wifi": map[string]any{"enabled": true, "registered": false, "recovering": true}}
	attachModuleAlerts(view, map[string][]moduleAlert{"module-1": {{Kind: "module", Active: true, Reason: "WIFI_REGISTRATION_FAILED"}}})
	if view["status"] != "online" || len(view["alerts"].([]moduleAlert)) != 0 {
		t.Fatal("old alarm overrode ongoing full recovery", view)
	}
}

func testAlertsDatabase(t *testing.T, s *server, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	var defaults alertSettings
	if err := s.db.QueryRow(ctx, "SELECT sip,message,module,revision FROM alert_settings").Scan(&defaults.SIP, &defaults.Message, &defaults.Module, &defaults.Revision); err != nil || defaults.SIP != 5 || defaults.Message != 5 || defaults.Module != 5 {
		t.Fatal("new installations must default to five", defaults, err)
	}
	var mid int64
	err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('alert-fixture','alert-fixture','usb','告警测试','alert-fixture') RETURNING id`).Scan(&mid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		exec("DELETE FROM messages WHERE module_id=$1", mid)
		exec("DELETE FROM sip_call_records WHERE account_id='alert-fixture'")
		exec("DELETE FROM modules WHERE id=$1", mid)
		exec("UPDATE telegram_settings SET token='',proxy='',admin_id='',notification_id='',revision=1")
		exec("UPDATE alert_settings SET sip=5,message=5,module=5,revision=1")
	}()
	exec("UPDATE alert_settings SET sip=2,message=2,module=2")
	exec(`ALTER TABLE alert_settings ALTER COLUMN message SET DEFAULT 3,
 DROP CONSTRAINT alert_settings_sip_check, ADD CHECK(sip BETWEEN 1 AND 100),
 DROP CONSTRAINT alert_settings_message_check, ADD CHECK(message BETWEEN 1 AND 100),
 DROP CONSTRAINT alert_settings_module_check, ADD CHECK(module BETWEEN 1 AND 100)`)
	exec(alertSchema)
	var settingsPreserved bool
	if err := s.db.QueryRow(ctx, "SELECT sip=2 AND message=2 AND module=2 AND revision=1 FROM alert_settings").Scan(&settingsPreserved); err != nil || !settingsPreserved {
		t.Fatal("migration replaced custom settings", err)
	}
	const cardA = "89123456789012345678"
	const cardB = "89123456789012345679"
	exec("UPDATE modules SET active_card=$2 WHERE id=$1", mid, cardA)
	check := func(kind string, n int, active bool) {
		t.Helper()
		var got int
		var a bool
		err := s.db.QueryRow(ctx, "SELECT failures,active FROM alert_counters WHERE module_id=$1 AND kind=$2", mid, kind).Scan(&got, &a)
		if err != nil || got != n || a != active {
			t.Fatalf("%s got %d/%v, want %d/%v (%v)", kind, got, a, n, active, err)
		}
	}
	countQueue := func(want int) {
		t.Helper()
		var n int
		s.db.QueryRow(ctx, "SELECT count(*) FROM alert_notifications WHERE module_id=$1 AND state='pending'", mid).Scan(&n)
		if n != want {
			t.Fatalf("queue %d != %d", n, want)
		}
	}
	message := func(id, kind, state string) {
		t.Helper()
		exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state) VALUES($1,$2,$3,'fixture','+15555550000',true,$4,'queued')`, id, mid, cardA, kind)
		exec("UPDATE messages SET state=$2,issue='SMS_REJECTED' WHERE id=$1", id, state)
	}
	message("alert-1", "sms", "failed")
	check("sms", 1, false)
	var workers sync.WaitGroup
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			_, e := s.db.Exec(ctx, "SELECT alert_observe($1,1,'mms',$2,false,'MMS_REJECTED',clock_timestamp())", mid, fmt.Sprint("alert-concurrent-", i))
			failures <- e
		}(i)
	}
	workers.Wait()
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	check("mms", 12, true)
	exec("SELECT alert_observe($1,1,'mms','alert-concurrent-reset',true,'',clock_timestamp())", mid)
	exec("UPDATE messages SET state='failed' WHERE id='alert-1'")
	check("sms", 0, false)
	message("alert-2", "sms", "failed")
	check("sms", 1, false)
	message("alert-2b", "sms", "failed")
	check("sms", 2, true)
	countQueue(0)
	message("alert-mms", "mms", "accepted")
	check("sms", 0, false)
	check("mms", 0, false)
	message("alert-3", "sms", "accepted")
	check("sms", 0, false)
	exec("UPDATE messages SET state='failed' WHERE id='alert-3'")
	check("sms", 0, false)
	tokenValue := "123456:" + strings.Repeat("x", 30)
	exec("UPDATE telegram_settings SET token=$1,notification_id='-123'", tokenValue)
	message("alert-4", "sms", "failed")
	message("alert-5", "sms", "failed")
	message("alert-6", "sms", "failed")
	check("sms", 3, true)
	countQueue(1)
	message("alert-pending", "sms", "waiting_network")
	check("sms", 3, true)
	exec("UPDATE modules SET active_card=$2 WHERE id=$1", mid, cardB)
	check("sms", 0, false)
	countQueue(0)
	exec("UPDATE modules SET active_card=$2 WHERE id=$1", mid, cardA)
	exec("UPDATE messages SET state='failed' WHERE id='alert-pending'")
	check("sms", 0, false)
	var epoch int
	s.db.QueryRow(ctx, "SELECT card_epoch FROM modules WHERE id=$1", mid).Scan(&epoch)
	if epoch != 3 {
		t.Fatal("A-B-A epoch", epoch)
	}
	uncertain := func(id, issue, result string) {
		t.Helper()
		message(id, "sms", "sending")
		exec("UPDATE messages SET state='unknown',issue=$2,result=$3::jsonb WHERE id=$1", id, issue, result)
	}
	for i, result := range []string{`{}`, `{"partsAttempted":0}`, `{"partsAttempted":"1"}`, `{"partsAttempted":null}`} {
		uncertain(fmt.Sprint("alert-no-submit-", i), "SMS_OUTCOME_UNKNOWN", result)
		check("sms", 0, false)
	}
	uncertain("alert-receipt-missing", "SMS_DELIVERY_UNCONFIRMED", `{"partsAttempted":1}`)
	check("sms", 0, false)
	uncertain("alert-unconfirmed-1", "SMS_OUTCOME_UNKNOWN", `{"partsAttempted":1,"partsAccepted":0}`)
	check("sms", 1, false)
	uncertain("alert-unconfirmed-2", "SMS_OUTCOME_UNKNOWN", `{"partsAttempted":1,"partsAccepted":0}`)
	check("sms", 2, true)
	countQueue(1)
	exec("UPDATE messages SET state='unknown' WHERE id='alert-unconfirmed-2'")
	exec("UPDATE messages SET state='failed',issue='SMS_REJECTED' WHERE id='alert-unconfirmed-2'")
	check("sms", 2, true)
	countQueue(1)
	exec("UPDATE messages SET state='delivered',issue='' WHERE id='alert-unconfirmed-1'")
	check("sms", 1, false) // Late success leaves the newer failure intact.
	countQueue(0)
	message("alert-late-confirmation-recovered", "sms", "accepted")
	check("sms", 0, false)
	message("alert-mixed-failure", "sms", "failed")
	uncertain("alert-mixed-unconfirmed", "SMS_OUTCOME_UNKNOWN", `{"partsAttempted":1}`)
	check("sms", 2, true)
	countQueue(1)
	exec("UPDATE messages SET state='delivered' WHERE id='alert-unconfirmed-1'")
	check("sms", 2, true)
	message("alert-unconfirmed-recovered", "sms", "accepted")
	check("sms", 0, false)
	countQueue(0)
	message("alert-backfilled", "sms", "sending")
	exec("SELECT alert_observe($1,3,'sms','message:alert-backfilled',false,'SMS_OUTCOME_UNKNOWN',clock_timestamp())", mid)
	check("sms", 1, false)
	exec(`UPDATE messages SET state='unknown',issue='SMS_OUTCOME_UNKNOWN',result='{"partsAttempted":1}'::jsonb WHERE id='alert-backfilled'`)
	check("sms", 1, false)
	exec("UPDATE messages SET state='delivered',issue='' WHERE id='alert-backfilled'")
	check("sms", 0, false)
	message("alert-7", "sms", "failed")
	exec("UPDATE modules SET active_card=$2 WHERE id=$1", mid, cardA)
	check("sms", 1, false)
	call := func(id, direction, outcome string, ring, answered bool) {
		t.Helper()
		exec(`INSERT INTO sip_call_records(id,account_id,module_id,peer,direction) VALUES($1,'alert-fixture',$2,'+15555550000',$3)`, id, moduleID(mid), direction)
		exec(`UPDATE sip_call_records SET outcome=$2,ended_at=clock_timestamp(),alert_ringing=$3,answered_at=CASE WHEN $4 THEN clock_timestamp() END WHERE id=$1`, id, outcome, ring, answered)
	}
	for i, reason := range []string{"busy", "rejected", "no_answer", "cancelled", "module_busy", "failed", "call_timeout", "peer_unavailable"} {
		call(fmt.Sprint("alert-ignore-", i), "outgoing", reason, false, false)
	}
	call("alert-inbound", "incoming", "module_error", false, false)
	call("alert-call-1", "outgoing", "not_registered", false, false)
	check("sip", 1, false)
	call("alert-call-2", "outgoing", "carrier_rejected", false, false)
	check("sip", 2, true)
	countQueue(1)
	call("alert-call-ring", "outgoing", "no_answer", true, false)
	check("sip", 0, false)
	countQueue(0)
	call("alert-call-3", "outgoing", "module_error", false, false)
	check("sip", 1, false)
	call("alert-call-ok", "outgoing", "connected", false, true)
	check("sip", 0, false)
	call("alert-call-dial-failed-1", "outgoing", "dial_failed", false, false)
	check("sip", 1, false)
	call("alert-call-dial-failed-2", "outgoing", "dial_failed", false, false)
	check("sip", 2, true)
	countQueue(1)
	exec("UPDATE sip_call_records SET state='ended' WHERE id='alert-call-dial-failed-2'")
	check("sip", 2, true)
	call("alert-call-dial-recovered", "outgoing", "no_answer", true, false)
	check("sip", 0, false)
	countQueue(0)
	call("alert-disable-1", "outgoing", "dial_failed", false, false)
	call("alert-disable-2", "outgoing", "dial_failed", false, false)
	check("sip", 2, true)
	exec("UPDATE alert_settings SET sip=0")
	check("sip", 0, false)
	countQueue(0)
	call("alert-while-disabled", "outgoing", "dial_failed", false, false)
	check("sip", 0, false)
	exec("UPDATE alert_settings SET sip=2")
	call("alert-reenabled", "outgoing", "dial_failed", false, false)
	check("sip", 1, false)
	call("alert-reenabled-ok", "outgoing", "connected", false, true)
	check("sip", 0, false)
	check("sms", 0, false)
	message("alert-after-sip-success", "sms", "failed")
	check("sms", 1, false)
	exec("SELECT alert_observe($1,0,'module','fixture-health-1',false,'READ_TIMEOUT',clock_timestamp())", mid)
	exec("SELECT alert_observe($1,0,'module','fixture-health-1',false,'READ_TIMEOUT',clock_timestamp())", mid)
	check("module", 1, false)
	exec("SELECT alert_observe($1,0,'module','fixture-health-2',false,'READ_TIMEOUT',clock_timestamp())", mid)
	check("module", 2, true)
	exec("SELECT alert_observe($1,0,'module','fixture-health-ok',true,'',clock_timestamp())", mid)
	check("module", 0, false)
	countQueue(0)
	message("alert-disable-sms", "sms", "failed")
	message("alert-disable-mms-1", "mms", "failed")
	message("alert-disable-mms-2", "mms", "failed")
	exec("SELECT alert_observe($1,0,'module','fixture-disable-1',false,'READ_TIMEOUT',clock_timestamp())", mid)
	exec("SELECT alert_observe($1,0,'module','fixture-disable-2',false,'READ_TIMEOUT',clock_timestamp())", mid)
	countQueue(3)
	exec("UPDATE alert_settings SET message=0")
	check("sms", 0, false)
	check("mms", 0, false)
	check("module", 2, true)
	countQueue(1)
	exec("UPDATE alert_settings SET module=0")
	check("module", 0, false)
	countQueue(0)
	exec("UPDATE alert_settings SET message=2,module=2")
	// A concurrent observation either precedes the reset or sees disabled limits.
	gate := make(chan struct{})
	racing := make(chan error, 9)
	raceCtx, stopRace := context.WithTimeout(ctx, 10*time.Second)
	defer stopRace()
	for i := 0; i < 9; i++ {
		go func(i int) {
			<-gate
			var err error
			if i == 0 {
				_, err = s.db.Exec(raceCtx, "UPDATE alert_settings SET message=0")
			} else {
				_, err = s.db.Exec(raceCtx, "SELECT alert_observe($1,$2,'sms',$3,false,'SMS_REJECTED',clock_timestamp())", mid, epoch, fmt.Sprint("alert-disable-race-", i))
			}
			racing <- err
		}(i)
	}
	close(gate)
	for i := 0; i < 9; i++ {
		if err := <-racing; err != nil {
			t.Fatal("disable race", err)
		}
	}
	check("sms", 0, false)
	countQueue(0)
	request := func(method, path, body string, auth, protect bool, want int) map[string]any {
		t.Helper()
		r := localRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", s.origin)
		if auth {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		if protect {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), tokenValue) {
			t.Fatal("token exposed")
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	request("GET", "/api/settings/developer", "", false, false, 401)
	request("PATCH", "/api/settings/developer", `{"revision":1}`, true, false, 403)
	data := request("GET", "/api/settings/developer", "", true, false, 200)["data"].(map[string]any)
	if data["hasBotToken"] != true {
		t.Fatal(data)
	}
	request("PATCH", "/api/settings/developer", `{"revision":1,"adminId":"123","telegramProxy":"127.0.0.1:1080:u:p"}`, true, true, 200)
	cfg, e := s.readTelegram(ctx)
	if e != nil || cfg.Token != tokenValue || cfg.Proxy == "" {
		t.Fatal("omitted secret lost")
	}
	request("PATCH", "/api/settings/developer", `{"revision":1}`, true, true, 409)
	request("PATCH", "/api/settings/developer", `{"revision":2,"botToken":null,"telegramProxy":null}`, true, true, 200)
	cfg, e = s.readTelegram(ctx)
	if e != nil || cfg.Token != "" || cfg.Proxy != "" {
		t.Fatal("clear failed")
	}
	for _, body := range []string{`{"revision":1}`, `{"sip":null,"message":5,"module":5,"revision":1}`, `{"sip":-1,"message":5,"module":5,"revision":1}`} {
		request("PUT", "/api/settings/alerts", body, true, true, 400)
	}
	request("PUT", "/api/settings/alerts", `{"sip":0,"message":0,"module":0,"revision":1}`, true, true, 200)
	exec(alertSchema)
	if err := s.db.QueryRow(ctx, "SELECT sip=0 AND message=0 AND module=0 AND revision=2 FROM alert_settings").Scan(&settingsPreserved); err != nil || !settingsPreserved {
		t.Fatal("migration reopened disabled alerts", err)
	}
	message("alert-disabled-sms", "sms", "failed")
	uncertain("alert-disabled-unconfirmed", "SMS_OUTCOME_UNKNOWN", `{"partsAttempted":1}`)
	message("alert-disabled-mms", "mms", "failed")
	exec("SELECT alert_observe($1,0,'module','fixture-disabled',false,'READ_TIMEOUT',clock_timestamp())", mid)
	for _, kind := range []string{"sip", "sms", "mms", "module"} {
		check(kind, 0, false)
	}
	countQueue(0)
	request("PUT", "/api/settings/alerts", `{"sip":5,"message":5,"module":5,"revision":1}`, true, true, 409)
	request("PUT", "/api/settings/alerts", `{"sip":5,"message":5,"module":5,"revision":2}`, true, true, 200)
	exec("UPDATE messages SET state='unknown' WHERE id='alert-disabled-unconfirmed'")
	check("sms", 0, false)
	uncertain("alert-old-card-unconfirmed", "SMS_OUTCOME_UNKNOWN", `{"partsAttempted":1}`)
	check("sms", 1, false)
	exec("UPDATE modules SET active_card=$2 WHERE id=$1", mid, cardB)
	exec("SELECT alert_observe($1,4,'sms','alert-new-card-failure',false,'SMS_REJECTED',clock_timestamp())", mid)
	check("sms", 1, false)
	exec("UPDATE messages SET state='delivered',issue='' WHERE id='alert-old-card-unconfirmed'")
	check("sms", 1, false)
}
