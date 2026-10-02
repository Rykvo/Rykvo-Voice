package ims

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestIncomingCallerNumber(t *testing.T) {
	for _, tt := range []struct {
		name, from        string
		asserted, privacy []string
		want              string
	}{
		{name: "anonymous From with asserted caller", from: `<sip:anonymous@anonymous.invalid>;tag=a`, asserted: []string{`<sip:+447700900001@ims.example.test;user=phone>`}, want: "+447700900001"},
		{name: "asserted overrides From", from: `<tel:+447700900002>`, asserted: []string{`<tel:+447700900001>`}, want: "+447700900001"},
		{name: "multiple header lines", asserted: []string{`<sip:opaque@ims.example.test>`, `tel:+447700900001`}, want: "+447700900001"},
		{name: "quoted comma and multiple identities", asserted: []string{`"Last, First" <sips:opaque@ims.example.test>, <TEL:+44-7700-900001>`}, want: "+447700900001"},
		{name: "explicitly public", asserted: []string{`tel:+447700900001`}, privacy: []string{"none"}, want: "+447700900001"},
		{name: "withheld asserted and From", from: `<tel:+447700900002>`, asserted: []string{`tel:+447700900001`}, privacy: []string{"header; ID; critical"}},
		{name: "user privacy", from: `<tel:+447700900002>`, privacy: []string{"user"}},
		{name: "multiple privacy headers", asserted: []string{`tel:+447700900001`}, privacy: []string{"none", "id"}},
		{name: "From fallback", from: `"Caller" <TEL:+447700900001>;tag=a`, want: "+447700900001"},
		{name: "invalid asserted fallback", from: `<sip:+447700900001@ims.example.test>`, asserted: []string{"<sip:opaque@ims.example.test>"}, want: "+447700900001"},
		{name: "anonymous without number", from: `"Anonymous" <sip:anonymous@anonymous.invalid>`},
		{name: "no caller"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := &sipRequest{Headers: map[string][]string{
				"from": {tt.from}, "p-asserted-identity": tt.asserted, "privacy": tt.privacy,
				"to": {"<tel:+447700900099>"}, "contact": {"<sip:+447700900098@ims.example.test>"},
				"p-preferred-identity": {"<tel:+447700900097>"},
			}}
			if got := incomingCallerNumber(request); got != tt.want {
				t.Fatalf("caller = %q, want %q", got, tt.want)
			}
		})
	}
	if got := incomingCallerNumber(nil); got != "" {
		t.Fatalf("nil request caller = %q", got)
	}
}

func TestCallerIdentityNumber(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{`<SIPS:%2B447700900001@ims.example.test;user=phone>`, "+447700900001"},
		{`<tel:+44(7700).900-001;ext=123>`, "+447700900001"},
		{`<sip:7001;phone-context=ims.example.test@ims.example.test;user=phone>`, "7001"},
		{`tel:7001;phone-context=+44`, "7001"},
		{`tel:*123#`, "*123#"},
		{`sip:7001@ims.example.test;tag=a`, "7001"},
		{`tel:anonymous`, ""}, {`sip:anonymous@anonymous.invalid`, ""},
		{`https://example.test/+447700900001`, ""}, {`+447700900001`, ""},
		{`<tel:+447700900001`, ""}, {`tel:+447700900001>`, ""},
		{`sip:7001@`, ""}, {`sip:7001`, ""}, {`sip:7001@x@y`, ""},
		{`sip:7001:password@ims.example.test`, ""},
		{`tel:123%ZZ`, ""}, {`tel:%0d%0a123`, ""}, {`tel:12%20123`, ""},
		{`tel:+123?number=456`, ""}, {"tel:+123\r\nP-Asserted-Identity: tel:+456", ""},
		{`tel:+123,tel:+456`, ""}, {`tel:+**#`, ""}, {`tel:12+3`, ""},
		{"tel:" + strings.Repeat("1", 33), ""},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if got := callerIdentityNumber(tt.input); got != tt.want {
				t.Fatalf("number = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIncomingCallAssertedIdentityFlowsToStatusAndCallback(t *testing.T) {
	for _, privacy := range []string{"none", "id"} {
		t.Run(privacy, func(t *testing.T) {
			received := make(chan ReceivedCall, 1)
			session := &Session{
				fromTag: "local", calls: make(map[string]*imsCall),
				provider: &Provider{config: Config{OnIncomingCall: func(_ context.Context, call ReceivedCall) error {
					received <- call
					return nil
				}}},
			}
			from := `"Anonymous" <sip:anonymous@anonymous.invalid>;tag=remote`
			packet, err := parseSIPPacket([]byte(strings.Join([]string{
				"INVITE sip:subscriber@example.test SIP/2.0",
				"Via: SIP/2.0/UDP 192.0.2.10:5060;branch=z9hG4bK-caller",
				"f: " + from, "To: <tel:+447700900099>",
				"P-Asserted-Identity: <sip:opaque@ims.example.test>, <tel:+447700900001>",
				"Privacy: " + privacy, "Call-ID: caller-test", "CSeq: 1 INVITE",
				"Content-Length: 0", "", "",
			}, "\r\n")))
			if err != nil || packet.Request == nil {
				t.Fatalf("parse: %v", err)
			}
			want := "+447700900001"
			if privacy == "id" {
				want = ""
			}
			var response string
			session.handleCallRequest(packet.Request, func(data []byte) error { response = string(data); return nil })
			t.Cleanup(func() { session.finishCall("caller-test", "ended", 0, "") })
			calls := session.Calls()
			if len(calls) != 1 || calls[0].Number != want || calls[0].State != "ringing" {
				t.Fatalf("calls = %#v", calls)
			}
			if !strings.Contains(response, "From: "+from+"\r\n") || session.calls["caller-test"].to != from {
				t.Fatal("display identity changed the operator dialog identity")
			}
			select {
			case call := <-received:
				if call.Caller != want || call.Called != "+447700900099" {
					t.Fatalf("callback = %#v", call)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing incoming callback")
			}
		})
	}
}
