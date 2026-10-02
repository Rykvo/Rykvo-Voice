package main

import (
	"context"
	"errors"
	"testing"

	"github.com/emiago/sipgo/sip"
	"rykvo.local/auth/internal/sipregistrar"
	"rykvo.local/auth/internal/telephony"
)

func TestSIPRegistrationCyclePreservesEstablishedDialog(t *testing.T) {
	g := newSIPGateway(&server{})
	a := sipregistrar.Account{ID: "fixture", Username: "user", Port: 25061, Revision: 1}
	a.MD5, a.SHA256 = sipDigests(a.Username, "fixture-secret")
	p := telephony.Policy{Account: a.ID, Revision: 1, All: true}
	g.applyAccountLocked(a.ID, &a, &p)
	registerMutationFixture(t, g, a)
	var reg telephony.Registration
	for _, r := range g.calls.registrations {
		reg = r
	}
	g.calls.router.SetModuleReady("module-16", true)
	call, _, err := g.calls.router.Dial(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.calls.router.Connected(call.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := sip.ParseMessage([]byte("INVITE sip:123@localhost SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:35060;branch=z9hG4bK-original\r\nFrom: <sip:user@localhost>;tag=phone\r\nTo: <sip:123@localhost>;tag=server\r\nCall-ID: original-dialog\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	req := request.(*sip.Request)
	req.SetSource("127.0.0.1:35060")
	req.SetTransport("UDP")
	leg := &sipOutgoing{owner: g.calls, call: call, reg: reg, request: req, ctx: ctx, cancel: cancel}
	leg.accepted.Store(true)
	g.calls.active[call.ID] = leg
	for i, expires := range []int{300, 0, 300} {
		registerBindingFixture(t, g, a, i+2, expires)
		if ctx.Err() != nil || g.calls.router.Status(a.ID) != telephony.Busy {
			t.Fatalf("REGISTER expires=%d terminated active call", expires)
		}
		if g.registrar.Online(a.ID) != (expires > 0) {
			t.Fatal("dialog kept a stale registration online")
		}
	}
	for _, renewed := range g.calls.registrations {
		if renewed.ID == reg.ID {
			t.Fatal("new registration inherited old dialog identity")
		}
		if _, _, err := g.calls.router.Dial(renewed); !errors.Is(err, telephony.ErrBusy) {
			t.Fatal("registration refresh bypassed call occupancy", err)
		}
	}
	bye := req.Clone()
	bye.Method = sip.BYE
	bye.CSeq().MethodName, bye.CSeq().SeqNo = sip.BYE, 2
	tx := &voiceTestTX{responses: make(chan *sip.Response, 1)}
	bye.From().Params.Add("tag", "forged")
	g.calls.dialogLocked(bye, tx)
	if res := <-tx.responses; res.StatusCode != 481 || ctx.Err() != nil {
		t.Fatal("registration change weakened dialog binding", res.StatusCode)
	}
	bye.From().Params.Add("tag", "phone")
	g.calls.dialogLocked(bye, tx)
	if res := <-tx.responses; res.StatusCode != 200 || ctx.Err() == nil || !leg.peerClosed.Load() {
		t.Fatal("original dialog BYE rejected after registration change", res.StatusCode)
	}
}
