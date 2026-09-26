package ims

import (
	"context"
	"testing"
	"time"
)

// Reliable early SDP can answer the INVITE before its final 200 (RFC 3262).
func TestOutgoingFinalAnswerNegotiation(t *testing.T) {
	good := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 31000 RTP/AVP 0\r\n")
	bad := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 31000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\n")
	for _, tc := range []struct {
		name         string
		early, final []byte
		reliable     bool
		tag          string
		active       bool
	}{
		{name: "final SDP", final: good, tag: "early", active: true},
		{name: "reliable early answer", early: good, reliable: true, tag: "early", active: true},
		{name: "repeated SDP", early: good, final: good, reliable: true, tag: "early", active: true},
		{name: "missing answer", tag: "early"},
		{name: "unreliable early answer", early: good, tag: "early"},
		{name: "different dialog", early: good, reliable: true, tag: "other"},
		{name: "missing dialog", early: good, reliable: true},
		{name: "unsupported early codec", early: bad, reliable: true, tag: "early"},
		{name: "unsupported final codec", early: good, final: bad, reliable: true, tag: "early"},
		{name: "malformed final SDP", early: good, final: []byte("invalid"), reliable: true, tag: "early"},
		{name: "rejected audio", early: good, final: []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 0 RTP/AVP 0\r\n"), reliable: true, tag: "early"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, wire := terminationSession(t, "ringing", 200, nil)
			c.public.MediaReady = false
			c.public.Codec = ""
			c.media.mu.Lock()
			c.media.remote = nil
			c.media.codec = ""
			c.media.mu.Unlock()
			ctx, cancel := context.WithCancel(s.refreshContext)
			s.refreshContext = ctx
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.watchOutgoingCall(c, sipTransactionKey{callID: c.callID, cseq: 1, method: "INVITE"})
			}()
			t.Cleanup(func() { cancel(); s.conn.Close(); <-done })
			if tc.early != nil {
				early := &sipResponse{StatusCode: 183, Headers: map[string][]string{
					"to": {"<sip:remote@example.test>;tag=early"}, "contact": {"<sip:remote@example.test>"}, "cseq": {"1 INVITE"},
				}, Body: tc.early}
				if tc.reliable {
					early.Headers["require"] = []string{"100rel"}
					early.Headers["rseq"] = []string{"1"}
				}
				c.responses <- early
				if tc.reliable {
					nextCallMethod(t, wire, "PRACK")
				}
				if string(tc.early) == string(good) {
					waitCallState(t, s, "early_media")
				}
			}
			to := "<sip:remote@example.test>"
			if tc.tag != "" {
				to += ";tag=" + tc.tag
			}
			final := &sipResponse{StatusCode: 200, Headers: map[string][]string{"to": {to}, "contact": {"<sip:remote@example.test>"}}, Body: tc.final}
			c.responses <- final
			nextCallMethod(t, wire, "ACK")
			if !tc.active {
				nextCallMethod(t, wire, "BYE")
				state := waitCallState(t, s, "ended")
				if state.AnsweredAt != nil || state.MediaReady || state.SIPCode != 488 {
					t.Fatalf("invalid answer accepted: %+v", state)
				}
				return
			}
			state := waitCallState(t, s, "active")
			if !state.MediaReady || state.Codec != "PCMU" || state.AnsweredAt == nil || state.EndedAt != nil || state.SIPCode != 200 {
				t.Fatalf("lost negotiated media: %+v", state)
			}
			c.responses <- final
			nextCallMethod(t, wire, "ACK")
			select {
			case method := <-wire:
				t.Fatal("answered call unexpectedly ended:", method)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}
