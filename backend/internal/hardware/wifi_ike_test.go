package hardware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"net"
	"testing"
)

func TestWiFiIKEEnvelope(t *testing.T) {
	k := bytes.Repeat([]byte{1}, 32)
	enc := bytes.Repeat([]byte{2}, 16)
	s := &wifiIKE{prf: sha256.New, macLen: 16, ai: k, ar: k, ei: enc, er: enc}
	parts := []ikePart{{48, []byte{1, 2, 0, 5, 1}}, {41, []byte{0, 0, 64, 1}}}
	packet, err := s.seal(35, 1, parts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.open(packet)
	if err != nil || len(got) != 2 || !bytes.Equal(got[0].data, parts[0].data) {
		t.Fatal("envelope", err)
	}
	for i := range packet {
		corrupt := bytes.Clone(packet)
		corrupt[i] ^= 1
		if _, err = s.open(corrupt); err == nil {
			t.Fatal("unauthenticated change", i)
		}
	}
	for n := 0; n < len(packet); n++ {
		if _, err = s.open(packet[:n]); err == nil {
			t.Fatal("truncation", n)
		}
	}
}
func TestWiFiIKEPayloadBounds(t *testing.T) {
	body := ikePayloads([]ikePart{{33, []byte{1}}, {40, []byte{2, 3}}})
	if p, err := ikeParts(33, body); err != nil || len(p) != 2 {
		t.Fatal(err)
	}
	for n := 0; n < len(body); n++ {
		if _, err := ikeParts(33, body[:n]); err == nil {
			t.Fatal("truncated chain", n)
		}
	}
	malformed := bytes.Clone(body)
	binary.BigEndian.PutUint16(malformed[2:4], 65535)
	if _, err := ikeParts(33, malformed); err == nil {
		t.Fatal("large payload")
	}
}
func TestWiFiIKESuiteBounds(t *testing.T) {
	for _, proto := range []byte{1, 3} {
		spi := []byte(nil)
		if proto == 3 {
			spi = make([]byte, 4)
		}
		p := ikeProposal(1, proto, spi, 5, 12, true)
		if _, _, err := ikeSelection(p, proto); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < len(p); n++ {
			if _, _, err := ikeSelection(p[:n], proto); err == nil {
				t.Fatal("truncated suite")
			}
		}
		p[len(p)-1] ^= 1
		if _, _, err := ikeSelection(p, proto); err == nil {
			t.Fatal("unoffered suite")
		}
	}
}
func TestWiFiIKEPublicEndpoints(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "0.1.2.3", "::1", "fc00::1", "64:ff9b::7f00:1", "64:ff9b::a00:1"} {
		if publicWiFiIP(net.ParseIP(ip)) {
			t.Fatal("non-public endpoint", ip)
		}
	}
	if !publicWiFiIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public IPv4 rejected")
	}
}
func TestWiFiIKEFailedOpenClosesEAP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sim := &wifiSIM{id: wifiIdentity{IMSI: "001010123456789", MCC: "001", MNC: "01"}}
	if s, err := openWiFiIKE(ctx, sim); err == nil || s != nil {
		t.Fatal("cancelled network start")
	}
}
