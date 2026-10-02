package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeveloperSettingsValidation(t *testing.T) {
	for _, raw := range []string{"http://example.com/hook", "https://user:pass@example.com", "https://127.0.0.1/hook", "https://[::1]/", "https://169.254.169.254/", "https://example.com:8080/", "https://example.com/#fragment"} {
		if validWebhook(raw) {
			t.Fatal("unsafe webhook URL", raw)
		}
	}
	for _, raw := range []string{"", "https://example.com/hook?tenant=fixture"} {
		if !validWebhook(raw) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "::ffff:192.168.1.1", "100.64.0.1", "169.254.169.254", "fe80::1", "fc00::1", "64:ff9b::a00:1", "2001:db8::1", "0.0.0.0", "224.0.0.1", "2002:0a00:0001::1"} {
		if webhookPublicIP(netip.MustParseAddr(raw)) {
			t.Fatal("private webhook dial allowed", raw)
		}
	}
	if !webhookPublicIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public IPv4 rejected")
	}
	if messageDisplayStatus, _ := messageDisplay("accepted"); messageDisplayStatus != "delivered" {
		t.Fatal("display agreement lost")
	}
	if status, text := messageDisplay("waiting_network"); status != "pending" || text != "等待网络" {
		t.Fatal("network wait marked failed", status, text)
	}
	view := developerMessageView(messageView{State: "accepted"}, 1)
	if !view.CarrierAccepted || view.DeliveryConfirmed {
		t.Fatal("API fabricated a delivery receipt")
	}
}
func TestDeveloperBlankSecretPreserved(t *testing.T) {
	old := developerSettings{Revision: 1, KeyHash: tokenHash("fixture-api-key-123456"), Secret: "fixture-signing-key-123456"}
	next, changed, err := (developerPatch{Revision: 1, Key: json.RawMessage(`""`)}).apply(old)
	if err != nil || changed || !bytes.Equal(next.KeyHash, old.KeyHash) || next.Secret != old.Secret {
		t.Fatal("blank secret changed settings", err)
	}
	next, changed, err = (developerPatch{Revision: 1, Key: json.RawMessage(`null`)}).apply(old)
	if err != nil || !changed || len(next.KeyHash) != 0 || next.Secret != "" {
		t.Fatal("explicit clearing failed", err)
	}
}

func TestWebhookAPIKeySigning(t *testing.T) {
	const key = "fixture-api-key-1234567890"
	const expected = "5eeaa1f0faffcc58740b28383d87d12433bb3126e61826e82f1a79bb5283663c"
	if webhookSigningSecret(nil) != "" || webhookSigningSecret(tokenHash(key)) != expected {
		t.Fatal("signing derivation differs from the documented Python algorithm")
	}
	url := "https://example.com/hook"
	keyJSON, _ := json.Marshal(key)
	next, changed, err := (developerPatch{Revision: 1, Key: keyJSON, Webhook: &url}).apply(developerSettings{Revision: 1})
	if err != nil || !changed || next.Secret != expected {
		t.Fatal("new webhook requires an extra secret", next, err)
	}
	stored := developerSettings{Revision: 1, KeyHash: tokenHash(key)}
	fromStored, _, err := (developerPatch{Revision: 1, Webhook: &url}).apply(stored)
	if err != nil || fromStored.Secret != expected {
		t.Fatal("saved API key not reused", err)
	}
	if _, _, err = (developerPatch{Revision: 1, Webhook: &url}).apply(developerSettings{Revision: 1}); err == nil {
		t.Fatal("enabled a new webhook without any API key")
	}
	legacy := developerSettings{Revision: 1, KeyHash: tokenHash(key), Webhook: url, Secret: "legacy-signature-key-1234"}
	unchanged, changed, err := (developerPatch{Revision: 1, Key: json.RawMessage(`""`), Webhook: &url}).apply(legacy)
	if err != nil || changed || unchanged.Secret != legacy.Secret {
		t.Fatal("ordinary save broke an existing integration", err)
	}
	converted, changed, err := (developerPatch{Revision: 1, Key: keyJSON}).apply(legacy)
	if err != nil || !changed || converted.Secret != expected {
		t.Fatal("explicit key save did not enable automatic signing", err)
	}
	replacement, _, err := (developerPatch{Revision: 1, Key: json.RawMessage(`"replacement-api-key-123456"`)}).apply(next)
	if err != nil || replacement.Secret == expected || replacement.Secret != webhookSigningSecret(replacement.KeyHash) {
		t.Fatal("key rotation retained the old signing key", err)
	}
	disabled, _, err := (developerPatch{Revision: 1, Key: json.RawMessage(`null`)}).apply(next)
	if err != nil || disabled.Secret != "" || disabled.Webhook != url {
		t.Fatal("clearing API key must stop signing without losing the address", err)
	}
	empty := ""
	disabled, _, err = (developerPatch{Revision: 1, Webhook: &empty}).apply(next)
	if err != nil || disabled.Secret != "" || !bytes.Equal(disabled.KeyHash, next.KeyHash) {
		t.Fatal("clearing address changed API credentials", err)
	}
}

type webhookRoundTrip func(*http.Request) (*http.Response, error)

func (f webhookRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testDeveloperRuntimeDatabase(t *testing.T, parent *server) {
	t.Helper()
	ctx := context.Background()
	salt, hash, err := newPassword("runtime-developer-fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	err = parent.db.QueryRow(ctx, `INSERT INTO administrators(username,password_salt,password_hash,password_iterations)
 VALUES('runtime-developer-fixture',$1,$2,$3) RETURNING id::text`, salt, hash, passwordIterations).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		parent.db.Exec(ctx, "DELETE FROM login_sessions WHERE administrator_id=$1", id)
		parent.db.Exec(ctx, "DELETE FROM administrators WHERE id=$1", id)
	}()
	cookie, csrf := token(), token()
	_, err = parent.db.Exec(ctx, `INSERT INTO login_sessions(token_hash,administrator_id,csrf_token,expires_at)
 VALUES($1,$2,$3,now()+interval '1 hour')`, tokenHash(cookie), id, csrf)
	if err != nil {
		t.Fatal(err)
	}
	testDeveloperDatabase(t, &server{db: parent.db, origin: "http://test.local", slots: make(chan struct{}, 2)}, cookie, csrf)
}

func testDeveloperDatabase(t *testing.T, s *server, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	previous, err := s.readDeveloper(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldManager := s.modules
	defer func() {
		s.modules = oldManager
		s.db.Exec(ctx, "DELETE FROM developer_events")
		s.db.Exec(ctx, "DELETE FROM developer_uploads")
		s.db.Exec(ctx, "DELETE FROM developer_module_state")
		s.db.Exec(ctx, "UPDATE developer_settings SET key_hash=$1,webhook=$2,webhook_secret=$3,revision=$4", previous.KeyHash, previous.Webhook, previous.Secret, previous.Revision)
	}()
	request := func(method, path string, body any, admin bool, key string, want int) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		r := localRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		if admin {
			r.Header.Set("Origin", s.origin)
			r.Header.Set("X-CSRF-Token", csrf)
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		return w
	}
	tg, err := s.readTelegram(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := "fixture-api-key-1234567890"
	secret := webhookSigningSecret(tokenHash(key))
	payload := map[string]any{"revision": tg.Revision, "apiRevision": previous.Revision, "apiKey": key, "webhook": "https://example.com/hook"}
	w := request("PATCH", "/api/settings/developer", payload, true, "", 200)
	if strings.Contains(w.Body.String(), `"username"`) || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), secret) {
		t.Fatal("settings leaked secret")
	}
	saved, err := s.readDeveloper(ctx)
	if err != nil || saved.Secret != secret {
		t.Fatal("API settings did not derive the webhook signing key", err)
	}
	var tgAfter int64
	if err = s.db.QueryRow(ctx, "SELECT revision FROM telegram_settings").Scan(&tgAfter); err != nil || tgAfter != tg.Revision {
		t.Fatal("API settings invalidated Telegram notifications", err)
	}
	request("GET", "/api/v1/modules", nil, false, "", 401)
	request("GET", "/api/v1/modules", nil, true, "", 401)
	request("GET", "/api/v1/modules", nil, false, "incorrect-api-key-123456", 401)
	request("GET", "/api/v1/modules?apiKey=forbidden", nil, false, key, 400)
	request("GET", "/api/settings/developer", nil, false, key, 401)
	request("POST", "/api/v1/modules", map[string]any{}, false, key, 404)
	sample := wifiModuleFixture()
	sample.Candidate.Key = "usb:developer-fixture"
	sample.Reading.IMEI = "990000000007711"
	sample.Reading.ICCID = "89000000000000777101"
	sample.Reading.Number = "+12025550177"
	v, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.db.Exec(ctx, "DELETE FROM messages WHERE module_id=$1", v.ID)
		s.db.Exec(ctx, "DELETE FROM developer_module_state WHERE module_id=$1", v.ID)
		s.db.Exec(ctx, "DELETE FROM modules WHERE id=$1", v.ID)
	}()
	f := &fixtureSMS{}
	m := newModuleManagerWithWiFi(s.db, nil, f)
	m.ctx = ctx
	m.ready = true
	m.lastScan = time.Now()
	m.values[v.ID] = sample
	m.seen[sample.Candidate.Key] = sample.Candidate
	m.wifi[v.ID] = &moduleWiFi{Enabled: true, Registered: true, SMSReady: true, running: true, ICCID: sample.Reading.ICCID}
	s.modules = m
	w = request("GET", "/api/v1/modules", nil, false, key, 200)
	for _, legacy := range []string{"", "different-legacy-name"} {
		r := localRequest("GET", "/api/v1/modules", nil)
		r.Header.Set("X-API-Key", key)
		if legacy != "" {
			r.Header.Set("X-API-Username", legacy)
		}
		check := httptest.NewRecorder()
		s.ServeHTTP(check, r)
		if check.Code != 200 {
			t.Fatalf("key-only request: %d", check.Code)
		}
		r.Header.Add("X-API-Key", key)
		check = httptest.NewRecorder()
		s.ServeHTTP(check, r)
		if check.Code != 401 {
			t.Fatal("duplicate key accepted")
		}
	}
	if !strings.Contains(w.Body.String(), sample.Reading.Number) || strings.Contains(w.Body.String(), sample.Reading.ICCID) {
		t.Fatal("module projection invalid", w.Body.String())
	}
	in := messageInput{RequestID: "developer-message-0001", ModuleID: moduleID(v.ID), To: "+12025550199", Text: "fixture only"}
	request("POST", "/api/v1/messages", in, false, key, 202)
	request("POST", "/api/v1/messages", in, false, key, 200)
	in.Text = "conflict"
	request("POST", "/api/v1/messages", in, false, key, 409)
	in.Text = "fixture only"
	m.processMessage(ctx, v.ID)
	if f.calls != 1 {
		t.Fatal("API enqueued more than one send", f.calls)
	}
	w = request("GET", "/api/v1/messages/"+in.RequestID, nil, false, key, 200)
	if !strings.Contains(w.Body.String(), `"carrierAccepted":true`) || !strings.Contains(w.Body.String(), `"deliveryConfirmed":false`) {
		t.Fatal("raw and display outcomes conflated", w.Body.String())
	}
	request("GET", "/api/v1/messages?after=0&direction=outgoing&limit=1", nil, false, key, 200)
	request("GET", "/api/v1/messages?after=-1", nil, false, key, 400)
	request("GET", "/api/v1/messages?limit=9999", nil, false, key, 400)
	// Retrying the original id remains stable even after a card replacement.
	s.db.Exec(ctx, "UPDATE modules SET active_card='89000000000000777102' WHERE id=$1", v.ID)
	request("POST", "/api/v1/messages", in, false, key, 200)
	in.RequestID = "developer-message-0002"
	request("POST", "/api/v1/messages", in, false, key, 409)
	s.db.Exec(ctx, "UPDATE modules SET active_card=$2 WHERE id=$1", v.ID, sample.Reading.ICCID)
	const gif = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw=="
	if _, err := s.db.Exec(ctx, "UPDATE retention_settings SET upload_hours=4"); err != nil {
		t.Fatal(err)
	}
	w = request("POST", "/api/v1/attachments", map[string]string{"image": gif}, false, key, 201)
	var upload struct {
		Data struct {
			ID        string
			ExpiresIn int
		}
	}
	json.Unmarshal(w.Body.Bytes(), &upload)
	if upload.Data.ExpiresIn != 14400 {
		t.Fatal("upload response used a fixed lifetime", upload.Data.ExpiresIn)
	}
	if _, err := s.db.Exec(ctx, "UPDATE developer_uploads SET created_at=now()-interval '3 hours' WHERE id=$1", upload.Data.ID); err != nil {
		t.Fatal(err)
	}
	in.RequestID = "developer-message-0003"
	in.Text = ""
	in.AttachmentID = upload.Data.ID
	request("POST", "/api/v1/messages", in, false, key, 202)
	w = request("GET", "/api/v1/messages/"+in.RequestID+"/image", nil, false, key, 200)
	if w.Header().Get("Content-Type") != "image/gif" {
		t.Fatal("GIF type lost")
	}
	if _, err := s.db.Exec(ctx, "UPDATE retention_settings SET upload_hours=1"); err != nil {
		t.Fatal(err)
	}
	expired := in
	expired.RequestID = "developer-message-expired"
	request("POST", "/api/v1/messages", expired, false, key, 404)
	request("POST", "/api/v1/messages", in, false, key, 200)
	if _, err := s.db.Exec(ctx, "UPDATE retention_settings SET upload_hours=2"); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(ctx, "DELETE FROM developer_uploads WHERE id=$1", upload.Data.ID)
	request("POST", "/api/v1/messages", in, false, key, 200)
	in.RequestID = "developer-message-0004"
	request("POST", "/api/v1/messages", in, false, key, 404)
	// Inline image submission cannot bypass the attachment rate budget.
	s.developerWork.mu.Lock()
	s.developerWork.buckets["upload"] = apiBucket{tokens: 0, at: time.Now().Add(time.Second)}
	s.developerWork.mu.Unlock()
	inline := messageInput{RequestID: "developer-inline-limit-01", ModuleID: moduleID(v.ID), To: "+12025550199", Image: gif}
	request("POST", "/api/v1/messages", inline, false, key, 429)
	var rejectedCount int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM messages WHERE id=$1", inline.RequestID).Scan(&rejectedCount); err != nil || rejectedCount != 0 {
		t.Fatal("limited inline upload was queued", err, rejectedCount)
	}
	s.developerWork.mu.Lock()
	delete(s.developerWork.buckets, "upload")
	s.developerWork.mu.Unlock()
	// Simultaneous request retries still insert once.
	in.AttachmentID = ""
	in.Text = "concurrent"
	in.RequestID = "developer-message-0005"
	config, _ := s.readDeveloper(ctx)
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, e := s.enqueueMessage(ctx, in, config.Revision)
			results <- created
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	created := 0
	for yes := range results {
		if yes {
			created++
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if created != 1 {
		t.Fatal("concurrent duplicate", created)
	}
	if err = s.publishDeveloperModules(ctx); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/api/v1/events?after=0", nil, false, key, 200)
	if !strings.Contains(w.Body.String(), "message.status_changed") || !strings.Contains(w.Body.String(), "module.updated") || strings.Contains(w.Body.String(), secret) {
		t.Fatal("event catch-up invalid", w.Body.String())
	}
	var event int64
	if err = s.db.QueryRow(ctx, "SELECT min(id) FROM developer_events WHERE state='pending'").Scan(&event); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: webhookRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		data, _ := io.ReadAll(r.Body)
		stamp := r.Header.Get("X-Rykvo-Timestamp")
		if r.Header.Get("X-Rykvo-Signature") != webhookSignature(secret, stamp, data) || r.Header.Get("X-Rykvo-Event-ID") == "" {
			t.Error("signature missing")
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("fixture")), Header: make(http.Header)}, nil
	})}
	s.deliverWebhook(ctx, event, client, nil)
	var state string
	var attempts int
	s.db.QueryRow(ctx, "SELECT state,attempts FROM developer_events WHERE id=$1", event).Scan(&state, &attempts)
	if state != "pending" || attempts != 1 || calls != 1 {
		t.Fatal("callback retry not persisted", state, attempts, calls)
	}
	s.db.Exec(ctx, "UPDATE developer_events SET next_at=now() WHERE id=$1", event)
	client.Transport = webhookRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	s.deliverWebhook(ctx, event, client, nil)
	s.deliverWebhook(ctx, event, client, nil)
	s.db.QueryRow(ctx, "SELECT state FROM developer_events WHERE id=$1", event).Scan(&state)
	if state != "delivered" || calls != 2 {
		t.Fatal("duplicate successful delivery", state, calls)
	}
	// Rotation denies old keys immediately, without changing browser authentication.
	testDeveloperBatchDatabase(t, s, key, moduleID(v.ID))
	payload = map[string]any{"revision": tg.Revision, "apiRevision": config.Revision, "apiKey": "replacement-api-key-123456"}
	request("PATCH", "/api/settings/developer", payload, true, "", 200)
	request("GET", "/api/v1/modules", nil, false, key, 401)
	request("GET", "/api/v1/modules", nil, false, "replacement-api-key-123456", 200)
	var pending int
	s.db.QueryRow(ctx, "SELECT count(*) FROM developer_events WHERE state IN ('pending','sending')").Scan(&pending)
	if pending != 0 {
		t.Fatal("rotated API key retained old signed deliveries")
	}
	rotated, err := s.readDeveloper(ctx)
	if err != nil || rotated.Webhook != saved.Webhook || rotated.Secret == secret || rotated.Secret != webhookSigningSecret(rotated.KeyHash) {
		t.Fatal("key rotation lost the webhook address or signing key", err)
	}
	if err = s.db.QueryRow(ctx, `SELECT developer_emit('webhook.test','rotation','{}'::jsonb)`).Scan(&event); err != nil {
		t.Fatal(err)
	}
	payload = map[string]any{"revision": tg.Revision, "apiRevision": rotated.Revision, "apiKey": nil}
	request("PATCH", "/api/settings/developer", payload, true, "", 200)
	request("GET", "/api/v1/modules", nil, false, "replacement-api-key-123456", 401)
	s.db.QueryRow(ctx, "SELECT state FROM developer_events WHERE id=$1", event).Scan(&state)
	if state != "cancelled" {
		t.Fatal("clearing API key left a queued signed event", state)
	}
	if err = s.db.QueryRow(ctx, `SELECT developer_emit('webhook.test','disabled','{}'::jsonb)`).Scan(&event); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(ctx, "SELECT state FROM developer_events WHERE id=$1", event).Scan(&state)
	if state != "disabled" {
		t.Fatal("cleared key still permits webhook delivery", state)
	}
}

func TestDeveloperSixCharacterKey(t *testing.T) {
	for _, key := range []string{"123456", "aB9_-!", strings.Repeat("a", 256)} {
		if !validDeveloperSecret(key) {
			t.Fatal("valid key rejected")
		}
	}
	for _, key := range []string{"12345", strings.Repeat("a", 257), "123 456", "123\t456", "123\x00456"} {
		if validDeveloperSecret(key) {
			t.Fatal("invalid key accepted")
		}
	}
	key := "123456"
	encoded, _ := json.Marshal(key)
	got, changed, err := (developerPatch{Revision: 1, Key: encoded}).apply(developerSettings{Revision: 1})
	if err != nil || !changed || !bytes.Equal(got.KeyHash, tokenHash(key)) {
		t.Fatal("six-character key was not hashed", err)
	}
}
