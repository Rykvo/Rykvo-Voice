package ims

import (
	"context"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestCallFailureRequiresStructuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		code            int
		reason, warning string
		want            vowifi.CallFailure
	}{
		{606, "", "", ""},
		{606, `Q.850;cause=21;text="Call rejected"`, "", "rejected"},
		{606, `SIP;cause=606, Q.850; cause = 17`, "", "busy"},
		{606, `q.850;cause=19`, "", "no_answer"},
		{606, `Q.850;cause=16`, "", "remote_cancelled"},
		{606, `Q.850;cause=31`, "", "remote_cancelled"},
		{606, `Q.850;cause=88`, "", "unsupported_audio"},
		{400, "", `305 ims "Incompatible media format"`, "unsupported_audio"},
		{606, "", `370 ims "Insufficient bandwidth"`, ""},
		{606, "", `399 ims "305 Incompatible media format"`, ""},
		{606, `Q.850;text=";cause=21"`, "", ""},
		{606, `Q.850;cause=21;cause=17`, "", ""},
		{606, `Q.850;cause=21, Q.850;cause=17`, "", ""},
		{606, `Q.850;cause=+21`, "", ""},
		{606, `Q.850;cause=256`, "", ""},
		{606, `Q.850;cause=21;text="unterminated`, "", ""},
		{200, `Q.850;cause=21`, "", ""},
	} {
		r := &sipResponse{StatusCode: tc.code, Headers: map[string][]string{"reason": {tc.reason}, "warning": {tc.warning}}}
		if got := callResponseFailure(r); got != tc.want {
			t.Fatalf("%+v: got %q", tc, got)
		}
	}
}

func TestRejectedOutgoingResponsePublishesStructuredCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := &imsCall{public: vowifi.Call{ID: "cause", State: "dialing"}, callID: "cause", responses: make(chan *sipResponse, 1)}
	s := &Session{calls: map[string]*imsCall{"cause": call}, transactions: make(map[sipTransactionKey]chan *sipResponse), refreshContext: ctx}
	go s.watchOutgoingCall(call, sipTransactionKey{callID: "cause", cseq: 1, method: "INVITE"})
	call.responses <- &sipResponse{StatusCode: 606, Headers: map[string][]string{"reason": {`Q.850;cause=21`}}}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		calls := s.Calls()
		if len(calls) == 1 && calls[0].EndedAt != nil {
			if calls[0].SIPCode != 606 || calls[0].Failure != "rejected" {
				t.Fatalf("cause lost: %+v", calls[0])
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("call did not finish")
}
