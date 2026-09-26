package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/emiago/sipgo/sip"
	"net"
	"rykvo.local/auth/internal/telephony"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type voiceTestTX struct{ responses chan *sip.Response }

func (t *voiceTestTX) Respond(r *sip.Response) error    { t.responses <- r; return nil }
func (*voiceTestTX) Terminate()                         {}
func (*voiceTestTX) OnTerminate(sip.FnTxTerminate) bool { return true }
func (*voiceTestTX) Done() <-chan struct{}              { return nil }
func (*voiceTestTX) Err() error                         { return nil }
func (*voiceTestTX) Acks() <-chan *sip.Request          { return nil }
func (*voiceTestTX) OnCancel(sip.FnTxCancel) bool       { return true }

type voiceTestDevice struct{ dialed, closed, hangups, writes atomic.Int32 }

func (v *voiceTestDevice) Dial(context.Context, string) error    { v.dialed.Add(1); return nil }
func (v *voiceTestDevice) State(context.Context) (string, error) { return "active", nil }
func (v *voiceTestDevice) Hangup(context.Context) error          { v.hangups.Add(1); return nil }
func (v *voiceTestDevice) Close()                                { v.closed.Add(1) }
func (v *voiceTestDevice) WritePCM(p []int16) error {
	if len(p) > 0 {
		v.writes.Add(1)
	}
	return nil
}
func (v *voiceTestDevice) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	return make([]int16, 160), nil
}
func TestSIPGatewayCellularDialogAndBidirectionalMedia(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m := newModuleManager(nil, nil)
	s := &server{modules: m}
	g := newSIPGateway(s)
	c := g.calls
	c.ctx = ctx
	device := &voiceTestDevice{}
	c.open = func(context.Context, moduleSample) (cellularVoice, error) { return device, nil }
	c.router.PutPolicy(telephony.Policy{Account: "a", Revision: 1, All: true})
	c.router.SetModuleReady("module-01", true)
	reg, _, err := c.router.Register("a", 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call, _, err := c.router.Dial(reg)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	source := udp.LocalAddr().String()
	body := fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=test\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\n", udp.LocalAddr().(*net.UDPAddr).Port)
	raw := fmt.Sprintf("INVITE sip:12345@127.0.0.1:20001 SIP/2.0\r\nVia: SIP/2.0/UDP %s;branch=z9hG4bK-test\r\nFrom: <sip:1001@localhost>;tag=phone\r\nTo: <sip:12345@localhost>;tag=server\r\nCall-ID: voice-test\r\nCSeq: 1 INVITE\r\nContact: <sip:1001@%s>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", source, source, len(body), body)
	msg, err := sip.ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	req := msg.(*sip.Request)
	req.SetSource(source)
	req.SetTransport("UDP")
	tx := &voiceTestTX{make(chan *sip.Response, 16)}
	callCtx, stop := context.WithCancel(ctx)
	leg := &sipOutgoing{owner: c, call: call, reg: reg, request: req, tx: tx, ctx: callCtx, cancel: stop, ack: make(chan struct{})}
	c.active[call.ID] = leg
	done := make(chan struct{})
	go func() {
		defer close(done)
		leg.run(moduleSample{}, sipAccountNetwork{Start: 20000, End: 30000}, net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"))
	}()
	defer func() {
		leg.peerClosed.Store(true)
		stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("call worker leaked")
		}
	}()
	var answer *sip.Response
	select {
	case answer = <-tx.responses:
	case <-ctx.Done():
		t.Fatal("no response")
	}
	if answer.StatusCode != 200 {
		t.Fatal(answer.StatusCode)
	}
	var mediaPort int
	for _, line := range strings.Split(string(answer.Body()), "\r\n") {
		if strings.HasPrefix(line, "m=audio ") {
			fmt.Sscanf(line, "m=audio %d", &mediaPort)
		}
	}
	if mediaPort < 20000 || mediaPort > 30000 {
		t.Fatal(mediaPort)
	}
	ack := req.Clone()
	ack.Method = sip.ACK
	ack.CSeq().MethodName = sip.ACK
	s.sipAccountsMu.Lock()
	c.dialogLocked(ack, tx)
	s.sipAccountsMu.Unlock()
	udp.SetReadDeadline(time.Now().Add(2 * time.Second))
	packet := make([]byte, 2048)
	n, _, err := udp.ReadFromUDP(packet)
	if err != nil || n < 172 {
		t.Fatal("downlink missing", n, err)
	}
	uplink := make([]byte, 172)
	uplink[0] = 0x80
	uplink[1] = 8
	binary.BigEndian.PutUint32(uplink[8:12], 123)
	for i := 12; i < len(uplink); i++ {
		uplink[i] = 0xd5
	}
	udp.WriteToUDP(uplink, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: mediaPort})
	deadline := time.Now().Add(time.Second)
	for device.writes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if device.writes.Load() == 0 {
		t.Fatal("uplink missing")
	}
	if c.router.Status("a") != telephony.Busy {
		t.Fatal("call not reserved")
	}
	bye := req.Clone()
	bye.Method = sip.BYE
	bye.CSeq().MethodName = sip.BYE
	bye.CSeq().SeqNo = 2
	s.sipAccountsMu.Lock()
	c.dialogLocked(bye, tx)
	s.sipAccountsMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("hangup stalled")
	}
	if device.dialed.Load() != 1 || device.hangups.Load() != 1 || device.closed.Load() != 1 || c.router.Status("a") != telephony.Online {
		t.Fatal("cleanup", device, c.router.Status("a"))
	}
}
func TestSIPGatewayDialNumberBounds(t *testing.T) {
	for _, v := range []string{"+8612345678901", "12345", "123"} {
		if !validDialNumber(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"", "+1", "12;ATH", "*123#", "123\r\n", "12345678901234567"} {
		if validDialNumber(v) {
			t.Fatal(v)
		}
	}
}
