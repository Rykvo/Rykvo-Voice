package ims

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestSIMDownloadRetransmissionDoesNotRepeatUICC(t *testing.T) {
	var calls atomic.Int32
	s := &Session{provider: &Provider{config: Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnSIMDataDownload: func(context.Context, SIMDataDownload) (vowifi.SMSPPResult, error) {
			calls.Add(1)
			return vowifi.SMSPPResult{Status: 0x9000, Data: []byte{1, 2}}, nil
		}}}}
	req := &sipRequest{Headers: map[string][]string{"call-id": {"download-1"}, "cseq": {"1 MESSAGE"}}}
	rp := rpMessage{reference: 7, tpdu: []byte{1}}
	a := s.processSIMDownload(req, rp, []byte{1, 7}, 0x7f, 0xf6)
	b := s.processSIMDownload(req, rp, []byte{1, 7}, 0x7f, 0xf6)
	if calls.Load() != 1 || !bytes.Equal(a, b) {
		t.Fatal("retransmission repeated UICC or changed report")
	}
	req.Headers["call-id"] = []string{"download-2"}
	s.processSIMDownload(req, rp, []byte{1, 7}, 0x7f, 0xf6)
	if calls.Load() != 2 {
		t.Fatal("fresh transaction incorrectly suppressed")
	}
}

func TestSIMDownloadReportsRealUICCResult(t *testing.T) {
	for _, tc := range []struct {
		status uint16
		data   []byte
		err    error
		want   []byte
		ok     bool
	}{
		{0x9000, nil, nil, []byte{2, 7}, true},
		{0x9101, nil, nil, []byte{2, 7}, true},
		{0x9000, []byte{0xaa, 0xbb}, nil, []byte{2, 7, 0x41, 7, 0, 7, 0x7f, 0xf6, 2, 0xaa, 0xbb}, true},
		{0x9300, nil, nil, []byte{4, 7, 1, 111, 0x41, 3, 0, 0xd4, 0}, false},
		{0x6300, nil, nil, []byte{4, 7, 1, 111, 0x41, 3, 0, 0xd5, 0}, false},
		{0, nil, errors.New("not connected"), []byte{4, 7, 1, 111, 0x41, 3, 0, 0xd5, 0}, false},
		{0x9000, bytes.Repeat([]byte{1}, 141), nil, []byte{4, 7, 1, 111, 0x41, 3, 0, 0xd5, 0}, false},
	} {
		got, ok := simDownloadReport(7, 0x7f, 0xf6, vowifi.SMSPPResult{Status: tc.status, Data: tc.data}, tc.err)
		if ok != tc.ok || !bytes.Equal(got, tc.want) {
			t.Fatalf("status=%04X report=%X success=%v", tc.status, got, ok)
		}
	}
}

func TestSMSMemoryAvailableNotifiesOnceAndWaitsForRP(t *testing.T) {
	var count atomic.Int32
	sent := make(chan struct{}, 1)
	var s *Session
	conn := &fakeConn{onWrite: func(raw []byte) {
		p, err := parseSIPPacket(raw)
		if err != nil || p.Request == nil {
			t.Error("invalid notice")
			return
		}
		if len(p.Request.Body) != 2 || p.Request.Body[0] != 6 {
			t.Error("not RP-SMMA")
			return
		}
		count.Add(1)
		cseq, _, _ := cseqNumber(p.Request.value("CSeq"))
		s.dispatchPacket(sipPacket{Response: &sipResponse{StatusCode: 202, Headers: map[string][]string{"call-id": {p.Request.value("Call-ID")}, "cseq": {fmt.Sprintf("%d MESSAGE", cseq)}}}}, nil)
		s.receiveRPResult(&sipRequest{}, []byte{3, p.Request.Body[1]})
		sent <- struct{}{}
	}}
	s = &Session{provider: &Provider{config: Config{NotifySMSMemoryAvailable: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TransactionTimeout: time.Second}}, conn: conn, transport: "tcp", identity: identitySet{public: "sip:user@example.com"}, transactions: make(map[sipTransactionKey]chan *sipResponse), smsContactConfirmed: true, evidence: vowifi.IMSEvidence{Registered: true}}
	req := &sipRequest{Headers: map[string][]string{"from": {"<sip:gateway@example.com>"}}}
	s.notifySMSMemoryAvailable(req)
	s.notifySMSMemoryAvailable(req)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("notice not sent")
	}
	if count.Load() != 1 {
		t.Fatal("duplicate notice")
	}
}
