package ims

import (
	"context"
	"errors"
	"fmt"
	"rykvo.local/auth/internal/vocat/vowifi"
	"strings"
	"testing"
	"time"
)

func testRPResultRequest(r *sipRequest, addr string, body []byte) []byte {
	headers := []string{
		"MESSAGE " + firstURI(r.value("From")) + " SIP/2.0",
		"Via: SIP/2.0/UDP " + addr + ";branch=z9hG4bKrp" + fmt.Sprint(body[1]),
		"From: " + r.value("To") + ";tag=gw", "To: " + r.value("From"),
		"P-Asserted-Identity: " + r.value("To"), "Call-ID: rp-" + r.value("Call-ID"),
		"In-Reply-To: " + r.value("Call-ID"), "CSeq: 1 MESSAGE",
		"Content-Type: " + smsContentType, fmt.Sprintf("Content-Length: %d", len(body)), "", "",
	}
	return append([]byte(strings.Join(headers, "\r\n")), body...)
}
func TestRPResultsRequireNetworkTypeAndValidLengths(t *testing.T) {
	for _, data := range [][]byte{{3, 2}, {5, 2, 1, 42}, {3, 2, 0x41, 1, 0}, {5, 2, 1, 0x80 | 21, 0x41, 1, 0}} {
		r, e := parseRPResult(data)
		if e != nil || r.accepted != (data[0] == 3) {
			t.Fatal(data, r, e)
		}
	}
	for _, data := range [][]byte{{}, {3}, {2, 2}, {4, 2, 1, 42}, {5, 2}, {5, 2, 0}, {5, 2, 2, 42}, {3, 2, 0x41}, {3, 2, 0x41, 2, 0}, {3, 2, 4, 0}} {
		if _, e := parseRPResult(data); e == nil {
			t.Fatal("accepted malformed", data)
		}
	}
}
func TestRPResultCorrelationAndEarlyResponse(t *testing.T) {
	s := &Session{}
	ref, p, e := s.beginRP()
	if e != nil {
		t.Fatal(e)
	}
	p.callID = "expected"
	request := func(reply string) *sipRequest {
		packet, e := parseSIPPacket([]byte("MESSAGE sip:a@example.test SIP/2.0\r\nIn-Reply-To: " + reply + "\r\nContent-Length: 0\r\n\r\n"))
		if e != nil {
			t.Fatal(e)
		}
		return packet.Request
	}
	s.receiveRPResult(request("wrong"), []byte{3, ref})
	s.receiveRPResult(request("expected"), []byte{3, ref + 1})
	if len(p.result) != 0 {
		t.Fatal("wrong transaction matched")
	}
	s.receiveRPResult(request("expected"), []byte{3, ref})
	r, e := s.awaitRP(context.Background(), p)
	if e != nil || !r.accepted {
		t.Fatal(r, e)
	}
	s.endRP(ref)
	s.nextRPReference = ref
	next, _, e := s.beginRP()
	if e != nil || next == ref {
		t.Fatal("reused quarantined RP")
	}
}
func TestSMSRPRejectAndTimeoutAreNotAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply []byte
		cause bool
	}{{"reject", []byte{5, 21}, true}, {"no_report", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, seen, _ := smsPSITestSessionRP(t, &recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}}}, tc.reply)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			r, e := s.SendSMS(ctx, vowifi.SMSSubmitRequest{Recipient: "+12025550123", Text: "OFFLINE TEST"})
			if e == nil || r.AllPartsAccepted || r.PartsAccepted != 0 || r.PartsAttempted != 1 || len(r.PartResults) != 1 {
				t.Fatal(r, e)
			}
			part := r.PartResults[0]
			if part.SIPCode != 202 || part.Accepted || part.RPAcknowledged {
				t.Fatal(part)
			}
			if tc.cause && (!errors.Is(e, ErrSMSRejected) || part.RPCause == nil || *part.RPCause != 21) {
				t.Fatal(part, e)
			}
			<-seen
			select {
			case <-seen:
				t.Fatal("resubmitted")
			default:
			}
		})
	}
}
func TestRPAckIsNotRecipientDelivery(t *testing.T) {
	s, _, _ := smsPSITestSession(t, &recordingAKA{result: vowifi.AKAResult{RES: []byte{1, 2, 3, 4}}})
	r, e := s.SendSMS(context.Background(), vowifi.SMSSubmitRequest{Recipient: "+12025550123", Text: "OFFLINE TEST"})
	if e != nil || !r.AllPartsAccepted || !r.PartResults[0].RPAcknowledged || r.DeliveryConfirmed {
		t.Fatal(r, e)
	}
}
