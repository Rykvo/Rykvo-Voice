package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"rykvo.local/auth/internal/sipregistrar"
	"rykvo.local/auth/internal/telephony"
)

func registerMutationFixture(t *testing.T, g *sipGateway, a sipregistrar.Account) {
	t.Helper()
	registerBindingFixture(t, g, a, 1, 300)
}

func registerBindingFixture(t *testing.T, g *sipGateway, a sipregistrar.Account, sequence, expires int) {
	t.Helper()
	raw := fmt.Sprintf("REGISTER sip:127.0.0.1:%d SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:35060;branch=z9hG4bK-%s-%d\r\nFrom: <sip:%s@localhost>;tag=fixture\r\nTo: <sip:%s@localhost>\r\nCall-ID: %s\r\nCSeq: %d REGISTER\r\nContact: <sip:%s@127.0.0.1:35060>;expires=%d\r\nContent-Length: 0\r\n\r\n", a.Port, a.ID, sequence, a.Username, a.Username, a.ID, sequence, a.Username, expires)
	message, err := sip.ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	req := message.(*sip.Request)
	req.SetSource("127.0.0.1:35060")
	req.SetDestination(fmt.Sprintf("127.0.0.1:%d", a.Port))
	req.SetTransport("UDP")
	response := g.registrar.Handle(req, a.Port)
	if response.StatusCode != 401 {
		t.Fatal(response.StatusCode)
	}
	challenge, err := digest.ParseChallenge(response.GetHeaders("WWW-Authenticate")[0].Value())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := digest.Digest(challenge, digest.Options{Method: "REGISTER", URI: req.Recipient.String(), Username: a.Username, Password: "fixture-secret", Count: 1, Cnonce: "mutation"})
	if err != nil {
		t.Fatal(err)
	}
	req.AppendHeader(sip.NewHeader("Authorization", credential.String()))
	if response = g.registrar.Handle(req, a.Port); response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	g.calls.syncLocked(g.accounts, g.policies)
}

func TestSIPAccountMutationIsolation(t *testing.T) {
	for _, mode := range []string{"credentials", "permission", "delete", "suspend"} {
		t.Run(mode, func(t *testing.T) {
			g := newSIPGateway(&server{})
			accounts := make([]sipregistrar.Account, 2)
			policies := make([]telephony.Policy, 2)
			calls := make([]*sipOutgoing, 2)
			for i := range accounts {
				a := sipregistrar.Account{ID: fmt.Sprint("fixture-", i), Username: fmt.Sprint("user-", i), Port: 25061 + i, Revision: 1}
				a.MD5, a.SHA256 = sipDigests(a.Username, "fixture-secret")
				p := telephony.Policy{Account: a.ID, Revision: 1, Receive: true, Modules: []string{moduleID(int64(i + 1))}}
				accounts[i], policies[i] = a, p
				g.applyAccountLocked(a.ID, &a, &p)
				registerMutationFixture(t, g, a)
				g.calls.router.SetModuleReady(p.Modules[0], true)
			}
			for i, a := range accounts {
				var reg telephony.Registration
				for _, r := range g.calls.registrations {
					if r.Account == a.ID {
						reg = r
					}
				}
				c, _, err := g.calls.router.Dial(reg)
				if err != nil {
					t.Fatal(err)
				}
				if err = g.calls.router.Connected(c.ID); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				call := &sipOutgoing{owner: g.calls, call: c, reg: reg, ctx: ctx, cancel: cancel}
				calls[i] = call
				g.calls.active[c.ID] = call
			}
			// Disabling incoming offers does not terminate an established conversation.
			a, p := accounts[0], policies[0]
			p.Receive = false
			g.applyAccountLocked(a.ID, &a, &p)
			if calls[0].ctx.Err() != nil || calls[1].ctx.Err() != nil || !g.registrar.Online(a.ID) {
				t.Fatal("receive-only edit interrupted established calls")
			}
			// Audio-only edits preserve active calls and registered endpoints.
			incoming := &sipIncoming{owner: g.calls, ctx: context.Background()}
			before := newIncomingLeg(incoming, calls[0].reg, sipregistrar.Registration{}, nil, sipAccountNetwork{}, nil, nil, 0, "")
			defer before.cancel()
			a.LowBandwidthAudio = true
			g.applyAccountLocked(a.ID, &a, &p)
			after := newIncomingLeg(incoming, calls[0].reg, sipregistrar.Registration{}, nil, sipAccountNetwork{}, nil, nil, 0, "")
			defer after.cancel()
			if before.lowBandwidthAudio || !after.lowBandwidthAudio || g.lowBandwidthAudioLocked(accounts[1].ID) || g.lowBandwidthAudioLocked("missing") || calls[0].lowBandwidthAudio || calls[0].ctx.Err() != nil || calls[1].ctx.Err() != nil || !g.registrar.Online(a.ID) {
				t.Fatal("audio preference escaped its account or changed an existing call")
			}
			a.LowBandwidthAudio = false
			g.applyAccountLocked(a.ID, &a, &p)
			if !after.lowBandwidthAudio || g.lowBandwidthAudioLocked(a.ID) || calls[0].ctx.Err() != nil || !g.registrar.Online(a.ID) {
				t.Fatal("audio preference was not snapshotted")
			}
			epoch := g.epoch
			switch mode {
			case "credentials":
				a.Revision++
				p.Revision++
				g.applyAccountLocked(a.ID, &a, &p)
			case "permission":
				p.Modules = []string{"module-03"}
				g.applyAccountLocked(a.ID, &a, &p)
			case "delete":
				g.applyAccountLocked(a.ID, nil, nil)
			case "suspend":
				g.suspendAccountLocked(a.ID)
			}
			if g.epoch <= epoch || calls[0].ctx.Err() == nil || calls[1].ctx.Err() != nil || !g.registrar.Online(accounts[1].ID) {
				t.Fatal("mutation failed to isolate affected call")
			}
			if mode != "permission" && g.registrar.Online(a.ID) {
				t.Fatal("revoked registration remained online")
			}
			if mode == "suspend" {
				// An authoritative unchanged DB revision is recoverable, but old grants stay revoked.
				g.applyAccountLocked(a.ID, &a, &p)
				if g.registrar.Online(a.ID) {
					t.Fatal("suspended credentials restored without authentication")
				}
				registerMutationFixture(t, g, a)
				if !g.registrar.Online(a.ID) || !g.calls.policies[a.ID] {
					t.Fatal("confirmed unchanged policy did not recover")
				}
			}
		})
	}
}

func testSIPAccountMutationDatabase(t *testing.T, s *server) {
	ctx := context.Background()
	other := &server{db: s.db}
	g := newSIPGateway(other)
	other.sipGateway = g
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	reserve.Close()
	a := sipregistrar.Account{ID: token(), Username: "mutation-fixture", Port: port, Revision: 1}
	a.MD5, a.SHA256 = sipDigests(a.Username, "fixture-secret")
	if _, err = s.db.Exec(ctx, `INSERT INTO sip_accounts(id,username,port,digest_md5,digest_sha256,allocation,receive_calls) VALUES($1,$2,$3,$4,$5,'all',true)`, a.ID, a.Username, a.Port, a.MD5, a.SHA256); err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(ctx, "DELETE FROM sip_accounts WHERE id=$1", a.ID)
	p := telephony.Policy{Account: a.ID, Revision: 1, All: true, Receive: true}
	g.applyAccountLocked(a.ID, &a, &p)
	registerMutationFixture(t, g, a)
	refreshReached, releaseRefresh := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	other.sipNetwork = &sipNetworkManager{call: func(ctx context.Context, _ map[string]string) (sipNetworkStatus, error) {
		if reads.Add(1) == 2 {
			close(refreshReached)
			select {
			case <-releaseRefresh:
			case <-ctx.Done():
				return sipNetworkStatus{}, ctx.Err()
			}
		}
		return sipNetworkStatus{State: "disconnected"}, nil
	}}
	lock, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err = lock.Exec(ctx, "LOCK TABLE sip_accounts IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(sipAccountInput{Username: a.Username, Password: "new-fixture-secret", Port: a.Port, Allocation: "all", ReceiveCalls: true, Revision: 1})
	request := httptest.NewRequest("PATCH", "/api/sip/accounts/"+a.ID, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { other.sipAccountsAPI(requestCtx, recorder, request); close(done) }()
	defer func() {
		cancel()
		lock.Rollback(ctx)
		<-done
		for _, l := range g.listeners {
			l.close()
		}
	}()
	var waiting bool
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT username,port,revision,credential_revision%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		select {
		case <-done:
			t.Fatalf("mutation returned before SQL: %d %s", recorder.Code, recorder.Body.String())
		default:
		}
		t.Fatal("mutation did not reach blocked SQL")
	}
	if !other.sipAccountsMu.TryLock() {
		t.Fatal("account SQL blocked unrelated SIP dialogs")
	}
	other.sipAccountsMu.Unlock()
	lock.Rollback(ctx)
	select {
	case <-refreshReached:
	case <-time.After(3 * time.Second):
		t.Fatal("post-commit refresh missing")
	}
	if !other.sipAccountsMu.TryLock() {
		t.Fatal("post-commit refresh blocked SIP dialogs")
	}
	revision := g.accounts[0].Revision
	online := g.registrar.Online(a.ID)
	other.sipAccountsMu.Unlock()
	if revision != 2 || online {
		t.Fatal("credentials not revoked before slow refresh", revision, online)
	}
	close(releaseRefresh)
	<-done
	if recorder.Code != 200 {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var stored int64
	if err = s.db.QueryRow(ctx, "SELECT credential_revision FROM sip_accounts WHERE id=$1", a.ID).Scan(&stored); err != nil || stored != revision {
		t.Fatal(stored, err)
	}
}
