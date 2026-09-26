package ims

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func terminationSession(t *testing.T, state string, byeCode int, release <-chan struct{}) (*Session, *imsCall, <-chan string) {
	t.Helper()
	client, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	media, err := newRTPMedia(net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	sdp := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 31000 RTP/AVP 0\r\n")
	if err := media.configureRemote(sdp); err != nil {
		t.Fatal(err)
	}
	call := &imsCall{public: vowifi.Call{ID: "test-call", Direction: "outgoing", State: state, SIPCode: 180, MediaReady: true, Codec: "PCMU"}, callID: "test-call", target: "sip:remote@example.test", inviteTarget: "sip:remote@example.test", from: "<sip:local@example.test>;tag=local", to: "<sip:remote@example.test>;tag=remote", branch: "original", cseq: 1, media: media, accepted: state == "active", responses: make(chan *sipResponse, 8)}
	s := &Session{conn: client, transport: "tcp", provider: &Provider{config: Config{TransactionTimeout: time.Second}}, calls: map[string]*imsCall{call.callID: call}, transactions: make(map[sipTransactionKey]chan *sipResponse), refreshContext: ctx, cseq: 2}
	wire := make(chan string, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := bufio.NewReader(peer)
		for {
			p, err := readSIPPacket(reader)
			if err != nil {
				return
			}
			r := p.Request
			wire <- r.Method
			if r.Method == "ACK" {
				continue
			}
			if r.Method == "BYE" && release != nil {
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
			}
			code := 200
			if r.Method == "BYE" {
				code = byeCode
			}
			s.transactionsMu.Lock()
			for key, ch := range s.transactions {
				if key.method == r.Method {
					ch <- &sipResponse{StatusCode: code}
					break
				}
			}
			s.transactionsMu.Unlock()
		}
	}()
	t.Cleanup(func() { cancel(); client.Close(); peer.Close(); media.Close(); <-done })
	return s, call, wire
}
func nextCallMethod(t *testing.T, wire <-chan string, want string) {
	t.Helper()
	select {
	case got := <-wire:
		if got != want {
			t.Fatalf("method %s, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("missing %s", want)
	}
}
func waitCallState(t *testing.T, s *Session, want string) vowifi.Call {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		calls := s.Calls()
		if len(calls) == 1 && calls[0].State == want {
			return calls[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("want %s, got %+v", want, s.Calls())
	return vowifi.Call{}
}
func TestCancelWaitsForInviteFinalAndLateAcceptanceIsClosed(t *testing.T) {
	release := make(chan struct{})
	s, c, wire := terminationSession(t, "ringing", 200, release)
	if err := s.HangupCall(context.Background(), c.callID); !errors.Is(err, ErrCallEnding) {
		t.Fatal(err)
	}
	nextCallMethod(t, wire, "CANCEL")
	state := waitCallState(t, s, "ending")
	if state.EndedAt != nil || state.MediaReady {
		t.Fatal("CANCEL acknowledgement released carrier call")
	}
	c.media.downlink <- []int16{1, 2}
	if _, err := c.media.ReadPCM(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("closed audio still readable", err)
	}
	if err := c.media.WritePCM([]int16{1}); !errors.Is(err, io.EOF) {
		t.Fatal("closed audio accepted", err)
	}
	done := make(chan struct{})
	watchCtx, watchCancel := context.WithCancel(s.refreshContext)
	s.refreshContext = watchCtx
	go func() {
		defer close(done)
		s.watchOutgoingCall(c, sipTransactionKey{callID: c.callID, cseq: 1, method: "INVITE"})
	}()
	t.Cleanup(func() { watchCancel(); s.conn.Close(); <-done })
	accepted := &sipResponse{StatusCode: 200, Headers: map[string][]string{"to": {"<sip:remote@example.test>;tag=winner"}, "contact": {"<sip:remote@example.test>"}}}
	c.responses <- accepted
	nextCallMethod(t, wire, "ACK")
	nextCallMethod(t, wire, "BYE")
	if got := waitCallState(t, s, "ending"); got.AnsweredAt != nil || got.EndedAt != nil {
		t.Fatal("late answer revived call")
	}
	close(release)
	if got := waitCallState(t, s, "ended"); got.EndedAt == nil {
		t.Fatal("missing termination")
	}
	c.responses <- accepted
	nextCallMethod(t, wire, "ACK")
	if s.Calls()[0].State != "ended" {
		t.Fatal("retransmission revived call")
	}
}
func TestUnconfirmedBYERemainsQuarantinedAnd481Releases(t *testing.T) {
	for _, tc := range []struct {
		code  int
		state string
	}{{500, "ending"}, {481, "ended"}, {200, "ended"}} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			s, c, wire := terminationSession(t, "active", tc.code, nil)
			err := s.HangupCall(context.Background(), c.callID)
			nextCallMethod(t, wire, "BYE")
			got := waitCallState(t, s, tc.state)
			if (err == nil) != (tc.code != 500) || (got.EndedAt != nil) != (tc.code != 500) || got.MediaReady {
				t.Fatalf("err=%v call=%+v", err, got)
			}
			s.setCallState(c.callID, "active")
			s.setCallMediaReady(c.callID)
			if s.Calls()[0].State != tc.state || s.Calls()[0].MediaReady {
				t.Fatal("late event revived terminated call")
			}
		})
	}
}
func TestHangupBeforeProvisionalDoesNotSendPrematureCancel(t *testing.T) {
	s, c, wire := terminationSession(t, "dialing", 200, nil)
	s.callMu.Lock()
	c.public.SIPCode = 0
	s.callMu.Unlock()
	if err := s.HangupCall(context.Background(), c.callID); !errors.Is(err, ErrCallEnding) {
		t.Fatal(err)
	}
	select {
	case m := <-wire:
		t.Fatal("premature request", m)
	default:
	}
	s.setCallState(c.callID, "ringing")
	if !strings.EqualFold(s.Calls()[0].State, "ending") {
		t.Fatal("ringing revived terminated call")
	}
}

func TestIncomingRetransmissionAndLateCancelPreserveAcceptedCall(t *testing.T) {
	s := &Session{fromTag: "local", calls: map[string]*imsCall{}}
	packet, err := parseSIPPacket([]byte("INVITE sip:local@example.test SIP/2.0\r\nVia: SIP/2.0/UDP 192.0.2.1;branch=z9hG4bKfirst\r\nFrom: <sip:remote@example.test>;tag=remote\r\nTo: <sip:local@example.test>\r\nCall-ID: incoming-1\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	var response []byte
	respond := func(b []byte) error { response = append([]byte(nil), b...); return nil }
	s.handleCallRequest(packet.Request, respond)
	call := s.calls["incoming-1"]
	t.Cleanup(func() { call.media.Close() })
	s.handleCallRequest(packet.Request, respond)
	if s.calls["incoming-1"] != call || !strings.HasPrefix(string(response), "SIP/2.0 180") {
		t.Fatal("retransmission replaced dialog")
	}
	if _, err := s.AnswerCall(context.Background(), call.callID); err != nil {
		t.Fatal(err)
	}
	s.handleCallRequest(packet.Request, respond)
	if !strings.HasPrefix(string(response), "SIP/2.0 200") {
		t.Fatal("missing cached answer")
	}
	cancel := *packet.Request
	cancel.Method = "CANCEL"
	cancel.Headers = map[string][]string{}
	for k, v := range packet.Request.Headers {
		cancel.Headers[k] = append([]string(nil), v...)
	}
	cancel.Headers["cseq"] = []string{"1 CANCEL"}
	s.handleCallRequest(&cancel, respond)
	if !strings.HasPrefix(string(response), "SIP/2.0 200") || s.Calls()[0].State != "active" {
		t.Fatal("late CANCEL ended answered dialog")
	}
	cancel.Headers["cseq"] = []string{"999 CANCEL"}
	s.handleCallRequest(&cancel, respond)
	if !strings.HasPrefix(string(response), "SIP/2.0 481") || s.Calls()[0].State != "active" {
		t.Fatal("unmatched CANCEL changed dialog")
	}
	cancel.Headers["cseq"] = []string{""}
	if matchesCancelledInvite(packet.Request, &cancel) {
		t.Fatal("malformed CSeq matched")
	}
}

func TestSecondIncomingDoesNotReplaceOccupiedModule(t *testing.T) {
	s := &Session{fromTag: "local", calls: map[string]*imsCall{"first": {callID: "first", public: vowifi.Call{ID: "first", Direction: "incoming", State: "ringing"}}}}
	p, err := parseSIPPacket([]byte("INVITE sip:local@example.test SIP/2.0\r\nVia: SIP/2.0/UDP 192.0.2.1;branch=z9hG4bKsecond\r\nFrom: <sip:remote@example.test>;tag=remote\r\nTo: <sip:local@example.test>\r\nCall-ID: second\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	var response []byte
	s.handleCallRequest(p.Request, func(b []byte) error { response = b; return nil })
	if len(s.Calls()) != 1 || !strings.HasPrefix(string(response), "SIP/2.0 486") {
		t.Fatal("occupied module accepted second call")
	}
}

func TestCancelKeepsOriginalInviteTargetToAndRoute(t *testing.T) {
	s, c, _ := terminationSession(t, "ringing", 200, nil)
	s.callMu.Lock()
	c.inviteTo = "<sip:original@example.test>"
	c.inviteTarget = "sip:original@example.test"
	c.inviteRoutes = []string{"<sip:original-proxy.example.test;lr>"}
	c.to = "<sip:original@example.test>;tag=early-dialog"
	c.target = "sip:redirected@example.test"
	c.routes = []string{"<sip:early-proxy.example.test;lr>"}
	s.callMu.Unlock()
	packet := string(s.buildDialogRequest(c, "CANCEL", 1))
	for _, want := range []string{"CANCEL sip:original@example.test SIP/2.0", "To: <sip:original@example.test>\r\n", "Route: <sip:original-proxy.example.test;lr>\r\n", "branch=z9hG4bKoriginal;rport"} {
		if !strings.Contains(packet, want) {
			t.Fatalf("missing %s in %s", want, packet)
		}
	}
}
