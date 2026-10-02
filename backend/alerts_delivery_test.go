package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func testWiFiAlertDeliveryDatabase(t *testing.T, s *server) {
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	oldManager, oldHost := s.modules, s.hostnameCall
	defer func() { s.modules, s.hostnameCall = oldManager, oldHost }()
	s.hostnameCall = func(context.Context, map[string]string) (hostnameStatus, error) {
		return hostnameStatus{Hostname: "fixture"}, nil
	}
	exec("UPDATE alert_settings SET module=1")
	exec("UPDATE telegram_settings SET token='test-only',notification_id='-1'")
	var id int64
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES('wifi-delivery','wifi-delivery','usb','wifi-delivery','wifi-delivery') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer func() {
		exec("DELETE FROM modules WHERE id=$1", id)
		exec("UPDATE alert_settings SET module=5")
		exec("UPDATE telegram_settings SET token='',notification_id=''")
	}()
	now := time.Now()
	m := newModuleManager(nil, nil)
	s.modules = m
	m.ready, m.lastScan = true, now
	c := hardware.Candidate{Key: "wifi-delivery", Kind: "usb"}
	m.seen[c.Key] = c
	m.values[id] = moduleSample{c, hardware.Reading{Responsive: true, ICCID: "89123456789012345678", UpdatedAt: now}}
	w := &moduleWiFi{Enabled: true, ICCID: "89123456789012345678", running: true}
	m.wifi[id] = w
	exec("SELECT alert_observe($1,0,'module','wifi-delivery-failed',false,'WIFI_REGISTRATION_FAILED',clock_timestamp())", id)
	check := func(want string) {
		t.Helper()
		var state string
		var attempts int
		if err := s.db.QueryRow(ctx, "SELECT state,attempts FROM alert_notifications WHERE module_id=$1", id).Scan(&state, &attempts); err != nil || state != want || attempts != 0 {
			t.Fatalf("%s/%d, want %s/0 (%v)", state, attempts, want, err)
		}
	}
	// An old queued failure must wait through the new process's recovery window.
	w.setRegistered(false, now)
	s.deliverAlert(ctx)
	check("pending")
	exec("UPDATE alert_notifications SET next_at=now() WHERE module_id=$1", id)
	w.setRegistered(true, time.Now())
	s.deliverAlert(ctx)
	check("cancelled")
	// A newer successful delivery replaces the old connection error in settings.
	exec(`INSERT INTO alert_notifications(module_id,kind,epoch,cycle,revision,failures,reason,state,issue)
 SELECT $1,'module',0,2,revision,1,'WIFI_REGISTRATION_FAILED','failed','TELEGRAM_CONNECTION_FAILED' FROM telegram_settings`, id)
	exec(`INSERT INTO alert_notifications(module_id,kind,epoch,cycle,revision,failures,reason,state)
 SELECT $1,'module',0,3,revision,1,'WIFI_REGISTRATION_FAILED','sent' FROM telegram_settings`, id)
	response := httptest.NewRecorder()
	s.developerSettingsAPI(ctx, response, httptest.NewRequest("GET", "/api/settings/developer", nil))
	var body struct {
		Data struct {
			State string `json:"deliveryState"`
			Issue string `json:"deliveryIssue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 200 || body.Data.State != "sent" || body.Data.Issue != "" {
		t.Fatal("successful notification retained an old error", response.Code, body.Data, err)
	}
}
