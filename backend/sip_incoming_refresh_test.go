package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestSIPGatewayIncomingRefreshStaysInDialog(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			c, phones, device, call, done := incomingWireSetup(t, transport, 1)
			p := phones[0]
			p.answer(t)
			if r, ok := p.read(t).(*sip.Request); !ok || r.Method != sip.ACK {
				t.Fatal("answer ACK missing")
			}
			if !call.legs[p.reg.ID].selected.Load() {
				t.Fatal("ACK exposed a dialog before winner selection")
			}
			send := func(method string, seq int, body, tag string) {
				branch := method
				if method == "ACK" && seq != 1 {
					branch = "INVITE"
				}
				wire := fmt.Sprintf("%s %s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=z9hG4bKrefresh%d%s\r\nFrom: %s;tag=phone-%s\r\nTo: %s%s\r\nCall-ID: %s\r\nCSeq: %d %s\r\nContact: <sip:phone@192.0.2.88:9999>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", method, p.invite.Contact().Address.String(), strings.ToUpper(transport), p.conn.LocalAddr(), seq, branch, p.invite.To().Value(), p.id, p.invite.From().Value(), tag, p.invite.CallID().Value(), seq, method, len(body), body)
				m, err := sip.ParseMessage([]byte(wire))
				if err != nil {
					t.Fatal(err)
				}
				p.write(t, m)
			}
			body := fmt.Sprintf("v=0\r\nc=IN IP4 192.0.2.88\r\nm=audio %d RTP/AVP 8 0 97 99\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:97 telephone-event/8000\r\na=rtpmap:99 AMR/8000\r\na=sendrecv\r\n", p.media.LocalAddr().(*net.UDPAddr).Port)
			readCode := func(want int) {
				t.Helper()
				r, ok := p.read(t).(*sip.Response)
				if !ok || r.StatusCode != want {
					t.Fatalf("refresh want %d got %v", want, r)
				}
			}
			send("INVITE", 1, body, "")
			readCode(200)
			send("INVITE", 2, body, "")
			readCode(491)
			send("ACK", 2, "", "")
			send("ACK", 1, "", "")
			leg := call.legs[p.reg.ID]
			until := time.Now().Add(time.Second)
			for time.Now().Before(until) {
				leg.mu.Lock()
				pending := leg.refreshACK != nil
				leg.mu.Unlock()
				if !pending {
					break
				}
				time.Sleep(time.Millisecond)
			}
			send("INVITE", 3, strings.Replace(body, "RTP/AVP 8 0 97 99", "RTP/AVP 99", 1), "")
			readCode(488)
			send("ACK", 3, "", "")
			send("INVITE", 4, body, ";tag=wrong")
			readCode(481)
			send("ACK", 4, "", ";tag=wrong")
			until = time.Now().Add(2 * time.Second)
			for !call.connected.Load() && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if !call.connected.Load() || device.answers.Load() != 1 {
				t.Fatal("refresh redialed or stopped carrier")
			}
			call.recordsMu.Lock()
			count := len(call.records)
			call.recordsMu.Unlock()
			if count != 1 {
				t.Fatal("refresh created another history record", count)
			}
			device.state.Store(2)
			bye, ok := p.read(t).(*sip.Request)
			if !ok || bye.Method != sip.BYE {
				t.Fatal("remote hangup not delivered", bye)
			}
			p.response(t, bye, 200, nil)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("refresh leaked call")
			}
			if _, exists := c.router.Snapshot(call.call.ID); exists {
				t.Fatal("refresh leaked reservation")
			}
		})
	}
}
