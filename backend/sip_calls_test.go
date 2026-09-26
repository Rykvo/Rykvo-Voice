package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"io"
	"net"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/telephony"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise sipgo's real handler/transaction lifetime, not only a fake Respond.
func TestSIPGatewayCallTransactionSurvivesAnswer(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		for _, sameBranch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-branch-%t", transport, sameBranch), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				g := newSIPGateway(&server{modules: newModuleManager(nil, nil)})
				c := g.calls
				c.ctx = ctx
				device := &voiceTestDevice{}
				c.open = func(context.Context, sipVoiceSample) (moduleVoice, error) { return device, nil }
				c.router.PutPolicy(telephony.Policy{Account: "a", Revision: 1, All: true})
				c.router.SetModuleReady("module-01", true)
				reg, _, _ := c.router.Register("a", 1, time.Minute)
				call, _, _ := c.router.Dial(reg)
				ua, err := sipgo.NewUA()
				if err != nil {
					t.Fatal(err)
				}
				defer ua.Close()
				srv, err := sipgo.NewServer(ua)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				srv.OnInvite(g.callHandler(func(req *sip.Request, tx sip.ServerTransaction) <-chan struct{} {
					client, err := sipgo.NewClient(ua)
					if err != nil {
						t.Error(err)
						return nil
					}
					copy := req.Clone()
					copy.To().Params.Add("tag", "server")
					callCtx, stop := context.WithCancel(ctx)
					leg := &sipOutgoing{owner: c, call: call, reg: reg, request: copy, tx: tx, client: client, ctx: callCtx, cancel: stop, ack: make(chan struct{})}
					c.active[call.ID] = leg
					go func() {
						defer close(done)
						leg.run(sipVoiceSample{}, sipAccountNetwork{Start: 20000, End: 30000}, net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"))
					}()
					return done
				}))
				dialog := func(req *sip.Request, tx sip.ServerTransaction) {
					g.server.sipAccountsMu.Lock()
					defer g.server.sipAccountsMu.Unlock()
					c.dialogLocked(req, tx)
				}
				srv.OnAck(dialog)
				srv.OnBye(dialog)
				var address string
				if transport == "udp" {
					listener, err := net.ListenPacket("udp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					address = listener.LocalAddr().String()
					go srv.ServeUDP(listener)
				} else {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					address = listener.Addr().String()
					go srv.ServeTCP(listener)
				}
				conn, err := net.Dial(transport, address)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(6 * time.Second))
				media, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
				if err != nil {
					t.Fatal(err)
				}
				defer media.Close()
				body := fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=test\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\n", media.LocalAddr().(*net.UDPAddr).Port)
				send := func(method, branch, tag string, seq int, body string) {
					raw := fmt.Sprintf("%s sip:12345@%s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=z9hG4bK-%s;rport\r\nFrom: <sip:1001@localhost>;tag=phone\r\nTo: <sip:12345@localhost>%s\r\nCall-ID: wire-call\r\nCSeq: %d %s\r\nContact: <sip:1001@%s>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", method, address, strings.ToUpper(transport), conn.LocalAddr(), branch, tag, seq, method, conn.LocalAddr(), len(body), body)
					if _, err := io.WriteString(conn, raw); err != nil {
						t.Fatal(err)
					}
				}
				reader := bufio.NewReader(conn)
				read := func() *sip.Response {
					var wire []byte
					if transport == "udp" {
						wire = make([]byte, 8192)
						n, err := conn.Read(wire)
						if err != nil {
							t.Fatal(err)
						}
						wire = wire[:n]
					} else {
						size := 0
						for {
							line, err := reader.ReadString('\n')
							if err != nil {
								t.Fatal(err)
							}
							wire = append(wire, line...)
							fmt.Sscanf(line, "Content-Length: %d", &size)
							if line == "\r\n" {
								break
							}
						}
						body := make([]byte, size)
						if _, err := io.ReadFull(reader, body); err != nil {
							t.Fatal(err)
						}
						wire = append(wire, body...)
					}
					msg, err := sip.ParseMessage(wire)
					if err != nil {
						t.Fatal(err)
					}
					res, ok := msg.(*sip.Response)
					if !ok {
						t.Fatalf("unexpected server request: %T", msg)
					}
					return res
				}
				send("INVITE", "invite", "", 1, body)
				defer func() {
					g.server.sipAccountsMu.Lock()
					for _, leg := range c.active {
						leg.peerClosed.Store(true)
						leg.stop()
					}
					g.server.sipAccountsMu.Unlock()
					cancel()
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("worker leaked")
					}
				}()
				answer := read()
				for answer.StatusCode < 200 {
					answer = read()
				}
				if answer.StatusCode != 200 {
					t.Fatal(answer.StatusCode)
				}
				branch := "ack"
				if sameBranch {
					branch = "invite"
				}
				send("ACK", branch, ";tag=server", 1, "")
				media.SetReadDeadline(time.Now().Add(2 * time.Second))
				packet := make([]byte, 2048)
				for i := 0; i < 15; i++ {
					n, peer, err := media.ReadFromUDP(packet)
					if err != nil || n < 172 {
						t.Fatalf("media ended after answer: %d %v", n, err)
					}
					media.WriteToUDP(packet[:n], peer)
				}
				if device.nonzeroWrites.Load() == 0 || device.hangups.Load() != 0 {
					t.Fatal("audio not bridged")
				}
				send("BYE", "bye", ";tag=server", 2, "")
				if res := read(); res.StatusCode != 200 {
					t.Fatal(res.StatusCode)
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("cleanup timed out")
				}
				if device.hangups.Load() != 1 || c.router.Status("a") != telephony.Online {
					t.Fatal("cleanup incomplete")
				}
			})
		}
	}
}

type voiceTestTX struct{ responses chan *sip.Response }

func (t *voiceTestTX) Respond(r *sip.Response) error    { t.responses <- r; return nil }
func (*voiceTestTX) Terminate()                         {}
func (*voiceTestTX) OnTerminate(sip.FnTxTerminate) bool { return true }
func (*voiceTestTX) Done() <-chan struct{}              { return nil }
func (*voiceTestTX) Err() error                         { return nil }
func (*voiceTestTX) Acks() <-chan *sip.Request          { return nil }
func (*voiceTestTX) OnCancel(sip.FnTxCancel) bool       { return true }

type voiceTestDevice struct {
	dialed, closed, hangups, nonzeroWrites atomic.Int32
	writeSamples                           int
	rejectCode                             int
}

func (v *voiceTestDevice) Dial(context.Context, string) error { v.dialed.Add(1); return nil }
func (v *voiceTestDevice) State(context.Context) (string, error) {
	if v.rejectCode != 0 {
		return "idle", nil
	}
	return "active", nil
}
func (v *voiceTestDevice) SIPCode() int                 { return v.rejectCode }
func (v *voiceTestDevice) Hangup(context.Context) error { v.hangups.Add(1); return nil }
func (v *voiceTestDevice) Close()                       { v.closed.Add(1) }
func (v *voiceTestDevice) WritePCM(p []int16) error {
	if v.writeSamples > 0 && len(p) != v.writeSamples {
		return fmt.Errorf("unexpected block size: %d", len(p))
	}
	for _, sample := range p {
		if sample != 0 {
			v.nonzeroWrites.Add(1)
			break
		}
	}
	return nil
}
func (v *voiceTestDevice) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	if v.rejectCode != 0 {
		return nil, hardware.ErrVoiceEnded
	}
	return make([]int16, 160), nil
}
func TestSIPGatewayCellularDialogAndBidirectionalMedia(t *testing.T) {
	testSIPGatewayMedia(t, false, 0)
}
func TestSIPGatewayWiFiDialogAndBidirectionalMedia(t *testing.T) { testSIPGatewayMedia(t, true, 0) }
func TestSIPGatewayWiFiCarrierCodecRejection(t *testing.T)       { testSIPGatewayMedia(t, true, 488) }
func testSIPGatewayMedia(t *testing.T, wifi bool, reject int) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m := newModuleManager(nil, nil)
	s := &server{modules: m}
	g := newSIPGateway(s)
	c := g.calls
	c.ctx = ctx
	device := &voiceTestDevice{rejectCode: reject}
	if wifi {
		device.writeSamples = 160
		gate := m.gate("")
		gate <- struct{}{}
		defer func() { <-gate }()
	}
	c.open = func(context.Context, sipVoiceSample) (moduleVoice, error) { return device, nil }
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
		leg.run(sipVoiceSample{wifi: wifi}, sipAccountNetwork{Start: 20000, End: 30000}, net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"))
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
	if reject != 0 {
		if answer.StatusCode != reject {
			t.Fatalf("carrier rejection %d became %d", reject, answer.StatusCode)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("failed carrier call leaked")
		}
		if device.hangups.Load() == 0 || device.closed.Load() == 0 {
			t.Fatal("rejected call was not cleaned")
		}
		return
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
	for device.nonzeroWrites.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if device.nonzeroWrites.Load() == 0 {
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
