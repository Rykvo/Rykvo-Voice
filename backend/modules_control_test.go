package main

import (
	"context"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func testControlWritesDoNotHoldStateLock(t *testing.T, s *server) {
	ctx := context.Background()
	sample := wifiModuleFixture()
	sample.Candidate.Key = "control-lock-fixture"
	sample.Reading.IMEI = "123456789012399"
	sample.Reading.ESIM = &hardware.ESIMInfo{EID: "89000000000000000000000000000099"}
	v := moduleRecord{Endpoint: sample.Candidate.Key, Identity: sample.Candidate.Identity(sample.Reading)}
	if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key) VALUES($1,$2,'modem','Controls','controls') RETURNING id`, v.Identity, v.Endpoint).Scan(&v.ID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"roaming", "wifi", "esim", "restart", "host"} {
		t.Run("control-lock-"+kind, func(t *testing.T) {
			fake := &restartFake{}
			m := newModuleManager(s.db, fake)
			m.ctx = ctx
			m.ready = true
			m.roamingReady = true
			m.lastScan = time.Now()
			m.values[v.ID] = sample
			m.seen[v.Endpoint] = sample.Candidate
			m.wifiEngine = &scheduledSMS{}
			table := "module_wifi"
			if kind == "roaming" {
				table = "card_data_policy_requests"
			}
			if kind == "restart" || kind == "host" {
				table = "module_jobs"
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch kind {
				case "roaming":
					done <- m.setRoaming(call, v, wifiLine(sample.Reading), "control-lock-roaming", true)
				case "wifi":
					done <- m.setWiFi(call, v, wifiLine(sample.Reading), "control-lock-wifi", true)
				case "esim":
					_, e := m.startJob(call, v, hardware.ESIMRequest{Action: "enable", EID: sample.Reading.ESIM.EID, ICCID: sample.Reading.ICCID}, "control-lock-esim")
					done <- e
				case "host":
					_, e := m.restartHost(call, "control-lock-host")
					done <- e
				case "restart":
					_, e := m.restartModules(call, []moduleRecord{v}, moduleID(v.ID), "control-lock-restart")
					done <- e
				}
			}()
			waiting := false
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if err = s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)", "%"+table+"%").Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !waiting {
				cancel()
				t.Fatal("operation did not reach persistence")
			}
			if !m.mu.TryRLock() {
				cancel()
				t.Fatal("slow administrative write held global module state lock")
			}
			m.mu.RUnlock()
			cancel()
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("locked write falsely succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled write leaked")
			}
			if len(m.operationSlots) != 0 || m.job(v.ID).active() || fake.resets.Load() != 0 {
				t.Fatal("failed persistence leaked a reservation or touched hardware")
			}
		})
	}
}
