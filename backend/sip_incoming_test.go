package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"rykvo.local/auth/internal/sipregistrar"
	"rykvo.local/auth/internal/telephony"
)

type incomingTestVoice struct{ state, answers, writes atomic.Int32 }

func (v *incomingTestVoice) Dial(context.Context, string) error {
	return fmt.Errorf("incoming must not dial")
}
func (v *incomingTestVoice) Answer(context.Context) error {
	if !v.state.CompareAndSwap(0, 1) {
		return fmt.Errorf("not ringing")
	}
	v.answers.Add(1)
	return nil
}
func (v *incomingTestVoice) State(context.Context) (string, error) {
	switch v.state.Load() {
	case 0:
		return "ringing", nil
	case 1:
		return "active", nil
	}
	return "idle", nil
}
func (v *incomingTestVoice) Hangup(context.Context) error { v.state.Store(2); return nil }
func (v *incomingTestVoice) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	p := make([]int16, 160)
	for i := range p {
		p[i] = 1000
	}
	return p, nil
}
func (v *incomingTestVoice) WritePCM([]int16) error { v.writes.Add(1); return nil }
func (v *incomingTestVoice) Close()                 {}

type incomingTestPhone struct {
	conn      net.Conn
	reader    *bufio.Reader
	transport string
	id        string
	account   string
	reg       telephony.Registration
	invite    *sip.Request
	media     *net.UDPConn
}

func (p *incomingTestPhone) read(t *testing.T) sip.Message {
	t.Helper()
	p.conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var wire []byte
	if p.transport == "udp" {
		wire = make([]byte, 16384)
		n, e := p.conn.Read(wire)
		if e != nil {
			t.Fatal(e)
		}
		wire = wire[:n]
	} else {
		size := 0
		for {
			line, e := p.reader.ReadString('\n')
			if e != nil {
				t.Fatal(e)
			}
			wire = append(wire, line...)
			fmt.Sscanf(line, "Content-Length: %d", &size)
			if line == "\r\n" {
				break
			}
		}
		body := make([]byte, size)
		if _, e := io.ReadFull(p.reader, body); e != nil {
			t.Fatal(e)
		}
		wire = append(wire, body...)
	}
	m, e := sip.ParseMessage(wire)
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func (p *incomingTestPhone) write(t *testing.T, m sip.Message) {
	t.Helper()
	if _, e := io.WriteString(p.conn, m.String()); e != nil {
		t.Fatal(e)
	}
}
func (p *incomingTestPhone) response(t *testing.T, req *sip.Request, code int, body []byte) {
	r := sip.NewResponseFromRequest(req, code, "Test", body)
	r.To().Params.Add("tag", "phone-"+p.id)
	r.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: p.id, Host: "192.0.2.88", Port: 9999}})
	if body != nil {
		r.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	p.write(t, r)
}
func (p *incomingTestPhone) answer(t *testing.T) {
	body := []byte(fmt.Sprintf("v=0\r\nc=IN IP4 192.0.2.88\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n", p.media.LocalAddr().(*net.UDPAddr).Port))
	p.response(t, p.invite, 200, body)
}

func incomingWireSetup(t *testing.T, transport string, count int, shared ...bool) (*sipCalls, []*incomingTestPhone, *incomingTestVoice, *sipIncoming, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g := newSIPGateway(&server{})
	c := g.calls
	c.ctx = ctx
	var phones []*incomingTestPhone
	var accounts []sipregistrar.Account
	var address string
	var port int
	for i := 0; i < count; i++ {
		newAccount := i == 0 || len(shared) == 0 || !shared[0]
		if newAccount {
			reserve, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			address, port = reserve.Addr().String(), reserve.Addr().(*net.TCPAddr).Port
			reserve.Close()
		}
		id := fmt.Sprintf("user-%d", i)
		account := id
		if !newAccount {
			account = "user-0"
		}
		if newAccount {
			a, b := md5.Sum([]byte(account+":rykvo:secret")), sha256.Sum256([]byte(account+":rykvo:secret"))
			accounts = append(accounts, sipregistrar.Account{ID: account, Username: account, Revision: 1, Port: port, MD5: a[:], SHA256: b[:]})
			g.registrar.Replace(accounts)
			listener, e := g.listen(address, port)
			if e != nil {
				t.Fatal(e)
			}
			g.listeners[address] = listener
		}
		conn, e := net.Dial(transport, address)
		if e != nil {
			t.Fatal(e)
		}
		media, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if e != nil {
			t.Fatal(e)
		}
		p := &incomingTestPhone{conn: conn, reader: bufio.NewReader(conn), transport: transport, id: id, account: account, media: media}
		phones = append(phones, p)
		register := func(seq int, auth string) *sip.Response {
			raw := fmt.Sprintf("REGISTER sip:%s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=z9hG4bK-%s-%d;rport\r\nFrom: <sip:%s@localhost>;tag=register\r\nTo: <sip:%s@localhost>\r\nCall-ID: register-%s\r\nCSeq: %d REGISTER\r\nContact: <sip:%s@192.0.2.88:9999>;expires=300\r\n%sContent-Length: 0\r\n\r\n", address, strings.ToUpper(transport), conn.LocalAddr(), id, seq, account, account, id, seq, id, auth)
			if _, e := io.WriteString(conn, raw); e != nil {
				t.Fatal(e)
			}
			return p.read(t).(*sip.Response)
		}
		challenge, e := digest.ParseChallenge(register(1, "").GetHeaders("WWW-Authenticate")[0].Value())
		if e != nil {
			t.Fatal(e)
		}
		credential, e := digest.Digest(challenge, digest.Options{Method: "REGISTER", URI: "sip:" + address, Username: account, Password: "secret", Count: 1, Cnonce: "test"})
		if e != nil {
			t.Fatal(e)
		}
		if r := register(2, "Authorization: "+credential.String()+"\r\n"); r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
		c.router.PutPolicy(telephony.Policy{Account: account, Revision: 1, All: true, Receive: true})
		reg, _, _ := c.router.Register(account, 1, time.Minute)
		for binding, registration := range g.registrar.Registrations() {
			if registration.Account == account && registration.Source == conn.LocalAddr().String() {
				c.registrations[binding] = reg
			}
		}
		p.reg = reg
	}
	device := &incomingTestVoice{}
	call := c.startIncomingLocked("module-03", sipVoiceSample{wifi: true}, device, "+12025550123", sipAccountNetwork{Start: 20000, End: 30000})
	if call == nil {
		t.Fatal("incoming not scheduled")
	}
	done := make(chan struct{})
	go func() { defer close(done); call.run() }()
	t.Cleanup(func() {
		cancel()
		call.stop("interrupted")
		for _, l := range call.legs {
			l.peerClosed.Store(true)
			l.stop("interrupted")
		}
		for _, p := range phones {
			p.conn.Close()
			p.media.Close()
		}
		for _, l := range g.listeners {
			l.close()
		}
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("incoming driver did not stop")
		}
	})
	for _, p := range phones {
		r, ok := p.read(t).(*sip.Request)
		if !ok || r.Method != sip.INVITE {
			t.Fatal("missing incoming INVITE")
		}
		p.invite = r
		p.response(t, r, 180, nil)
	}
	return c, phones, device, call, done
}

func TestSIPGatewayIncomingFirstAnswerAndRemoteHangup(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, transport := range []string{"udp", "tcp"} {
			t.Run(fmt.Sprintf("%s/shared=%t", transport, shared), func(t *testing.T) {
				c, phones, device, call, done := incomingWireSetup(t, transport, 2, shared)
				// Both answer: the losing 200 must receive ACK and BYE, never carrier audio.
				for _, p := range phones {
					p.answer(t)
				}
				for _, p := range phones {
					r, ok := p.read(t).(*sip.Request)
					if ok && r.Method == sip.CANCEL {
						p.response(t, r, 200, nil)
						r, ok = p.read(t).(*sip.Request)
					}
					if !ok || r.Method != sip.ACK {
						t.Fatalf("want ACK, got %v", r)
					}
				}
				until := time.Now().Add(2 * time.Second)
				for !call.connected.Load() && time.Now().Before(until) {
					time.Sleep(time.Millisecond)
				}
				if !call.connected.Load() || device.answers.Load() != 1 {
					t.Fatal("not exactly one carrier answer", device.answers.Load())
				}
				snapshot, _ := c.router.Snapshot(call.call.ID)
				var winner, loser *incomingTestPhone
				for _, p := range phones {
					if p.reg.ID == snapshot.Winner {
						winner = p
					} else {
						loser = p
					}
				}
				bye, ok := loser.read(t).(*sip.Request)
				if !ok || bye.Method != sip.BYE {
					t.Fatal("late answer not terminated")
				}
				loser.response(t, bye, 200, nil)
				winner.media.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 512)
				if n, _, e := winner.media.ReadFromUDP(buf); e != nil || n != 172 {
					t.Fatal("winner PCM not delivered", n, e)
				}
				device.state.Store(2)
				at := time.Now()
				bye, ok = winner.read(t).(*sip.Request)
				if !ok || bye.Method != sip.BYE {
					t.Fatal("carrier hangup not forwarded")
				}
				if time.Since(at) > 2*time.Second {
					t.Fatal("BYE delayed")
				}
				winner.response(t, bye, 200, nil)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("cleanup stuck")
				}
				if _, exists := c.router.Snapshot(call.call.ID); exists {
					t.Fatal("reservations not released")
				}
				leg := call.legs[loser.reg.ID]
				leg.mu.Lock()
				reason := leg.outcome
				leg.mu.Unlock()
				call.recordsMu.Lock()
				count := len(call.records)
				call.recordsMu.Unlock()
				want := 2
				if shared {
					want = 1
				}
				if count != want {
					t.Fatal("duplicate account records", count, want)
				}
				if reason != "answered_elsewhere" {
					t.Fatal("loser result", reason)
				}
			})
		}
	}

}
func TestSIPGatewayIncomingOneRejectionDoesNotEndOthers(t *testing.T) {
	c, phones, device, call, done := incomingWireSetup(t, "udp", 2)
	phones[0].response(t, phones[0].invite, 603, nil)
	if r := phones[0].read(t).(*sip.Request); r.Method != sip.ACK {
		t.Fatal("missing failure ACK")
	}
	if device.state.Load() != 0 {
		t.Fatal("one rejection ended carrier")
	}
	phones[1].answer(t)
	if r := phones[1].read(t).(*sip.Request); r.Method != sip.ACK {
		t.Fatal("missing winner ACK")
	}
	until := time.Now().Add(2 * time.Second)
	for !call.connected.Load() && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if !call.connected.Load() {
		t.Fatal("remaining user cannot answer")
	}
	device.state.Store(2)
	r := phones[1].read(t).(*sip.Request)
	if r.Method != sip.BYE {
		t.Fatal("no BYE")
	}
	phones[1].response(t, r, 200, nil)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup stuck")
	}
	if c.router.Status(phones[1].id) != telephony.Online {
		t.Fatal("account remains busy")
	}
}

func TestSIPGatewayIncomingCancelLateAnswerAndDisabledReceiver(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			c, phones, device, call, done := incomingWireSetup(t, "udp", 1)
			p := phones[0]
			c.g.server.sipAccountsMu.Lock()
			actions, _ := c.router.PutPolicy(telephony.Policy{Account: p.id, Revision: 1, All: true, Receive: false})
			c.actionsLocked(actions)
			c.g.server.sipAccountsMu.Unlock()
			r := p.read(t).(*sip.Request)
			if r.Method != sip.CANCEL {
				t.Fatal("disabled receiver still ringing", r.Method)
			}
			p.response(t, r, 200, nil)
			if late {
				p.answer(t)
				if ack := p.read(t).(*sip.Request); ack.Method != sip.ACK {
					t.Fatal("late answer missing ACK")
				}
				bye := p.read(t).(*sip.Request)
				if bye.Method != sip.BYE {
					t.Fatal("late answer missing BYE")
				}
				p.response(t, bye, 200, nil)
			} else {
				p.response(t, p.invite, 487, nil)
				if ack := p.read(t).(*sip.Request); ack.Method != sip.ACK {
					t.Fatal("487 missing ACK")
				}
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("cancel cleanup stuck")
			}
			if device.answers.Load() != 0 {
				t.Fatal("late answer answered carrier")
			}
			if _, ok := c.router.Snapshot(call.call.ID); ok {
				t.Fatal("reservation leaked")
			}
			// With every receiver disabled no leg/record is created for the next call.
			c.g.server.sipAccountsMu.Lock()
			next := c.startIncomingLocked("module-03", sipVoiceSample{}, &incomingTestVoice{}, "12345", sipAccountNetwork{Start: 20000, End: 30000})
			c.g.server.sipAccountsMu.Unlock()
			if next != nil {
				t.Fatal("all-disabled created incoming history")
			}
		})
	}
}

func TestSIPGatewayIncomingRegistrationAndRevocation(t *testing.T) {
	for _, reason := range []string{"logout", "delete", "password", "suspend", "logout-then-password"} {
		t.Run(reason, func(t *testing.T) {
			c, phones, device, call, done := incomingWireSetup(t, "udp", 1)
			p := phones[0]
			p.answer(t)
			if r := p.read(t).(*sip.Request); r.Method != sip.ACK {
				t.Fatal(r.Method)
			}
			until := time.Now().Add(2 * time.Second)
			for !call.connected.Load() && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if !call.connected.Load() {
				t.Fatal("not connected")
			}
			c.g.server.sipAccountsMu.Lock()
			var actions []telephony.Action
			switch reason {
			case "logout":
				actions, _ = c.router.Logout(p.reg)
			case "logout-then-password":
				logout, err := c.router.Logout(p.reg)
				c.actionsLocked(logout)
				if err != nil || call.ctx.Err() != nil {
					c.g.server.sipAccountsMu.Unlock()
					t.Fatal("registration removal interrupted dialog")
				}
				actions, _ = c.router.PutPolicy(telephony.Policy{Account: p.id, Revision: 2, All: true, Receive: true})
			case "suspend":
				actions = c.router.SuspendAccount(p.id)
			case "delete":
				actions = c.router.DeleteAccount(p.id)
			case "password":
				actions, _ = c.router.PutPolicy(telephony.Policy{Account: p.id, Revision: 2, All: true, Receive: true})
			}
			c.actionsLocked(actions)
			c.g.server.sipAccountsMu.Unlock()
			if reason == "logout" {
				if call.ctx.Err() != nil || device.state.Load() != 1 {
					t.Fatal("registration removal stopped established media")
				}
				device.state.Store(2) // Real carrier hangup still sends BYE after unregister.
			} else if call.ctx.Err() == nil {
				t.Fatal("revocation kept media live")
			}
			bye := p.read(t).(*sip.Request)
			if bye.Method != sip.BYE {
				t.Fatal(bye.Method)
			}
			p.response(t, bye, 200, nil)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("revocation cleanup stuck")
			}
			if device.state.Load() != 2 {
				t.Fatal("carrier not ended")
			}
		})
	}
}

func TestSIPGatewayIncomingLostTerminationReplyKeepsLoginNotCall(t *testing.T) {
	for _, lost := range []string{"invite-final", "bye"} {
		t.Run(lost, func(t *testing.T) {
			count := 1
			if lost == "invite-final" {
				count = 2
			}
			c, phones, device, call, done := incomingWireSetup(t, "udp", count, true)
			winner := phones[0]
			winner.answer(t)
			if req := winner.read(t).(*sip.Request); req.Method != sip.ACK {
				t.Fatal(req.Method)
			}
			until := time.Now().Add(2 * time.Second)
			for !call.connected.Load() && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if !call.connected.Load() {
				t.Fatal("winner not connected")
			}
			if count == 2 {
				loser := phones[1]
				req := loser.read(t).(*sip.Request)
				if req.Method != sip.CANCEL {
					t.Fatal(req.Method)
				}
				loser.response(t, req, 200, nil) // No final INVITE response from this phone.
			}
			device.state.Store(2)
			bye := winner.read(t).(*sip.Request)
			if bye.Method != sip.BYE {
				t.Fatal(bye.Method)
			}
			if lost != "bye" {
				winner.response(t, bye, 200, nil)
			}
			select {
			case <-done:
			case <-time.After(40 * time.Second):
				t.Fatal("SIP cleanup never completed")
			}
			c.wait.Wait()
			if _, ok := c.router.Snapshot(call.call.ID); ok || len(c.incoming) != 0 {
				t.Fatal("ended call held by online phone")
			}
			if !c.g.registrar.Online(winner.account) || c.router.Status(winner.account) != telephony.Online {
				t.Fatal("call cleanup revoked login")
			}
			if _, _, err := c.router.Dial(winner.reg); err != nil {
				t.Fatal("next call blocked", err)
			}
			call.recordsMu.Lock()
			record := *call.records[winner.account]
			call.recordsMu.Unlock()
			if time.Since(record.ended) < time.Second {
				t.Fatal("SIP cleanup time counted as talk time")
			}
		})
	}
}
