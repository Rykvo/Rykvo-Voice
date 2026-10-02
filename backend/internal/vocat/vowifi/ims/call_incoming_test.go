package ims

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIncomingResponsesPreserveCarrierDialogRoutes(t *testing.T) {
	routes := []string{"<sip:pcscf.example.test:5060;lr;token=one>", "<sip:scscf.example.test;lr>, <sip:tas.example.test;lr;token=two>"}
	request := &sipRequest{Method: "INVITE", Headers: map[string][]string{
		"via":  {"SIP/2.0/UDP 192.0.2.10:5060;branch=z9hG4bKroute"},
		"from": {"<sip:caller@example.test>;tag=remote"}, "to": {"<sip:callee@example.test>"},
		"call-id": {"incoming-route"}, "cseq": {"18 INVITE"}, "record-route": routes,
	}}
	for _, code := range []int{180, 200} {
		wire, err := buildSIPResponseWithBody(request, code, "local", nil)
		if err != nil {
			t.Fatal(err)
		}
		packet, err := parseSIPPacket(wire)
		if err != nil || packet.Response == nil {
			t.Fatalf("response %d: %v", code, err)
		}
		if got := packet.Response.values("Record-Route"); !slices.Equal(got, routes) {
			t.Fatalf("response %d lost carrier dialog path: got %v want %v", code, got, routes)
		}
	}
}

func TestIncomingCarrierACKGatesMediaAndStopsAnswerRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bind := net.IPv4(127, 0, 0, 1)
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: bind})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := newRTPMedia(bind)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s := &Session{fromTag: "local", calls: map[string]*imsCall{}, refreshContext: ctx, conn: conn}
	raw := "INVITE sip:local@example.test SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKfirst\r\nFrom: <sip:remote@example.test>;tag=remote\r\nTo: <sip:local@example.test>\r\nCall-ID: ack-gate\r\nCSeq: 18 INVITE\r\nContent-Type: application/sdp\r\nContent-Length: 0\r\n\r\n"
	p, e := parseSIPPacket([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	p.Request.Body = peer.offerSDP(bind)
	p.Request.Headers["record-route"] = []string{"<sip:pcscf.example.test;lr;token=carrier>"}
	p.Request.Headers["supported"] = []string{"100rel, timer"}
	p.Request.Headers["session-expires"] = []string{"1800"}
	responses := make(chan string, 8)
	s.handleCallRequest(p.Request, func(b []byte) error { responses <- string(b); return nil })
	<-responses
	t.Cleanup(func() { s.finishCall("ack-gate", "ended", 0, "") })
	if c, e := s.AnswerCall(ctx, "ack-gate"); e != nil || c.MediaReady {
		t.Fatal("media before ACK", c, e)
	}
	first := <-responses
	answer, err := parseSIPPacket([]byte(first))
	if err != nil || answer.Response == nil || !slices.Equal(answer.Response.values("Record-Route"), p.Request.values("Record-Route")) {
		t.Fatal("carrier cannot route its ACK through the original dialog path", err)
	}
	if answer.Response.value("Session-Expires") != "1800;refresher=uas" || answer.Response.value("Require") != "timer" {
		t.Fatal("carrier answer omitted the negotiated session timer")
	}
	if err = peer.configureRemote(answer.Response.Body); err != nil {
		t.Fatal(err)
	}
	select {
	case again := <-responses:
		if again != first {
			t.Fatal("answer changed on repeat")
		}
	case <-time.After(time.Second):
		t.Fatal("answer not retransmitted")
	}
	ack := func(seq, from, to string) {
		v := &sipRequest{Method: "ACK", Headers: map[string][]string{"call-id": {"ack-gate"}, "cseq": {seq + " ACK"}, "from": {"<sip:remote@example.test>;tag=" + from}, "to": {"<sip:local@example.test>;tag=" + to}}}
		s.handleCallRequest(v, func([]byte) error { return nil })
	}
	ack("19", "remote", "local")
	ack("18", "wrong", "local")
	ack("18", "remote", "wrong")
	if s.Calls()[0].MediaReady {
		t.Fatal("unrelated ACK activated media")
	}
	ack("18", "remote", "local")
	if !s.Calls()[0].MediaReady {
		t.Fatal("matching ACK did not activate media")
	}
	select {
	case <-responses:
		t.Fatal("answer repeated after ACK")
	case <-time.After(1100 * time.Millisecond):
	}
	media, e := s.CallMedia(ctx, "ack-gate")
	if e != nil {
		t.Fatal(e)
	}
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = 4096
	}
	audio, done := context.WithTimeout(ctx, time.Second)
	defer done()
	if err := peer.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	if got, err := media.ReadPCM(audio); err != nil || len(got) != 160 || got[0] < 3900 {
		t.Fatal("incoming audio missing", err, got)
	}
	if err := media.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	if got, err := peer.ReadPCM(audio); err != nil || len(got) != 160 || got[0] < 3900 {
		t.Fatal("outgoing audio missing", err, got)
	}
	bye := &sipRequest{Method: "BYE", Headers: map[string][]string{
		"via": {"SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKbye"}, "call-id": {"ack-gate"}, "cseq": {"19 BYE"},
		"from": {"<sip:remote@example.test>;tag=remote"}, "to": {"<sip:local@example.test>;tag=local"},
	}}
	s.handleCallRequest(bye, func(b []byte) error { responses <- string(b); return nil })
	if state := s.Calls()[0]; state.State != "ended" || state.EndedAt == nil || state.MediaReady {
		t.Fatal("remote BYE left carrier call active", state)
	}
	if !strings.HasPrefix(first, "SIP/2.0 200") {
		t.Fatal(first)
	}
}
