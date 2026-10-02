package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"rykvo.local/auth/internal/durable"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestCellularInboxDiagnosticsDeduplicatedAndPrivate(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	r := &messageRuntime{}
	r.inbox(8, nil)
	r.inbox(8, errors.New("INVALID_MESSAGE"))
	r.inbox(8, errors.New("INVALID_MESSAGE"))
	r.inbox(8, errors.New("private SMS content and subscriber"))
	r.inbox(8, nil)
	r.inbox(8, nil)
	if bytes.Count(output.Bytes(), []byte("message inbox:")) != 3 || bytes.Contains(output.Bytes(), []byte("private")) || len(r.inboxIssues) != 0 {
		t.Fatal("inbox diagnostics leaked data or repeated")
	}
}

func TestMessageGateWaitsForPollingWithoutBlockingOtherModules(t *testing.T) {
	m := newModuleManager(nil, nil)
	busy := m.gate("busy")
	busy <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan func(), 1)
	go func() { done <- m.waitMessageGate(ctx, "busy") }()
	if release := m.waitMessageGate(ctx, "other"); release == nil {
		t.Fatal("one module blocked another")
	} else {
		release()
	}
	select {
	case release := <-done:
		if release != nil {
			release()
		}
		t.Fatal("busy polling was not queued")
	case <-time.After(20 * time.Millisecond):
	}
	<-busy
	select {
	case release := <-done:
		if release == nil {
			t.Fatal("missed the polling release")
		}
		if len(busy) != 1 {
			t.Fatal("message did not own the gate")
		}
		release()
	case <-ctx.Done():
		t.Fatal("polling release did not wake the message")
	}
	busy <- struct{}{}
	wait, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if release := m.waitMessageGate(wait, "busy"); release != nil {
		release()
		t.Fatal("cancelled wait acquired busy module")
	}
	if len(busy) != 1 {
		t.Fatal("cancelled waiter released another owner")
	}
	<-busy
	if release := m.waitMessageGate(wait, "busy"); release != nil {
		release()
		t.Fatal("expired wait acquired idle module")
	}
	if len(busy) != 0 {
		t.Fatal("cancelled waiter leaked its reservation")
	}
}

type scheduledSMS struct {
	started chan string
	release chan struct{}
	blocked string
}

func (*scheduledSMS) WiFi(context.Context, hardware.Candidate, string, string, func(string)) error {
	return nil
}
func (f *scheduledSMS) SendSMS(ctx context.Context, _ hardware.Candidate, _, id, _, _ string) (vowifi.SMSSubmitResult, error) {
	f.started <- id
	if id == f.blocked {
		select {
		case <-f.release:
		case <-ctx.Done():
			return vowifi.SMSSubmitResult{PartsAttempted: 1}, ctx.Err()
		}
	}
	return vowifi.SMSSubmitResult{PartsTotal: 1, PartsAttempted: 1, PartsAccepted: 1, AllPartsAccepted: true}, nil
}

func TestMessageRuntimeIncludesReceiptFailure(t *testing.T) {
	var state messageRuntime
	state.update(messageRuntimeState{Ready: true, CheckedAt: time.Now()})
	state.receipts(errors.New("database unavailable"))
	if got := state.snapshot(); got.Ready || got.Issue != "MESSAGE_RECEIPT_UPDATE_FAILED" {
		t.Fatal(got)
	}
	state.update(messageRuntimeState{Ready: true, CheckedAt: time.Now()})
	if state.snapshot().Ready {
		t.Fatal("scheduler hid receipt error")
	}
	state.receipts(nil)
	if !state.snapshot().Ready {
		t.Fatal("recovery not visible")
	}
}

func testMessageWorkerDatabase(t *testing.T, s *server, original moduleRecord) {
	t.Helper()
	ctx := context.Background()
	sample := wifiModuleFixture()
	sample.Candidate.Key = original.Endpoint
	other := sample
	other.Candidate.Key = "usb:scheduler-fixture"
	other.Reading.IMEI = "990000000009991"
	other.Reading.ICCID = "89000000000000999901"
	v, err := bindModule(ctx, s.db, other.Candidate, other.Reading)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.db.Exec(ctx, "DELETE FROM messages WHERE line_id='scheduler-fixture'")
		s.db.Exec(ctx, "DELETE FROM modules WHERE id=$1", v.ID)
	}()
	f := &scheduledSMS{started: make(chan string, 16), release: make(chan struct{}), blocked: "scheduler-slow-0001"}
	m := newModuleManagerWithWiFi(s.db, nil, f)
	m.ctx = ctx
	m.ready, m.lastScan = true, time.Now()
	for id, reading := range map[int64]moduleSample{original.ID: sample, v.ID: other} {
		m.values[id] = reading
		m.seen[reading.Candidate.Key] = reading.Candidate
		m.wifi[id] = &moduleWiFi{Enabled: true, Registered: true, SMSReady: true, running: true, ICCID: reading.Reading.ICCID}
	}
	// Use the real line identity while retaining an isolated cleanup key in request_hash.
	add := func(id string, module int64, reading moduleSample) {
		t.Helper()
		_, err := s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,request_hash)
 VALUES($1,$2,$3,$4,'+12025559999',true,'sms','fixture','queued','scheduler-fixture')`, id, module, reading.Reading.ICCID, wifiLine(reading.Reading))
		if err != nil {
			t.Fatal(err)
		}
	}
	defer s.db.Exec(ctx, "DELETE FROM messages WHERE request_hash='scheduler-fixture'")
	add(f.blocked, original.ID, sample)
	add("scheduler-same-0002", original.ID, sample)
	add("scheduler-other-0003", v.ID, other)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); m.runMessages(workerCtx) }()
	var release sync.Once
	defer func() { release.Do(func() { close(f.release) }); cancel(); <-done }()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-f.started:
			if id == "scheduler-same-0002" {
				t.Fatal("same module overlapped blocked send")
			}
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatal("slow module blocked another module", seen)
		}
	}
	if !seen[f.blocked] || !seen["scheduler-other-0003"] {
		t.Fatal(seen)
	}
	release.Do(func() { close(f.release) })
	select {
	case id := <-f.started:
		if id != "scheduler-same-0002" {
			t.Fatal("unexpected replay", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("module queue did not resume")
	}
	cancel()
	<-done
	// Deferred join may safely receive the closed channel again.
	if m.messageRuntime.snapshot().Ready {
		t.Fatal("stopped worker reported ready")
	}
	testMessageOutcomeDatabase(t, s, original.ID)
	testMessageInitializationRecovery(t, s)
	testModuleDatabaseDoesNotHoldStateLock(t, s, original)
}

func testModuleDatabaseDoesNotHoldStateLock(t *testing.T, s *server, v moduleRecord) {
	t.Helper()
	ctx := context.Background()
	m := newModuleManager(s.db, nil)
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	m.values[v.ID] = sample
	m.seen[v.Endpoint] = sample.Candidate
	m.lastScan = time.Now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(72859601)"); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); m.accept(ctx, sample) }()
	defer func() { tx.Rollback(ctx); <-done }()
	// The device gate proves acceptance has entered its database phase.
	gate := m.gate(v.Endpoint)
	deadline := time.Now().Add(time.Second)
	for len(gate) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	read := make(chan struct{})
	go func() { m.state(v); close(read) }()
	select {
	case <-read:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("database I/O held the module state lock")
	}
}

func testMessageOutcomeDatabase(t *testing.T, s *server, module int64) {
	t.Helper()
	ctx := context.Background()
	const id = "scheduler-result-0004"
	if _, err := s.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state) VALUES($1,$2,'89000000000000999902','scheduler-fixture','123',true,'sms','sending')`, id, module); err != nil {
		t.Fatal(err)
	}
	m := newModuleManager(s.db, nil)
	m.messageResults.Dir = t.TempDir()
	if err := m.messageResults.Prepare(); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM messages WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	write, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	err = m.messageState(write, id, "accepted", "", map[string]any{"partsTotal": 1, "allPartsAccepted": true})
	cancel()
	if err == nil {
		t.Fatal("locked database appeared to save result")
	}
	tx.Rollback(ctx)
	fresh := newModuleManager(s.db, nil)
	fresh.messageResults.Dir = m.messageResults.Dir
	if err = fresh.prepareMessages(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.db.QueryRow(ctx, "SELECT state FROM messages WHERE id=$1", id).Scan(&state); err != nil || state != "accepted" {
		t.Fatal("durable result lost on restart", state, err)
	}
	if _, err = s.db.Exec(ctx, "UPDATE messages SET state='delivered' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(messageOutcome{ID: id, State: "accepted"})
	_ = fresh.messageResults.Apply(ctx, id, payload, func(context.Context, []byte) error { return errors.New("simulate leftover journal") })
	if err = fresh.messageResults.Replay(ctx, fresh.applyMessageOutcome); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(ctx, "SELECT state FROM messages WHERE id=$1", id).Scan(&state); err != nil || state != "delivered" {
		t.Fatal("old outcome overwrote newer receipt", state, err)
	}
}

func testMessageInitializationRecovery(t *testing.T, s *server) {
	t.Helper()
	m := newModuleManager(s.db, nil)
	dir := filepath.Join(t.TempDir(), "journal")
	if err := os.WriteFile(dir, []byte("temporarily unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	m.messageResults = durable.Journal{Dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.runMessages(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for m.messageRuntime.snapshot().Issue == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.messageRuntime.snapshot().Issue != "MESSAGE_INITIALIZATION_FAILED" {
		t.Fatal("initialization failure not observable")
	}
	select {
	case <-done:
		t.Fatal("worker quit after transient initialization failure")
	default:
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(7 * time.Second)
	for !m.messageRuntime.snapshot().Ready && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.messageRuntime.snapshot().Ready {
		t.Fatal("worker did not recover without restarting web server")
	}
}
