package hardware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func eapHex(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic(err)
	}
	return b
}

func TestAKAPRFPublishedVector(t *testing.T) {
	// RFC 4186 A.5 shares the RFC 4187 FIPS-186-2 expansion.
	got := akaPRF(eapHex("e576d5ca332e9930018bf1baee2763c795b3c712"))
	want := eapHex(`536e5ebc4465582aa6a8ec9986ebb620 25af1942efcbf4bc72b3943421f2a974
	39d45aeaf4e30601983e972b6cfd46d1 c363773365690d09cd44976b525f47d3
	a60a985e955c53b090b2e4b73719196a 402542968fd14a888f46b9a7886e4488
	5949eab0fff69d52315c6c634fd14a7f 0d52023d56f79698fa6596abeed4f93f
	bb48eb534d985414ceed0d9a8ed33c38 7c9dfdab92ffbdf240fcecf65a2c93b9`)
	if !bytes.Equal(got[:], want) {
		t.Fatal("PRF differs from published vector")
	}
}

func eapFixture() (*wifiEAP, *int) {
	calls := new(int)
	e, _ := newWiFiEAP(&wifiSIM{id: wifiIdentity{IMSI: "001010123456789", MCC: "001", MNC: "01"}})
	e.authenticate = func(_ context.Context, rand, autn []byte) (akaResult, error) {
		*calls++
		if !bytes.Equal(rand, bytes.Repeat([]byte{3}, 16)) || !bytes.Equal(autn, bytes.Repeat([]byte{4}, 16)) {
			return akaResult{}, errors.New("bad test challenge")
		}
		return akaResult{RES: bytes.Repeat([]byte{5}, 8), IK: bytes.Repeat([]byte{1}, 16), CK: bytes.Repeat([]byte{2}, 16)}, nil
	}
	return e, calls
}

func fixtureKeys(identity string) [160]byte {
	h := sha1.New()
	h.Write([]byte(identity))
	h.Write(bytes.Repeat([]byte{1}, 16))
	h.Write(bytes.Repeat([]byte{2}, 16))
	return akaPRF(h.Sum(nil))
}

func requestAKA(id, subtype byte, attrs ...[]byte) []byte {
	packet := akaPacket(id, subtype, bytes.Join(attrs, nil))
	packet[0] = 1
	return packet
}

func challengeAKA(e *wifiEAP, id byte, extra ...[]byte) []byte {
	attrs := [][]byte{akaAttribute(atRAND, 0, bytes.Repeat([]byte{3}, 16)), akaAttribute(atAUTN, 0, bytes.Repeat([]byte{4}, 16))}
	attrs = append(attrs, extra...)
	attrs = append(attrs, akaAttribute(atMAC, 0, make([]byte, 16)))
	packet := requestAKA(id, akaChallenge, attrs...)
	keys := fixtureKeys(e.identity)
	if _, err := parseAKAAttributes(packet); err == nil {
		signAKA(packet, keys[16:32])
	}
	return packet
}

func TestWiFiEAPChallengeSuccessAndCleanup(t *testing.T) {
	e, calls := eapFixture()
	if _, err := e.masterKey(); err == nil {
		t.Fatal("key before authentication")
	}
	req := challengeAKA(e, 7)
	reply, err := e.handle(context.Background(), req)
	if err != nil || !e.verified || e.complete || *calls != 1 {
		t.Fatal("challenge did not verify", err)
	}
	attrs, _ := parseAKAAttributes(reply)
	if binary.BigEndian.Uint16(attrs[atRES].data[2:4]) != 64 || !bytes.Equal(attrs[atRES].data[4:], bytes.Repeat([]byte{5}, 8)) || !validAKAMAC(reply, attrs[atMAC], e.keys[16:32]) {
		t.Fatal("incorrect response")
	}
	second, err := e.handle(context.Background(), req)
	if err != nil || *calls != 1 || !bytes.Equal(reply, second) {
		t.Fatal("retransmit reauthenticated SIM")
	}
	second[0] = 99
	third, _ := e.handle(context.Background(), req)
	if third[0] != 2 {
		t.Fatal("cached response is aliased")
	}
	if _, err = e.masterKey(); err == nil {
		t.Fatal("challenge mistaken for EAP Success")
	}
	if _, err = e.handle(context.Background(), []byte{3, 7, 0, 4}); err != nil {
		t.Fatal(err)
	}
	key, err := e.masterKey()
	if err != nil || len(key) != 64 {
		t.Fatal("missing MSK")
	}
	key[0] ^= 255
	if key[0] == e.keys[32] {
		t.Fatal("MSK aliases session")
	}
	cached := e.response
	e.close()
	if !bytes.Equal(e.keys[:], make([]byte, 160)) || !bytes.Equal(cached, make([]byte, len(cached))) || e.complete || e.authenticate != nil {
		t.Fatal("secrets retained on close")
	}
	if _, err = e.masterKey(); err == nil {
		t.Fatal("closed key returned")
	}
}

func TestWiFiEAPIdentityAndCheckcode(t *testing.T) {
	e, calls := eapFixture()
	plain := []byte{1, 1, 0, 5, 1}
	reply, err := e.handle(context.Background(), plain)
	if err != nil || string(reply[5:]) != e.identity {
		t.Fatal("outer identity")
	}
	id := requestAKA(2, akaIdentity, akaAttribute(atAnyID, 0, nil))
	reply, err = e.handle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	attrs, _ := parseAKAAttributes(reply)
	if string(attrs[atIdentity].data[4:4+len(e.identity)]) != e.identity || *calls != 0 {
		t.Fatal("AKA identity")
	}
	transcript := append(bytes.Clone(id), reply...)
	if !bytes.Equal(transcript, e.transcript) {
		t.Fatal("incorrect identity transcript")
	}
	digest := sha1.Sum(transcript)
	reply, err = e.handle(context.Background(), challengeAKA(e, 3, akaAttribute(atCheckcode, 0, digest[:])))
	if err != nil {
		t.Fatal(err)
	}
	attrs, _ = parseAKAAttributes(reply)
	if !bytes.Equal(attrs[atCheckcode].data[4:], digest[:]) {
		t.Fatal("checkcode not echoed")
	}
}

func TestWiFiEAPProtectedResult(t *testing.T) {
	for _, valid := range []bool{true, false} {
		e, _ := eapFixture()
		if _, err := e.handle(context.Background(), challengeAKA(e, 1, akaAttribute(atResult, 0, nil))); err != nil {
			t.Fatal(err)
		}
		if !valid {
			if _, err := e.handle(context.Background(), []byte{3, 1, 0, 4}); err == nil {
				t.Fatal("skipped protected result")
			}
			continue
		}
		note := requestAKA(2, akaNotification, akaAttribute(atNotification, 32768, nil), akaAttribute(atMAC, 0, make([]byte, 16)))
		signAKA(note, e.keys[16:32])
		reply, err := e.handle(context.Background(), note)
		if err != nil || e.resultPending || e.complete {
			t.Fatal("result confirmation", err)
		}
		attrs, _ := parseAKAAttributes(reply)
		if !validAKAMAC(reply, attrs[atMAC], e.keys[16:32]) {
			t.Fatal("unprotected result response")
		}
		if _, err = e.handle(context.Background(), []byte{3, 2, 0, 4}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWiFiEAPRejectsInvalidPacketsAndClearsKeys(t *testing.T) {
	cases := map[string]func(*wifiEAP) []byte{
		"premature-success": func(e *wifiEAP) []byte { return []byte{3, 1, 0, 4} },
		"failure":           func(e *wifiEAP) []byte { return []byte{4, 1, 0, 4} },
		"wrong-length":      func(e *wifiEAP) []byte { p := challengeAKA(e, 1); p[3]--; return p },
		"duplicate-rand":    func(e *wifiEAP) []byte { return challengeAKA(e, 1, akaAttribute(atRAND, 0, make([]byte, 16))) },
		"mandatory-unknown": func(e *wifiEAP) []byte { return challengeAKA(e, 1, akaAttribute(99, 0, nil)) },
		"bad-mac":           func(e *wifiEAP) []byte { p := challengeAKA(e, 1); p[len(p)-1] ^= 1; return p },
		"bad-checkcode":     func(e *wifiEAP) []byte { return challengeAKA(e, 1, akaAttribute(atCheckcode, 0, make([]byte, 20))) },
		"bad-result-length": func(e *wifiEAP) []byte { return challengeAKA(e, 1, akaAttribute(atResult, 0, make([]byte, 4))) },
		"zero-attribute":    func(e *wifiEAP) []byte { p := challengeAKA(e, 1); p[9] = 0; return p },
		"large-attribute":   func(e *wifiEAP) []byte { p := challengeAKA(e, 1); p[9] = 255; return p },
		"identity-conflict": func(e *wifiEAP) []byte {
			return requestAKA(1, akaIdentity, akaAttribute(atAnyID, 0, nil), akaAttribute(atFullID, 0, nil))
		},
		"reauth-unsupported": func(e *wifiEAP) []byte { return requestAKA(1, 13) },
	}
	for name, makePacket := range cases {
		t.Run(name, func(t *testing.T) {
			e, calls := eapFixture()
			packet := makePacket(e)
			if reply, err := e.handle(context.Background(), packet); err == nil || reply != nil || !e.closed || !bytes.Equal(e.keys[:], make([]byte, 160)) {
				t.Fatal("invalid packet accepted")
			}
			if name != "bad-mac" && *calls != 0 {
				t.Fatal("malformed packet reached SIM")
			}
		})
	}
	for _, kind := range []string{"wrong-success-id", "changed-retransmit", "new-challenge", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := eapFixture()
			packet := challengeAKA(e, 8)
			_, err := e.handle(context.Background(), packet)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "wrong-success-id":
				packet = []byte{3, 9, 0, 4}
			case "changed-retransmit":
				packet[len(packet)-1] ^= 1
			case "new-challenge":
				packet = challengeAKA(e, 9)
			case "cancelled":
				cancel()
			}
			if _, err = e.handle(ctx, packet); err == nil || !e.closed {
				t.Fatal("invalid transition accepted")
			}
		})
	}
}

func TestWiFiEAPSyncFailureAndBounds(t *testing.T) {
	e, _ := eapFixture()
	e.authenticate = func(context.Context, []byte, []byte) (akaResult, error) {
		return akaResult{AUTS: bytes.Repeat([]byte{7}, 14)}, nil
	}
	for id := byte(1); id <= 3; id++ {
		reply, err := e.handle(context.Background(), challengeAKA(e, id))
		if err != nil {
			t.Fatal(err)
		}
		if e.verified || reply[5] != 4 || !bytes.Equal(reply[8:], append([]byte{atAUTS, 4}, bytes.Repeat([]byte{7}, 14)...)) {
			t.Fatal("AUTS encoded as success")
		}
	}
	if _, err := e.handle(context.Background(), challengeAKA(e, 4)); err == nil {
		t.Fatal("unbounded SIM authentication")
	}
	for _, result := range []akaResult{{}, {RES: make([]byte, 3), CK: make([]byte, 16), IK: make([]byte, 16)}, {AUTS: make([]byte, 14), CK: make([]byte, 16)}} {
		e, _ := eapFixture()
		e.authenticate = func(context.Context, []byte, []byte) (akaResult, error) { return result, nil }
		if _, err := e.handle(context.Background(), challengeAKA(e, 1)); err == nil {
			t.Fatal("malformed SIM result accepted")
		}
	}
}

type eapSIMTranscript struct {
	wifiTranscript
	authCalls int
}

func (p *eapSIMTranscript) Write(b []byte) (int, error) {
	command := strings.TrimSpace(string(b))
	if strings.HasPrefix(command, `AT+CSIM=80,"018800812210`) {
		p.commands = append(p.commands, command)
		p.authCalls++
		data := append([]byte{0xdb, 8}, bytes.Repeat([]byte{5}, 8)...)
		data = append(data, 16)
		data = append(data, bytes.Repeat([]byte{2}, 16)...)
		data = append(data, 16)
		data = append(data, bytes.Repeat([]byte{1}, 16)...)
		data = append(data, 0x90, 0)
		p.response = fmt.Sprintf("+CSIM: %d,\"%X\"\r\nOK\r\n", len(data)*2, data)
		return len(b), nil
	}
	return p.wifiTranscript.Write(b)
}

func TestWiFiEAPUsesExistingBoundSIMChannel(t *testing.T) {
	p := &eapSIMTranscript{}
	sim, err := inspectWiFiSIM(context.Background(), &atSession{port: p}, "89123456789012345678")
	if err != nil {
		t.Fatal(err)
	}
	defer sim.close()
	e, err := newWiFiEAP(sim)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	before := p.iccidReads
	if _, err = e.handle(context.Background(), challengeAKA(e, 1)); err != nil {
		t.Fatal(err)
	}
	if p.authCalls != 1 || p.iccidReads-before != 2 {
		t.Fatal("SIM binding was not checked around AKA")
	}
	for _, cmd := range p.commands {
		if strings.Contains(cmd, "CFUN=") || strings.Contains(cmd, "CGACT=") {
			t.Fatal("EAP modified radio")
		}
	}
}

func TestWiFiEAPRejectsEveryTruncation(t *testing.T) {
	e, _ := eapFixture()
	full := challengeAKA(e, 1)
	for n := 0; n < len(full); n++ {
		e, _ = eapFixture()
		if _, err := e.handle(context.Background(), full[:n]); err == nil {
			t.Fatal("truncated packet accepted")
		}
	}
}

func FuzzWiFiEAPPacket(f *testing.F) {
	e, _ := eapFixture()
	f.Add(challengeAKA(e, 1))
	f.Add([]byte{3, 1, 0, 4})
	f.Fuzz(func(t *testing.T, packet []byte) {
		e, _ := eapFixture()
		defer e.close()
		_, _ = e.handle(context.Background(), packet)
	})
}

func TestWiFiEAPMACCoversWholePacket(t *testing.T) {
	e, _ := eapFixture()
	packet := challengeAKA(e, 1, akaAttribute(200, 0, []byte{1, 2, 3, 4}))
	attrs, _ := parseAKAAttributes(packet)
	mac := attrs[atMAC]
	copyPacket := bytes.Clone(packet)
	clear(copyPacket[mac.offset+4 : mac.offset+20])
	keys := fixtureKeys(e.identity)
	h := hmac.New(sha1.New, keys[16:32])
	h.Write(copyPacket)
	if !hmac.Equal(mac.data[4:], h.Sum(nil)[:16]) {
		t.Fatal("MAC does not cover full EAP packet")
	}
	if _, err := e.handle(context.Background(), packet); err != nil {
		t.Fatal("skippable extension rejected", err)
	}
}

func TestWiFiEAPRejectsInvalidIdentity(t *testing.T) {
	for _, sim := range []*wifiSIM{nil, {}, {id: wifiIdentity{IMSI: "001010123456789", MCC: "002", MNC: "01"}}} {
		if _, err := newWiFiEAP(sim); err == nil {
			t.Fatal("invalid subscriber identity")
		}
	}
}

func TestWiFiEAPIdentityRoundLimit(t *testing.T) {
	e, _ := eapFixture()
	if _, err := e.handle(context.Background(), []byte{1, 0, 0, 5, 1}); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []byte{atAnyID, atFullID, atPermanentID} {
		if _, err := e.handle(context.Background(), requestAKA(byte(i+1), akaIdentity, akaAttribute(kind, 0, nil))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.handle(context.Background(), requestAKA(4, akaIdentity, akaAttribute(atPermanentID, 0, nil))); err == nil {
		t.Fatal("unbounded identity rounds")
	}
}

func TestWiFiEAPRejectsForgedProtectedResult(t *testing.T) {
	for _, mode := range []string{"mac", "failure", "unprotected", "before-challenge"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := eapFixture()
			if mode != "before-challenge" {
				if _, err := e.handle(context.Background(), challengeAKA(e, 1, akaAttribute(atResult, 0, nil))); err != nil {
					t.Fatal(err)
				}
			}
			code := uint16(32768)
			if mode == "failure" {
				code = 0
			}
			if mode == "unprotected" {
				code = 49152
			}
			packet := requestAKA(2, akaNotification, akaAttribute(atNotification, code, nil), akaAttribute(atMAC, 0, make([]byte, 16)))
			signAKA(packet, e.keys[16:32])
			if mode == "mac" {
				packet[len(packet)-1] ^= 1
			}
			if _, err := e.handle(context.Background(), packet); err == nil || !e.closed {
				t.Fatal("forged success accepted")
			}
		})
	}
}
