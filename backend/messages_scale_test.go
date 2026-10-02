package main

import (
	"context"
	"fmt"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/vocat/vowifi"
	"sync"
	"testing"
	"time"
)

type scaleTransport struct {
	mu              sync.Mutex
	calls           map[string]int
	active, maximum int
	slow            string
	release         chan struct{}
}

func (*scaleTransport) WiFi(context.Context, hardware.Candidate, string, string, func(string)) error {
	return nil
}
func (f *scaleTransport) SendSMS(ctx context.Context, c hardware.Candidate, card, id, to, body string) (vowifi.SMSSubmitResult, error) {
	f.mu.Lock()
	f.calls[id]++
	f.active++
	f.maximum = max(f.maximum, f.active)
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if id == f.slow {
		select {
		case <-ctx.Done():
			return vowifi.SMSSubmitResult{PartsAttempted: 1}, ctx.Err()
		case <-f.release:
		}
	}
	select {
	case <-ctx.Done():
		return vowifi.SMSSubmitResult{PartsAttempted: 1}, ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	return vowifi.SMSSubmitResult{PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true}, nil
}

// Synthetic transport exercises real persistence/scheduling, never real SMS recipients.
func testMessageScaleDatabase(t *testing.T, s *server) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &scaleTransport{calls: map[string]int{}, slow: "scale-message-0001", release: make(chan struct{})}
	m := newModuleManagerWithWiFi(s.db, nil, f)
	m.ctx = ctx
	m.ready = true
	var moduleIDs []int64
	defer func() {
		s.db.Exec(context.Background(), "DELETE FROM messages WHERE request_hash='scale-fixture'")
		s.db.Exec(context.Background(), "DELETE FROM modules WHERE id=ANY($1)", moduleIDs)
	}()
	for i := 1; i <= 120; i++ {
		sample := wifiModuleFixture()
		sample.Candidate.Key = fmt.Sprintf("scale-module-%04d", i)
		sample.Reading.IMEI = fmt.Sprintf("99000000888%04d", i)
		sample.Reading.ICCID = fmt.Sprintf("890000000000088%04d", i)
		v, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading)
		if err != nil {
			t.Fatal(err)
		}
		moduleIDs = append(moduleIDs, v.ID)
		m.values[v.ID], m.seen[sample.Candidate.Key] = sample, sample.Candidate
		m.wifi[v.ID] = &moduleWiFi{Enabled: true, Registered: true, SMSReady: true, running: true, ICCID: sample.Reading.ICCID}
		_, err = s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,request_hash) VALUES($1,$2,$3,$4,'+12025550123',true,'sms','fixture','queued','scale-fixture')`, fmt.Sprintf("scale-message-%04d", i), v.ID, sample.Reading.ICCID, wifiLine(sample.Reading))
		if err != nil {
			t.Fatal(err)
		}
	}
	m.lastScan = time.Now()
	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); m.runMessages(ctx) }()
	defer func() { cancel(); <-done }()
	accepted := 0
	for time.Since(start) < 15*time.Second {
		if err := s.db.QueryRow(ctx, "SELECT count(*) FROM messages WHERE request_hash='scale-fixture' AND state='accepted'").Scan(&accepted); err != nil {
			t.Fatal(err)
		}
		if accepted == 119 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accepted != 119 {
		t.Fatal("one slow module stalled the fleet", accepted)
	}
	f.mu.Lock()
	peak := f.maximum
	for id, count := range f.calls {
		if count != 1 {
			t.Error("network operation replayed", id, count)
		}
	}
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 120 || peak > messageSendConcurrency {
		t.Fatal("concurrency bounds", calls, peak)
	}
	t.Logf("synthetic modules=120 fast accepted=%d slow=1 peak=%d elapsed=%s", accepted, peak, time.Since(start))
	close(f.release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.db.QueryRow(ctx, "SELECT count(*) FROM messages WHERE request_hash='scale-fixture' AND state='accepted'").Scan(&accepted)
		if accepted == 120 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accepted != 120 {
		t.Fatal("slow module did not recover", accepted)
	}
}
