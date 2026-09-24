package main

import (
	"context"
	"testing"
	"time"
)

func phoneFixture() (*moduleManager, *moduleWiFi, moduleSample, moduleRecord) {
	m := newModuleManager(nil, nil)
	sample := wifiModuleFixture()
	m.lastScan = time.Now()
	m.values[1] = sample
	m.seen[sample.Candidate.Key] = sample.Candidate
	w := &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, running: true}
	m.wifi[1] = w
	return m, w, sample, moduleRecord{ID: 1, Endpoint: sample.Candidate.Key}
}

func TestIMSPhoneAppearsInListAndSIMWithoutOverwritingSIMNumber(t *testing.T) {
	m, w, sample, v := phoneFixture()
	m.acceptWiFiNumber(context.Background(), 1, w, sample, "+12025550123")
	server := &server{modules: m}
	view := server.moduleView(v)
	if view["number"] != "+12025550123" || view["sims"].([]any)[0].(map[string]any)["number"] != "+12025550123" {
		t.Fatal("IMS number not reflected in both views")
	}
	sample.Reading.Number = "+12025550124"
	m.values[1] = sample
	if server.moduleView(v)["number"] != "+12025550124" {
		t.Fatal("SIM number was overwritten")
	}
}

func TestIMSPhoneRejectsOldCardAndCancelledCallbacks(t *testing.T) {
	m, w, sample, v := phoneFixture()
	for _, number := range []string{"310026123456789", "89123456789012345678", "+123\n456", "+1234"} {
		m.acceptWiFiNumber(context.Background(), 1, w, sample, number)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.acceptWiFiNumber(ctx, 1, w, sample, "+12025550123")
	w.Enabled = false
	m.acceptWiFiNumber(context.Background(), 1, w, sample, "+12025550123")
	w.Enabled = true
	live := sample
	live.Reading.ICCID = "89123456789012345679"
	m.values[1] = live
	m.acceptWiFiNumber(context.Background(), 1, w, sample, "+12025550123")
	if len(m.phoneNumbers) != 0 {
		t.Fatal("invalid or stale number accepted")
	}
	m.phoneNumbers[sample.Reading.ICCID] = "+12025550123"
	if (&server{modules: m}).moduleView(v)["number"] != "" {
		t.Fatal("old card number leaked to new SIM")
	}
}

func testPhoneDatabase(t *testing.T, s *server) {
	t.Helper()
	m, w, sample, _ := phoneFixture()
	m.db = s.db
	m.acceptWiFiNumber(context.Background(), 1, w, sample, "+12025550123")
	restarted := newModuleManager(s.db, nil)
	restarted.loadPhoneNumbers(context.Background())
	if restarted.phoneNumbers[sample.Reading.ICCID] != "+12025550123" {
		t.Fatal("IMS number not restored after restart")
	}
	var source string
	if err := s.db.QueryRow(context.Background(), "SELECT source FROM card_phone_numbers WHERE iccid=$1", sample.Reading.ICCID).Scan(&source); err != nil || source != "ims" {
		t.Fatal("number source missing", err)
	}
}
