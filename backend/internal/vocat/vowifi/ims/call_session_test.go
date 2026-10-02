package ims

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestIncomingSessionTimerAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, offer, supported, want string
		require                      bool
	}{
		{"carrier_default", "1800", "histinfo,100rel,timer", "1800;refresher=uas", true},
		{"caller_refreshes", "1800;refresher=uac", "timer", "1800;refresher=uac", true},
		{"callee_refreshes", "900;refresher=uas", "timer", "900;refresher=uas", true},
		{"legacy_caller", "1800", "100rel", "1800;refresher=uas", false},
		{"no_timer", "", "timer", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"INVITE", "UPDATE"} {
				r := &sipRequest{Method: method, Headers: map[string][]string{
					"via":  {"SIP/2.0/TCP [2001:db8::1]:6060;branch=z9hG4bKtimer"},
					"from": {"<sip:caller@example.test>;tag=remote"}, "to": {"<sip:callee@example.test>"},
					"call-id": {"timer-answer"}, "cseq": {"1 " + method},
					"session-expires": {tc.offer}, "supported": {tc.supported},
				}}
				wire, err := buildSIPResponseWithBody(r, 200, "local", nil)
				if err != nil {
					t.Fatal(err)
				}
				packet, err := parseSIPPacket(wire)
				if err != nil {
					t.Fatal(err)
				}
				if got := packet.Response.value("Session-Expires"); got != tc.want {
					t.Fatalf("%s timer=%q want %q", method, got, tc.want)
				}
				if got := packet.Response.value("Require") == "timer"; got != tc.require {
					t.Fatalf("%s requires timer=%t", method, got)
				}
			}
		})
	}
}

func TestSessionTimerRoleAndSchedule(t *testing.T) {
	for _, tc := range []struct {
		header       string
		uac, refresh bool
		seconds      int
		delay        time.Duration
	}{
		{"1800;refresher=uas", false, true, 1800, 900 * time.Second},
		{"1800;refresher=uac", false, false, 1800, 1768 * time.Second},
		{"1800;refresher=uas", true, false, 1800, 1768 * time.Second},
		{"1800;refresher=uac", true, true, 1800, 900 * time.Second},
		{"90;refresher=uac", false, false, 90, 60 * time.Second},
		{"1800", true, true, 1800, 900 * time.Second},
		{"", true, false, 0, 0}, {"89", true, false, 0, 0},
		{"86401", true, false, 0, 0}, {"1800;refresher=other", true, false, 0, 0},
	} {
		seconds, refresh, delay := sessionTimerSchedule(tc.header, tc.uac)
		if seconds != tc.seconds || refresh != tc.refresh || delay != tc.delay {
			t.Fatalf("%+v: got %d %t %v", tc, seconds, refresh, delay)
		}
	}
}

func TestSessionTimerRefreshExpiryAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		refresh, stale, cancelled, noTimer bool
		updateCode, byeCode                int
		methods                            []string
		ended, ending                      bool
		seconds                            int
	}{
		{name: "refresh", refresh: true, updateCode: 200, methods: []string{"UPDATE"}, seconds: 900},
		{name: "refresh_removes_timer", refresh: true, updateCode: 200, noTimer: true, methods: []string{"UPDATE"}},
		{name: "peer_expired", byeCode: 200, methods: []string{"BYE"}, ended: true, ending: true, seconds: 1800},
		{name: "refresh_failed", refresh: true, updateCode: 500, byeCode: 200, methods: []string{"UPDATE", "BYE"}, ended: true, ending: true, seconds: 1800},
		{name: "unconfirmed_end", byeCode: 500, methods: []string{"BYE"}, ending: true, seconds: 1800},
		{name: "stale_timer", stale: true, seconds: 1800},
		{name: "cancelled", cancelled: true, seconds: 1800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &identityTestConn{}
			call := &imsCall{callID: "timer-call", target: "sip:remote@example.test", from: "<sip:local@example.test>;tag=local", to: "<sip:remote@example.test>;tag=remote", accepted: true, sessionGeneration: 1, sessionExpires: 1800, public: vowifi.Call{ID: "timer-call", Direction: "incoming", State: "active"}}
			s := &Session{identity: identitySet{user: "fixture"}, conn: conn, transport: "tcp", fromTag: "local", cseq: 2, refreshContext: ctx, provider: &Provider{config: Config{TransactionTimeout: time.Second}}, transactions: map[sipTransactionKey]chan *sipResponse{}, calls: map[string]*imsCall{"timer-call": call}}
			defer s.finishCall(call.callID, "ended", 0, "")
			var methods []string
			conn.onWrite = func(wire []byte) (int, error) {
				packet, err := parseSIPPacket(wire)
				if err != nil || packet.Request == nil {
					return 0, fmt.Errorf("invalid request: %v", err)
				}
				r := packet.Request
				methods = append(methods, r.Method)
				code := tc.byeCode
				headers := map[string][]string{"call-id": {r.value("Call-ID")}, "cseq": {r.value("CSeq")}}
				if r.Method == "UPDATE" {
					code = tc.updateCode
					if r.value("Session-Expires") != "1800;refresher=uac" {
						t.Error("refresh request has wrong role")
					}
					if !tc.noTimer {
						headers["session-expires"] = []string{"900;refresher=uas"}
					}
				}
				s.dispatchPacket(sipPacket{Response: &sipResponse{StatusCode: code, Headers: headers}}, nil)
				return len(wire), nil
			}
			if tc.stale {
				call.sessionGeneration = 2
			}
			if tc.cancelled {
				cancel()
			}
			s.runSessionTimer(ctx, call, 1, time.Millisecond, tc.refresh)
			s.callMu.Lock()
			ended, ending, seconds := call.public.EndedAt != nil, call.terminated, call.sessionExpires
			s.callMu.Unlock()
			if !slices.Equal(methods, tc.methods) || ended != tc.ended || ending != tc.ending || seconds != tc.seconds {
				t.Fatalf("methods=%v ended=%t ending=%t seconds=%d", methods, ended, ending, seconds)
			}
		})
	}
}

type dialogContactTestConn struct {
	fakeConn
	address net.Addr
}

func (c *dialogContactTestConn) LocalAddr() net.Addr { return c.address }

func TestDialogContactUsesNegotiatedTransport(t *testing.T) {
	for _, host := range []string{"192.0.2.1", "2001:db8::1"} {
		for _, transport := range []string{"tcp", "udp"} {
			for _, gsma := range []bool{true, false} {
				s := &Session{identity: identitySet{user: "fixture"}, transport: transport,
					conn:             &dialogContactTestConn{address: &net.TCPAddr{IP: net.ParseIP(host), Port: 22029}},
					provider:         &Provider{config: Config{SecurityMode: SecurityRequired}},
					securityProposal: securityProposal{portServer: 21059}, instanceID: "urn:uuid:fixture"}
				if gsma {
					s.request.Identity = vowifi.SIMIdentity{HomeMCC: "234", HomeMNC: "10", IMSI: "234100000000001", GID1: "508FFFFF"}
				}
				contact := s.dialogContactHeader()
				want := net.JoinHostPort(host, "21059") + ";transport=" + transport + ">"
				if !strings.Contains(contact, want) {
					t.Fatalf("gsma=%t contact=%s missing %s", gsma, contact, want)
				}
				if gsma && strings.Contains(contact, "fixture@") {
					t.Fatal("GSMA contact gained a subscriber identity")
				}
				if !strings.Contains(contact, ";+sip.instance=") || !strings.Contains(contact, mmtelFeatureTag) {
					t.Fatal("contact lost device or service tags")
				}
			}
		}
	}
}
