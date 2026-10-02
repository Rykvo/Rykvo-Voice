package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testAlertRecoveryDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE alert_settings SET sip=2,message=2,module=2")
	exec("UPDATE telegram_settings SET token='test-only',notification_id='-1'")
	var ids [2]int64
	for i := range ids {
		name := fmt.Sprint("alert-recovery-", i)
		if err := s.db.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key,active_card,card_epoch) VALUES($1,$1,'usb',$1,$1,'89123456789012345678',1) RETURNING id`, name).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		exec("DELETE FROM modules WHERE id=ANY($1)", ids[:])
		exec("UPDATE alert_settings SET sip=5,message=5,module=5")
		exec("UPDATE telegram_settings SET token='',notification_id=''")
	}()
	start := time.Now().UTC().Add(-time.Hour)
	observe := func(mid, epoch int64, kind, id string, good bool, at time.Time) {
		t.Helper()
		exec("SELECT alert_observe($1,$2,$3,$4,$5,'fixture',$6)", mid, epoch, kind, id, good, at)
	}
	check := func(mid int64, kind string, failures int, active bool) {
		t.Helper()
		var n int
		var a bool
		if err := s.db.QueryRow(ctx, "SELECT failures,active FROM alert_counters WHERE module_id=$1 AND kind=$2", mid, kind).Scan(&n, &a); err != nil || n != failures || a != active {
			t.Fatalf("module %d %s: got %d/%v, want %d/%v (%v)", mid, kind, n, a, failures, active, err)
		}
	}
	queue := func(mid int64, want int) {
		t.Helper()
		var got int
		if err := s.db.QueryRow(ctx, "SELECT count(*) FROM alert_notifications WHERE module_id=$1 AND state='pending'", mid).Scan(&got); err != nil || got != want {
			t.Fatalf("module %d pending %d != %d (%v)", mid, got, want, err)
		}
	}
	kinds := []string{"sip", "sms", "mms"}
	for i, success := range kinds {
		t.Log("cross-recovery", success)
		base := start.Add(time.Duration(i) * time.Minute)
		for _, mid := range ids {
			for _, kind := range append([]string{"module"}, kinds...) {
				for n := 0; n < 2; n++ {
					observe(mid, 1, kind, fmt.Sprintf("recover-%d-%d-%s-%d", i, mid, kind, n), false, base.Add(time.Duration(n)*time.Second))
				}
			}
		}
		id := "recover-success-" + success
		observe(ids[0], 1, success, id, true, base.Add(3*time.Second))
		for _, kind := range kinds {
			check(ids[0], kind, 0, false)
		}
		queue(ids[0], 1) // A business success does not hide an unrelated hardware fault.
		queue(ids[1], 4)
		observe(ids[0], 1, "sms", id+"-new-failure", false, base.Add(4*time.Second))
		observe(ids[0], 1, success, id, true, base.Add(5*time.Second))
		observe(ids[0], 1, success, id+"-late", true, base.Add(2*time.Second))
		check(ids[0], "sms", 1, false)
	}
	base := start.Add(10 * time.Minute)
	for _, kind := range kinds {
		observe(ids[0], 1, kind, "disabled-source-fail-"+kind, false, base)
	}
	exec("UPDATE alert_settings SET sip=0")
	observe(ids[0], 1, "sip", "disabled-source-success", true, base.Add(time.Second))
	for _, kind := range kinds {
		check(ids[0], kind, 0, false)
	}
	exec("UPDATE alert_settings SET sip=2")
	// A→B→A is a new activation, even when the original SIM returns.
	exec("UPDATE modules SET active_card='89123456789012345679' WHERE id=$1", ids[0])
	exec("UPDATE modules SET active_card='89123456789012345678' WHERE id=$1", ids[0])
	for _, kind := range kinds {
		observe(ids[0], 3, kind, "new-card-fail-"+kind, false, base.Add(3*time.Second))
	}
	observe(ids[0], 1, "mms", "old-card-success", true, base.Add(4*time.Second))
	for _, kind := range kinds {
		check(ids[0], kind, 1, false)
	}
	observe(ids[0], 3, "mms", "new-card-success", true, base.Add(5*time.Second))
	// Out-of-order concurrent outcomes use operation time, not arrival order.
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			good := i%2 == 0
			kind, id, at := "sms", "concurrent-new-failure", base.Add(7*time.Second)
			if good {
				kind, id, at = "sip", "concurrent-old-success", base.Add(6*time.Second)
			}
			_, err := s.db.Exec(ctx, "SELECT alert_observe($1,3,$2,$3,$4,'fixture',$5)", ids[0], kind, id, good, at)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	check(ids[0], "sip", 0, false)
	check(ids[0], "mms", 0, false)
	check(ids[0], "sms", 1, false)
	exec(alertSchema)
	check(ids[0], "sms", 1, false)
	queue(ids[0], 1)
	queue(ids[1], 3) // Disabling SIP was global; SMS, MMS and hardware alarms remain.
}
