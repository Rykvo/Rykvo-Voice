package main

import (
	"context"
	"fmt"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"rykvo.local/auth/internal/sipregistrar"
	"testing"
	"time"

	"rykvo.local/auth/internal/telephony"
)

func testSIPSnapshotDatabase(t *testing.T, s *server) {
	ctx := context.Background()
	other := &server{db: s.db}
	g := newSIPGateway(other)
	other.sipGateway = g
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "LOCK TABLE sip_accounts IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	callCtx, stopCall := context.WithCancel(ctx)
	defer stopCall()
	g.calls.active[telephony.ID(9001)] = &sipOutgoing{cancel: stopCall, ctx: callCtx}
	timeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { g.refresh(timeout); close(done) }()
	var waiting bool
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if err = s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM sip_accounts a LEFT JOIN%')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		t.Fatal("snapshot did not reach database")
	}
	if !other.sipAccountsMu.TryLock() {
		t.Fatal("slow snapshot blocked SIP dialog mutex")
	}
	other.sipAccountsMu.Unlock()
	cancel()
	<-done
	if callCtx.Err() != nil || g.ready {
		t.Fatal("read failure ended an existing call or allowed new calls")
	}
	tx.Rollback(ctx)
	// A stale refresh can never restore credentials revoked by a later mutation.
	reached, release := make(chan struct{}), make(chan struct{})
	other.sipNetwork = &sipNetworkManager{call: func(context.Context, map[string]string) (sipNetworkStatus, error) {
		close(reached)
		<-release
		return sipNetworkStatus{State: "disconnected"}, nil
	}}
	done = make(chan struct{})
	go func() { g.refresh(ctx); close(done) }()
	<-reached
	other.sipAccountsMu.Lock()
	g.epoch++
	g.ready = false
	other.sipAccountsMu.Unlock()
	close(release)
	<-done
	if g.ready || len(g.listeners) != 0 {
		t.Fatal("stale snapshot overwrote a newer mutation")
	}
}

func testSIPAdmissionDatabase(t *testing.T, s *server) {
	ctx := context.Background()
	other := &server{db: s.db, modules: newModuleManager(s.db, nil)}
	g := newSIPGateway(other)
	legacy, modern := sipDigests("fixture", "fixture-password")
	a := sipregistrar.Account{ID: "admission-fixture", Username: "fixture", Port: 25060, Revision: 1, MD5: legacy, SHA256: modern}
	g.accounts = []sipregistrar.Account{a}
	g.ready, g.checkedAt = true, time.Now()
	g.registrar.Replace(g.accounts)
	request := func(method sip.RequestMethod, seq int) *sip.Request {
		raw := fmt.Sprintf("%s sip:123456@127.0.0.1:25060 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:35060;branch=z9hG4bK-admission-%d\r\nFrom: <sip:fixture@localhost>;tag=fixture\r\nTo: <sip:fixture@localhost>\r\nCall-ID: admission-fixture\r\nCSeq: %d %s\r\nContact: <sip:fixture@127.0.0.1:35060>;expires=300\r\nContent-Length: 0\r\n\r\n", method, seq, seq, method)
		message, err := sip.ParseMessage([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		r := message.(*sip.Request)
		r.SetSource("127.0.0.1:35060")
		r.SetDestination("127.0.0.1:25060")
		r.SetTransport("UDP")
		return r
	}
	authenticate := func(req *sip.Request, res *sip.Response) {
		if res == nil || res.StatusCode != 401 {
			t.Fatal("challenge missing", res)
		}
		challenge, err := digest.ParseChallenge(res.GetHeaders("WWW-Authenticate")[0].Value())
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := digest.Digest(challenge, digest.Options{Method: string(req.Method), URI: req.Recipient.String(), Username: a.Username, Password: "fixture-password", Count: 1, Cnonce: "admission"})
		if err != nil {
			t.Fatal(err)
		}
		req.AppendHeader(sip.NewHeader("Authorization", credentials.String()))
	}
	reg := request(sip.REGISTER, 1)
	authenticate(reg, g.registrar.Handle(reg, a.Port))
	if res := g.registrar.Handle(reg, a.Port); res.StatusCode != 200 {
		t.Fatal(res)
	}
	req := request(sip.INVITE, 2)
	_, _, challenge := g.registrar.AuthenticateInvite(req, a.Port)
	authenticate(req, challenge)
	g.calls.open = func(context.Context, sipVoiceSample) (moduleVoice, error) {
		t.Error("revoked admission opened hardware")
		return nil, fmt.Errorf("fixture")
	}
	lock, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err = lock.Exec(ctx, "LOCK TABLE sip_call_records IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	tx := &voiceTestTX{responses: make(chan *sip.Response, 10)}
	done := make(chan struct{})
	go func() {
		other.sipAccountsMu.Lock()
		g.calls.inviteLocked(req, tx, a.Port, "127.0.0.1:25060", nil)
		other.sipAccountsMu.Unlock()
		close(done)
	}()
	var waiting bool
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		err = s.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO sip_call_records%')").Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		t.Fatal("call did not reach blocked persistence")
	}
	if !other.sipAccountsMu.TryLock() {
		t.Fatal("slow INVITE persistence blocked other dialogs")
	}
	if !g.calls.admitting[a.ID] {
		t.Error("admission not reserved")
	}
	g.applyAccountLocked(a.ID, nil, nil)
	other.sipAccountsMu.Unlock()
	lock.Rollback(ctx)
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("admission did not finish")
	}
	g.calls.wait.Wait()
	if len(g.calls.admitting) != 0 {
		t.Fatal("admission leaked")
	}
	select {
	case response := <-tx.responses:
		if response.StatusCode != 503 {
			t.Fatal(response.StatusCode)
		}
	default:
		t.Fatal("no final response")
	}
	var outcome string
	if err = s.db.QueryRow(ctx, "SELECT outcome FROM sip_call_records WHERE account_id=$1 ORDER BY started_at DESC LIMIT 1", a.ID).Scan(&outcome); err != nil || outcome != "account_revoked" {
		t.Fatal(outcome, err)
	}
}
