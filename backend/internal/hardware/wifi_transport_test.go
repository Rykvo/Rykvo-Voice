package hardware

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"net"
	"testing"
)

func TestWiFiPermanentIdentity(t *testing.T) {
	for _, tc := range []struct{ imsi, mcc, mnc, want string }{
		{"310240000000001", "310", "240", "0310240000000001@nai.epc.mnc240.mcc310.3gppnetwork.org"},
		{"234100000000001", "234", "10", "0234100000000001@nai.epc.mnc010.mcc234.3gppnetwork.org"},
	} {
		e, err := newWiFiEAP(&wifiSIM{id: wifiIdentity{IMSI: tc.imsi, MCC: tc.mcc, MNC: tc.mnc}})
		if err != nil || e.identity != tc.want {
			t.Fatalf("identity mismatch: %v", err)
		}
		e.close()
	}
}
func TestWiFiDigestVector(t *testing.T) {
	got := wifiDigest("Mufasa", "testrealm@host.com", "dcd98b7102dd2f0e8b11d0f600bfb0c093", "/dir/index.html", "GET", "auth", "0a4f113b", 1, []byte("Circle Of Life"))
	if got != "6629fae49393a05397450978507c4ef1" {
		t.Fatalf("digest %s", got)
	}
	if wifiDigest("a", "b", "c", "d", "REGISTER", "", "", 1, []byte{0, 255}) == wifiDigest("a", "b", "c", "d", "REGISTER", "", "", 1, []byte("00ff")) {
		t.Fatal("RES was hex encoded")
	}
}
func TestWiFiParameters(t *testing.T) {
	p, err := wifiParameters("realm=\"a,b\", nonce=\"x\\\"y\", qop=\"auth,auth-int\"", ',')
	if err != nil || p["realm"] != "a,b" || p["nonce"] != "x\"y" || p["qop"] != "auth,auth-int" {
		t.Fatal("quoted parameters")
	}
	for _, s := range []string{"nonce=x,nonce=y", "nonce=\"unterminated", "x=1,", "x=\"ok\"trailing", "bad header=x", "x=ok\r\ny=no"} {
		if _, e := wifiParameters(s, ','); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
func espPair(sha bool) (*wifiChild, *wifiChild) {
	a := &wifiChild{spiIn: 1024, spiOut: 2048, encIn: bytes.Repeat([]byte{1}, 16), encOut: bytes.Repeat([]byte{2}, 16), authIn: bytes.Repeat([]byte{3}, 32), authOut: bytes.Repeat([]byte{4}, 32), mac: sha256.New, macLen: 16}
	if sha {
		a.mac = sha1.New
		a.macLen = 12
	}
	b := &wifiChild{spiIn: a.spiOut, spiOut: a.spiIn, encIn: bytes.Clone(a.encOut), encOut: bytes.Clone(a.encIn), authIn: bytes.Clone(a.authOut), authOut: bytes.Clone(a.authIn), mac: a.mac, macLen: a.macLen}
	return a, b
}
func TestWiFiESPIntegrityAndReplay(t *testing.T) {
	for _, sha := range []bool{false, true} {
		a, b := espPair(sha)
		p1, _ := a.seal([]byte("first"), 17)
		p2, _ := a.seal([]byte("second"), 17)
		corrupt := bytes.Clone(p2)
		corrupt[25] ^= 1
		if _, _, e := b.open(corrupt); e == nil || b.receiveSeq != 0 {
			t.Fatal("unauthenticated packet accepted or advanced replay")
		}
		for _, tc := range []struct {
			p []byte
			s string
		}{{p2, "second"}, {p1, "first"}} {
			v, next, e := b.open(tc.p)
			if e != nil || next != 17 || string(v) != tc.s {
				t.Fatal("valid packet rejected")
			}
		}
		if _, _, e := b.open(p1); e == nil {
			t.Fatal("replay accepted")
		}
		for i := 0; i < len(p2); i++ {
			if _, _, e := b.open(p2[:i]); e == nil {
				t.Fatal("truncated packet accepted")
			}
		}
		a.sendSeq = ^uint32(0)
		if _, e := a.seal(nil, 17); e == nil {
			t.Fatal("sequence rollover")
		}
		a.close()
		b.close()
	}
}
func TestWiFiUDPChecksums(t *testing.T) {
	for _, pair := range [][2]string{{"192.0.2.1", "192.0.2.2"}, {"2001:db8::1", "2001:db8::2"}} {
		src, dst := net.ParseIP(pair[0]), net.ParseIP(pair[1])
		p, next, e := wifiUDP(src, dst, 20000, 30000, []byte("SIP"))
		if e != nil {
			t.Fatal(e)
		}
		v, e := wifiReadUDP(p, next, dst, src, 30000, 20000)
		if e != nil || string(v) != "SIP" {
			t.Fatal("roundtrip", e)
		}
		if _, e = wifiReadUDP(p, next, dst, src, 30001, 20000); e == nil {
			t.Fatal("wrong destination")
		}
		if _, e = wifiReadUDP(p, next, dst, src, 30000, 20001); e == nil {
			t.Fatal("wrong source")
		}
		if _, e = wifiReadUDP(p, next, dst, src, 30000, 0); e != nil {
			t.Fatal("protected arbitrary peer source")
		}
		p[len(p)-1] ^= 1
		if _, e = wifiReadUDP(p, next, dst, src, 30000, 0); e == nil {
			t.Fatal("bad checksum")
		}
	}
}
func TestWiFiProtectedUDPServerAssociation(t *testing.T) {
	outer, peerOuter := espPair(false)
	inner, peerInner := espPair(true)
	local, peer := net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::2")
	udp, kind, _ := wifiUDP(peer, local, 50000, 49162, []byte("response"))
	_, _, _, body, _ := wifiParseIP(udp, kind)
	esp, _ := peerInner.seal(body, 17)
	ip, kind, _ := wifiIP(peer, local, 50, esp)
	wire, _ := peerOuter.seal(ip, kind)
	plain, kind, e := outer.open(wire)
	if e != nil {
		t.Fatal(e)
	}
	_, _, proto, body, e := wifiParseIP(plain, kind)
	if e != nil || proto != 50 {
		t.Fatal("outer IP")
	}
	body, proto, e = inner.open(body)
	if e != nil || proto != 17 {
		t.Fatal("inner ESP")
	}
	ip, kind, _ = wifiIP(peer, local, 17, body)
	v, e := wifiReadUDP(ip, kind, local, peer, 49162, 0)
	if e != nil || string(v) != "response" {
		t.Fatal("UDP must receive on protected server port", e)
	}
	if _, e = wifiReadUDP(ip, kind, local, peer, 49160, 0); e == nil {
		t.Fatal("client port accepted")
	}
}
func TestWiFiSIPResponseBounds(t *testing.T) {
	good := []byte("SIP/2.0 200 OK\r\nVia: SIP/2.0/UDP example;branch=x\r\nFrom: a\r\nTo: b\r\nCall-ID: c\r\nCSeq: 1 REGISTER\r\nContent-Length: 0\r\n\r\n")
	r, e := parseWiFiSIP(good)
	if e != nil || r.code != 200 {
		t.Fatal(e)
	}
	for i := 0; i < len(good); i++ {
		if _, e := parseWiFiSIP(good[:i]); e == nil {
			t.Fatal("truncation")
		}
	}
	for _, bad := range [][]byte{
		bytes.Replace(good, []byte("Content-Length: 0"), []byte("Content-Length: 1"), 1),
		bytes.Replace(good, []byte("Call-ID: c"), []byte("Call-ID: c\r\nCall-ID: d"), 1),
		bytes.Replace(good, []byte("Via:"), []byte("Bad\x00Header:"), 1),
	} {
		if _, e := parseWiFiSIP(bad); e == nil {
			t.Fatal("malformed SIP")
		}
	}
}
func FuzzWiFiPacketParsers(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte("SIP/2.0 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65536 {
			return
		}
		_, _ = parseWiFiSIP(b)
		_, _ = wifiParameters(string(b), ',')
		_, _ = wifiSelectors(b)
		_, _, _ = wifiConfig(b)
		_, _, _, _, _ = wifiParseIP(b, 4)
		_, _, _, _, _ = wifiParseIP(b, 41)
		_, _ = ikeParts(33, b)
	})
}
func TestWiFiTrafficBounds(t *testing.T) {
	s := []wifiSelector{{proto: 17, first: 5060, last: 5060, start: net.ParseIP("192.0.2.1").To4(), end: net.ParseIP("192.0.2.2").To4()}}
	if !wifiTrafficAllowed(s, net.ParseIP("192.0.2.1"), 17, 5060) || wifiTrafficAllowed(s, net.ParseIP("192.0.2.3"), 17, 5060) || wifiTrafficAllowed(s, net.ParseIP("192.0.2.1"), 50, 0) {
		t.Fatal("selector boundary")
	}
}
func TestWiFiCanceledTransaction(t *testing.T) {
	a, _ := espPair(false)
	a.tsi = []wifiSelector{{first: 0, last: 65535, start: net.IP{0, 0, 0, 0}, end: net.IP{255, 255, 255, 255}}}
	a.tsr = a.tsi
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := a.udpExchange(ctx, net.IP{192, 0, 2, 1}, net.IP{192, 0, 2, 2}, 49160, 5060, 49160, []byte("x"), nil, func([]byte) bool { return true })
	if e != context.Canceled {
		t.Fatal("cancel", e)
	}
}
func TestWiFiESPRejectsWrongSPI(t *testing.T) {
	a, b := espPair(true)
	p, _ := a.seal([]byte{1, 2, 3}, 17)
	binary.BigEndian.PutUint32(p, b.spiIn+1)
	if _, _, e := b.open(p); e == nil {
		t.Fatal("wrong association")
	}
}
