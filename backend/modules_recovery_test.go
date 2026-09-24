package main

import (
	"context"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func recoveryCandidate() hardware.Candidate {
	return hardware.Candidate{Key: "usb:2-4", Kind: "usb", Vendor: "2c7c", Product: "0125", Model: "EC20-CE", Control: "/dev/cdc-wdm3", Generation: "2:13"}
}

func TestModuleRecoveryRequiresSustainedTimeout(t *testing.T) {
	c, r := recoveryCandidate(), hardware.Reading{Issue: "READ_TIMEOUT"}
	s := recoveryStreak{}
	now := time.Now()
	for i, seconds := range []int{0, 30, 60, 90} {
		if got := s.observe(c, r, now.Add(time.Duration(seconds)*time.Second)); got != (i == 3) {
			t.Fatal(i, got)
		}
	}
	for _, change := range []string{"generation", "healthy", "gap", "unsupported"} {
		streak, candidate, reading, at := s, c, r, now.Add(100*time.Second)
		switch change {
		case "generation":
			candidate.Generation = "2:14"
		case "healthy":
			reading = hardware.Reading{Responsive: true, SIM: "absent"}
		case "gap":
			at = now.Add(4 * time.Minute)
		case "unsupported":
			candidate.Kind = "reader"
		}
		if streak.observe(candidate, reading, at) {
			t.Fatal("stale fault carried across", change)
		}
	}
}

type recoverySource struct{ reads, resets int }

func (s *recoverySource) Discover(context.Context) ([]hardware.Candidate, error) { return nil, nil }
func (s *recoverySource) Read(context.Context, hardware.Candidate) hardware.Reading {
	s.reads++
	return hardware.Reading{Issue: "READ_TIMEOUT"}
}
func (s *recoverySource) Restart(context.Context, hardware.Candidate) error { s.resets++; return nil }

func TestModuleRecoveryDoesNotInterruptOperationsOrHotplug(t *testing.T) {
	for _, reason := range []string{"job", "gate", "grace", "changed", "stale", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			source := &recoverySource{}
			m := newModuleManager(nil, source)
			c := recoveryCandidate()
			m.ready, m.recoveryReady = true, true
			m.seen[c.Key], m.lastScan = c, time.Now()
			m.values[1] = moduleSample{Candidate: c}
			m.recoveryProof[c.Key] = moduleSample{Candidate: c, Reading: hardware.Reading{IMEI: "123456789012345"}}
			m.recovery[c.Key] = recoveryStreak{generation: c.Generation, since: time.Now().Add(-2 * time.Minute), last: time.Now(), count: 4}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch reason {
			case "job":
				m.jobs[1] = moduleJob{State: "running"}
			case "gate":
				m.gate(c.Key) <- struct{}{}
			case "grace":
				m.recoveryUntil[c.Key] = time.Now().Add(time.Minute)
			case "changed":
				changed := c
				changed.Generation = "2:14"
				m.seen[c.Key] = changed
			case "stale":
				m.lastScan = time.Now().Add(-time.Minute)
			case "cancelled":
				cancel()
			}
			m.read(ctx, c)
			if source.resets != 0 {
				t.Fatal("unexpected reset")
			}
			if (reason == "job" || reason == "gate" || reason == "grace") && source.reads != 0 {
				t.Fatal("read during operation")
			}
		})
	}
}

func testRecoveryDatabase(t *testing.T, s *server, module int64) {
	t.Helper()
	ctx := context.Background()
	m := newModuleManager(s.db, nil)
	c := hardware.Candidate{Key: "usb:moved"}
	reserve := func(want bool) int64 {
		t.Helper()
		id, target, err := m.reserveRecovery(ctx, c, "imei:"+hardware.Digest("123456789012300"))
		if (err == nil) != (want) || (want && (id == 0 || target != module)) {
			t.Fatalf("reserve: %d %d %v", id, target, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	first := reserve(true)
	reserve(false)
	// Limits survive replacing the manager and moving the last attempt outside global grace.
	m = newModuleManager(s.db, nil)
	exec("UPDATE module_recoveries SET result='requested',attempted_at=now()-interval '5 minutes' WHERE id=$1", first)
	reserve(false)
	exec("UPDATE module_recoveries SET attempted_at=now()-interval '11 minutes' WHERE id=$1", first)
	second := reserve(true)
	exec("UPDATE module_recoveries SET result='requested',attempted_at=now()-interval '11 minutes' WHERE id=$1", second)
	reserve(false)
	exec("UPDATE module_recoveries SET attempted_at=now()-interval '61 minutes'")
	exec("UPDATE module_recoveries SET result='unconfirmed' WHERE id=$1", second)
	reserve(false)
	m.loadRecovery(ctx)
	m.verifyRecovery(ctx, module, hardware.Reading{Responsive: true, IMEI: "123456789012300"})
	c.Generation = "other"
	reserve(false)
	c.Generation = ""
	third := reserve(true)
	exec("UPDATE module_recoveries SET result='requested',attempted_at=now()-interval '11 minutes' WHERE id=$1", third)
	exec("INSERT INTO module_jobs(id,module_id,action,state) VALUES('recovery-busy-test',$1,'download','running')", module)
	reserve(false)
	exec("DELETE FROM module_jobs WHERE id='recovery-busy-test'")
	exec("DELETE FROM module_recoveries")
}
