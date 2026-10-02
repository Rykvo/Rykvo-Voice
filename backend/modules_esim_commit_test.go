package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

// No normal polling runs between completion and acquiring Wi-Fi's device gate.
func testESIMCardCommitDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	m := newModuleManager(s.db, nil)
	m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
	previous := s.modules
	s.modules = m
	defer func() { s.modules = previous }()
	sample := wifiModuleFixture()
	sample.Candidate.Key = "usb:esim-card-commit"
	sample.Reading.SIM = "READY"
	sample.Reading.Number = "+12025550101"
	cardA, cardB := sample.Reading.ICCID, "89123456789012345679"
	eid := "89049032001001234500012345678901"
	v, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading)
	if err != nil {
		t.Fatal(err)
	}
	m.values[v.ID], m.seen[v.Endpoint] = sample, sample.Candidate
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		exec("DELETE FROM module_jobs WHERE module_id=$1", v.ID)
		exec("DELETE FROM developer_module_state WHERE module_id=$1", v.ID)
		exec("DELETE FROM developer_events WHERE resource_id=$1", moduleID(v.ID))
		exec("DELETE FROM modules WHERE id=$1", v.ID)
		exec("UPDATE developer_settings SET key_hash=''::bytea")
	}()
	exec("UPDATE developer_settings SET key_hash=decode(repeat('01',32),'hex')")
	var epoch int64
	if err := s.db.QueryRow(ctx, "SELECT card_epoch FROM modules WHERE id=$1", v.ID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	for index, card := range []string{cardB, cardA, cardA} {
		t.Run(fmt.Sprintf("activation-%d", index), func(t *testing.T) {
			j := moduleJob{ID: fmt.Sprintf("esim-card-commit-%d", index), Module: v.ID, Action: "enable", State: "succeeded", Stage: "done",
				Verification: &moduleVerification{EID: eid, ICCID: card, IMEI: sample.Reading.IMEI}}
			r := hardware.ESIMRequest{Candidate: sample.Candidate, Action: "enable", ExpectedIMEI: sample.Reading.IMEI, EID: eid, ICCID: card}
			reading := sample.Reading
			reading.ICCID, reading.UpdatedAt = card, time.Now()
			reading.Number = fmt.Sprintf("+1202555010%d", index+2)
			reading.ESIM = &hardware.ESIMInfo{EID: eid, Profiles: []hardware.ESIMProfile{{ICCID: card, Enabled: true}}}
			exec("INSERT INTO module_jobs(id,module_id,action,state,stage) VALUES($1,$2,'enable','running','verifying')", j.ID, v.ID)
			m.jobs[v.ID] = moduleJob{ID: j.ID, Module: v.ID, State: "running"}
			exec("SELECT alert_observe($1,$2,'sms',$3,false,'SMS_REJECTED',clock_timestamp())", v.ID, epoch, "old-"+j.ID)
			gate := m.gate(v.Endpoint)
			gate <- struct{}{}
			m.finishESIMJob(j, r, reading)
			<-gate
			// A running Wi-Fi worker prevents normal accept() from repairing the DB.
			gate <- struct{}{}
			defer func() { <-gate }()
			var current string
			var version int64
			if err := s.db.QueryRow(ctx, "SELECT active_card,card_epoch FROM modules WHERE id=$1", v.ID).Scan(&current, &version); err != nil {
				t.Fatal(err)
			}
			if index < 2 {
				epoch++
			}
			if current != card || version != epoch {
				t.Fatalf("completed eSIM job published before card commit: epoch=%d want=%d cardMatches=%v", version, epoch, current == card)
			}
			var failures int
			if err := s.db.QueryRow(ctx, "SELECT failures FROM alert_counters WHERE module_id=$1 AND kind='sms'", v.ID).Scan(&failures); err != nil {
				t.Fatal(err)
			}
			wantFailures := 0
			if index == 2 {
				wantFailures = 1 // Re-reading the same card is not another activation.
			}
			if failures != wantFailures {
				t.Fatalf("card-bound failures=%d want=%d", failures, wantFailures)
			}
			items, err := s.developerModules(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item["id"] == moduleID(v.ID) && (item["number"] != reading.Number || item["cardVersion"] != epoch) {
					t.Fatalf("developer API did not publish committed card/number: %+v", item)
				}
			}
			if err := s.publishDeveloperModules(ctx); err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if err := s.db.QueryRow(ctx, "SELECT data FROM developer_events WHERE resource_id=$1 AND event_type='module.updated' ORDER BY id DESC LIMIT 1", moduleID(v.ID)).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var event struct {
				Data struct {
					Number      string `json:"number"`
					CardVersion int64  `json:"cardVersion"`
				}
			}
			if json.Unmarshal(payload, &event) != nil || event.Data.Number != reading.Number || event.Data.CardVersion != epoch {
				t.Fatal("new number and card version missing from durable webhook event")
			}
		})
	}
	for _, scenario := range []string{"database-rollback", "imei", "eid", "inactive-profile", "usb-generation", "stale-job", "refreshing", "missing-iccid", "slow-database"} {
		t.Run(scenario, func(t *testing.T) {
			j := moduleJob{ID: "esim-commit-" + scenario, Module: v.ID, Action: "enable", State: "succeeded", Stage: "done"}
			r := hardware.ESIMRequest{Candidate: sample.Candidate, Action: "enable", ExpectedIMEI: sample.Reading.IMEI, EID: eid, ICCID: cardB}
			reading := sample.Reading
			reading.ICCID, reading.UpdatedAt = cardB, time.Now()
			reading.ESIM = &hardware.ESIMInfo{EID: eid, Profiles: []hardware.ESIMProfile{{ICCID: cardB, Enabled: true}}}
			exec("INSERT INTO module_jobs(id,module_id,action,state,stage) VALUES($1,$2,'enable','running','verifying')", j.ID, v.ID)
			m.jobs[v.ID] = moduleJob{ID: j.ID, Module: v.ID, State: "running"}
			switch scenario {
			case "refreshing":
				reading.SIM = "unknown"
			case "missing-iccid":
				reading.ICCID = ""
			case "database-rollback":
				exec("ALTER TABLE module_jobs ADD CONSTRAINT test_esim_completion_fail CHECK(id<>'esim-commit-database-rollback' OR state<>'succeeded')")
				defer exec("ALTER TABLE module_jobs DROP CONSTRAINT test_esim_completion_fail")
			case "imei":
				reading.IMEI = "999999999999999"
			case "eid":
				reading.ESIM.EID = "89049032001001234500012345678999"
			case "inactive-profile":
				reading.ESIM.Profiles[0].Enabled = false
			case "usb-generation":
				r.Candidate.Generation = "old-generation"
			case "stale-job":
				m.jobs[v.ID] = moduleJob{ID: "newer-job", Module: v.ID, State: "running"}
			}
			if scenario == "slow-database" {
				tx, err := s.db.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if _, err = tx.Exec(ctx, "SELECT id FROM modules WHERE id=$1 FOR UPDATE", v.ID); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() { m.finishESIMJob(j, r, reading); close(done) }()
				time.Sleep(25 * time.Millisecond)
				if !m.mu.TryLock() {
					t.Fatal("SQL blocked all module state reads")
				}
				if !m.jobs[v.ID].active() {
					m.mu.Unlock()
					t.Fatal("job published before commit")
				}
				m.mu.Unlock()
				if err = tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("completion did not resume")
				}
			} else {
				m.finishESIMJob(j, r, reading)
			}
			var card, state string
			var version int64
			if err := s.db.QueryRow(ctx, "SELECT active_card,card_epoch FROM modules WHERE id=$1", v.ID).Scan(&card, &version); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(ctx, "SELECT state FROM module_jobs WHERE id=$1", j.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if scenario == "slow-database" {
				if card != cardB || version != epoch+1 || state != "succeeded" {
					t.Fatal("successful commit missing")
				}
			} else if card != cardA || version != epoch || state == "succeeded" {
				t.Fatalf("failed/stale completion changed card: cardMatches=%v epoch=%d state=%s", card == cardA, version, state)
			}
			if scenario == "database-rollback" && m.values[v.ID].Reading.Issue != "DATABASE_UNAVAILABLE" {
				t.Fatal("uncommitted snapshot left usable after rollback")
			}
		})
	}
	for _, scenario := range []string{"empty-failed", "disabled-success", "empty-wrong-eid", "empty-wrong-imei", "empty-stale-generation"} {
		t.Run(scenario, func(t *testing.T) {
			var before string
			var epoch int64
			if err := s.db.QueryRow(ctx, "SELECT active_card,card_epoch FROM modules WHERE id=$1", v.ID).Scan(&before, &epoch); err != nil {
				t.Fatal(err)
			}
			j := moduleJob{ID: "esim-commit-" + scenario, Module: v.ID, Action: "download", State: "uncertain", Stage: "done", Issue: "ESIM_SERVER_REJECTED"}
			r := hardware.ESIMRequest{Candidate: sample.Candidate, Action: "download", ExpectedIMEI: sample.Reading.IMEI, EID: eid}
			reading := sample.Reading
			reading.ICCID = "89000000000000000001"
			reading.ESIM = &hardware.ESIMInfo{EID: eid}
			switch scenario {
			case "disabled-success":
				reading.ESIM.Profiles = []hardware.ESIMProfile{{ICCID: cardB}}
				j.State, j.Issue = "succeeded", ""
			case "empty-wrong-eid":
				reading.ESIM.EID = "89049032001001234500012345678999"
			case "empty-wrong-imei":
				reading.IMEI = "999999999999999"
			case "empty-stale-generation":
				r.Candidate.Generation = "old-generation"
			}
			exec("INSERT INTO module_jobs(id,module_id,action,state,stage) VALUES($1,$2,'download','running','verifying')", j.ID, v.ID)
			m.jobs[v.ID] = moduleJob{ID: j.ID, Module: v.ID, State: "running"}
			m.finishESIMJob(j, r, reading)
			var card, state, issue string
			var version int64
			if err := s.db.QueryRow(ctx, "SELECT active_card,card_epoch FROM modules WHERE id=$1", v.ID).Scan(&card, &version); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(ctx, "SELECT state,issue FROM module_jobs WHERE id=$1", j.ID).Scan(&state, &issue); err != nil {
				t.Fatal(err)
			}
			wantState, wantIssue := j.State, j.Issue
			if scenario != "empty-failed" && scenario != "disabled-success" {
				wantState, wantIssue = "uncertain", "DEVICE_CHANGED"
			}
			if card != before || version != epoch || state != wantState || issue != wantIssue {
				t.Fatalf("empty inventory changed identity or lost result: card=%v epoch=%d state=%s issue=%s", card == before, version, state, issue)
			}
		})
	}
	t.Run("reader-without-imei", func(t *testing.T) {
		c := hardware.Candidate{Key: "pcsc:commit", Kind: "reader", Reader: "fixture", Generation: "1"}
		reading := hardware.Reading{Responsive: true, SIM: "READY", ICCID: cardA, ReaderSerial: "reader-a", UpdatedAt: time.Now()}
		v, err := bindModule(ctx, s.db, c, reading)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			exec("DELETE FROM module_jobs WHERE module_id=$1", v.ID)
			exec("DELETE FROM modules WHERE id=$1", v.ID)
		}()
		m.values[v.ID], m.seen[c.Key] = moduleSample{c, reading}, c
		for i := 0; i < 2; i++ {
			target := cardB
			if i == 1 {
				target, reading.ReaderSerial = cardA, "reader-b"
			}
			reading.ICCID = target
			reading.ESIM = &hardware.ESIMInfo{EID: eid, Profiles: []hardware.ESIMProfile{{ICCID: target, Enabled: true}}}
			j := moduleJob{ID: fmt.Sprintf("esim-reader-%d", i), Module: v.ID, Action: "enable", State: "succeeded", Stage: "done"}
			exec("INSERT INTO module_jobs(id,module_id,action,state,stage) VALUES($1,$2,'enable','running','verifying')", j.ID, v.ID)
			m.jobs[v.ID] = moduleJob{ID: j.ID, Module: v.ID, State: "running"}
			m.finishESIMJob(j, hardware.ESIMRequest{Candidate: c, EID: eid, ICCID: target, Action: "enable"}, reading)
			var card string
			if err := s.db.QueryRow(ctx, "SELECT active_card FROM modules WHERE id=$1", v.ID).Scan(&card); err != nil {
				t.Fatal(err)
			}
			if card != cardB || (i == 0 && m.jobs[v.ID].State != "succeeded") || (i == 1 && m.jobs[v.ID].Issue != "DEVICE_CHANGED") {
				t.Fatal("reader identity was not preserved without an IMEI")
			}
		}
	})
}
