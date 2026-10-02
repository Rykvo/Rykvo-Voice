package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"rykvo.local/auth/internal/telephony"
)

type earlyTestVoice struct {
	voiceTestDevice
	state   atomic.Int32
	wake    chan struct{}
	drained chan struct{}
}

func (v *earlyTestVoice) State(context.Context) (string, error) {
	if v.state.Load() == 1 {
		return "active", nil
	}
	return "early_media", nil
}
func (v *earlyTestVoice) Hangup(ctx context.Context) error {
	close(v.drained)
	return v.voiceTestDevice.Hangup(ctx)
}

func (v *earlyTestVoice) StateChanges() <-chan struct{} { return v.wake }
func (v *earlyTestVoice) ReadPCM(ctx context.Context) ([]int16, error) {
	p, err := v.voiceTestDevice.ReadPCM(ctx)
	if err != nil {
		<-v.drained
	}
	for i := range p {
		p[i] = 2000
	}
	return p, err
}

func TestSIPGatewayEarlyMediaDoesNotAnswerOrOpenMicrophone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := newSIPGateway(&server{modules: newModuleManager(nil, nil)})
	c := g.calls
	c.ctx = ctx
	d := &earlyTestVoice{wake: make(chan struct{}, 1), drained: make(chan struct{})}
	d.wake <- struct{}{}
	c.open = func(context.Context, sipVoiceSample) (moduleVoice, error) { return d, nil }
	c.router.PutPolicy(telephony.Policy{Account: "fixture", Revision: 1, All: true})
	c.router.SetModuleReady("module-16", true)
	reg, _, _ := c.router.Register("fixture", 1, time.Minute)
	call, _, _ := c.router.Dial(reg)
	phone, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	body := fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 0\r\n", phone.LocalAddr().(*net.UDPAddr).Port)
	wire := fmt.Sprintf("INVITE sip:123@localhost SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:35060;branch=z9hG4bK-early\r\nFrom: <sip:user@localhost>;tag=phone\r\nTo: <sip:123@localhost>;tag=server\r\nCall-ID: early-fixture\r\nCSeq: 1 INVITE\r\nContact: <sip:user@127.0.0.1:35060>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	msg, err := sip.ParseMessage([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	req := msg.(*sip.Request)
	req.SetSource("127.0.0.1:35060")
	req.SetTransport("UDP")
	tx := &voiceTestTX{responses: make(chan *sip.Response, 8)}
	legCtx, stop := context.WithCancel(ctx)
	leg := &sipOutgoing{owner: c, call: call, reg: reg, request: req, tx: tx, ctx: legCtx, cancel: stop, ack: make(chan struct{})}
	c.active[call.ID] = leg
	done := make(chan struct{})
	go func() {
		defer close(done)
		leg.run(sipVoiceSample{wifi: true}, sipAccountNetwork{Start: 20000, End: 30000}, net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1))
	}()
	defer func() {
		leg.peerClosed.Store(true)
		leg.ackOnce.Do(func() { close(leg.ack) })
		stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("call worker leaked")
		}
	}()
	read := func() *sip.Response {
		t.Helper()
		select {
		case r := <-tx.responses:
			return r
		case <-time.After(350 * time.Millisecond):
			t.Fatal("state change waited for polling")
			return nil
		}
	}
	progress := read()
	if progress.StatusCode != 183 || len(progress.Body()) == 0 || leg.accepted.Load() {
		t.Fatal("ringback incorrectly answered call", progress.StatusCode)
	}
	phone.SetReadDeadline(time.Now().Add(time.Second))
	packet := make([]byte, 512)
	n, source, err := phone.ReadFromUDP(packet)
	if err != nil || n != 172 {
		t.Fatal("ringback RTP absent", n, err)
	}
	// Yak may send RTP after 183; drain it without forwarding the microphone.
	for i := 0; i < 5; i++ {
		phone.WriteToUDP(packet[:n], source)
	}
	time.Sleep(100 * time.Millisecond)
	if d.nonzeroWrites.Load() != 0 {
		t.Fatal("microphone sent before answer")
	}
	d.state.Store(1)
	d.wake <- struct{}{}
	answer := read()
	if answer.StatusCode != 200 || !bytes.Equal(progress.Body(), answer.Body()) {
		t.Fatal("early/final SDP differs")
	}
	ack := req.Clone()
	ack.Method, ack.CSeq().MethodName = sip.ACK, sip.ACK
	g.server.sipAccountsMu.Lock()
	c.dialogLocked(ack, nil)
	g.server.sipAccountsMu.Unlock()
	bye := req.Clone()
	bye.Method, bye.CSeq().MethodName, bye.CSeq().SeqNo = sip.BYE, sip.BYE, 2
	g.server.sipAccountsMu.Lock()
	c.dialogLocked(bye, tx)
	g.server.sipAccountsMu.Unlock()
	if read().StatusCode != 200 {
		t.Fatal("hangup rejected")
	}
	select {
	case <-done:
	case <-time.After(350 * time.Millisecond):
		t.Fatal("hangup delayed")
	}
	if d.hangups.Load() != 1 {
		t.Fatal("module hangup not dispatched")
	}
}
