package hardware

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWiFiCarrierSelectors(t *testing.T) {
	rules, e := parseWiFiCarriers(wifiCarrierData)
	if e != nil || len(rules) < 600 {
		t.Fatal(len(rules), e)
	}
	for i, p := range rules {
		rules[i] = builtinWiFiCarrierFacts(p)
	}
	id := wifiIdentity{MCC: "234", MNC: "10", GID1: "508FFFFF"}
	p := resolveWiFiCarrier(id, rules)
	if p.ID != "o2-giffgaff-uk" || p.Transport != "tcp" || p.Country != "GB" {
		t.Fatalf("%+v", p)
	}
	rules = append(rules, wifiCarrier{ID: "broad", Match: []wifiMatch{{PLMN: "23410"}}, Transport: "udp"})
	if resolveWiFiCarrier(id, rules).Transport != "tcp" {
		t.Fatal("broad rule hid MVNO")
	}
	if resolveWiFiCarrier(wifiIdentity{MCC: "999", MNC: "99"}, rules).ID != "standard-3gpp" {
		t.Fatal("unknown operator not standard")
	}
	if matchWiFiCarrier(wifiMatch{PLMN: "23410", GID1: "508"}, wifiIdentity{MCC: "234", MNC: "10"}) >= 0 {
		t.Fatal("missing selector accepted")
	}
	for _, v := range []string{`{"version":2}`, `{"version":1,"profiles":[{"id":"a","match":[{"plmn":"23410"}],"agent":"x\r\nInjected: y"}]}`, `{"version":1,"profiles":[{"id":"a","match":[{"plmn":"23410"}],"epdg":"127.0.0.1"}]}`} {
		if _, e := parseWiFiCarriers([]byte(v)); e == nil {
			t.Fatal("invalid config")
		}
	}
}
func TestWiFiFallbackBoundaries(t *testing.T) {
	for _, code := range []string{"WIFI_IMS_TIMEOUT", "WIFI_TCP_CONNECT_FAILED"} {
		if !wifiTransportFallback(false, errors.New(code)) || wifiTransportFallback(true, errors.New(code)) {
			t.Fatal(code)
		}
	}
	for _, e := range []error{context.Canceled, errors.New("WIFI_PEER_AUTH_FAILED"), errors.New("WIFI_IMS_STATUS_403"), errors.New("AKA_REJECTED")} {
		if wifiTransportFallback(false, e) {
			t.Fatal(e)
		}
	}
}
func TestWiFiSIPStreamFraming(t *testing.T) {
	msg := "SIP/2.0 200 OK\r\nContent-Length: 3\r\n\r\nabc"
	r := newWiFiSIPReader(strings.NewReader("\r\n" + msg + msg))
	for i := 0; i < 2; i++ {
		got, e := r.read()
		if e != nil || string(got) != msg {
			t.Fatal(e)
		}
	}
	for _, raw := range []string{"SIP/2.0 200 OK\r\nContent-Length: -1\r\n\r\n", "SIP/2.0 200 OK\r\nContent-Length: 0\r\nl: 0\r\n\r\n", "SIP/2.0 200 OK\r\n\r\n", msg[:len(msg)-1], strings.Repeat("x", 20000)} {
		if _, e := newWiFiSIPReader(strings.NewReader(raw)).read(); e == nil {
			t.Fatal("accepted malformed stream")
		}
	}
}
func FuzzWiFiSIPStream(f *testing.F) {
	f.Add([]byte("SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 70000 {
			return
		}
		_, _ = newWiFiSIPReader(bytes.NewReader(b)).read()
	})
}

func TestWiFiSecurityAlternatives(t *testing.T) {
	values := []string{"ipsec-3gpp;alg=hmac-md5-96;ealg=null;spi-c=256;spi-s=257, ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;spi-c=258;spi-s=259;port-c=50000;port-s=50002;q=0.8"}
	p, verify, e := wifiSecuritySelection(values)
	if e != nil || p["spi-s"] != "259" || verify != values[0] {
		t.Fatal(p, e)
	}
	if _, _, e := wifiSecuritySelection([]string{"ipsec-3gpp;alg=hmac-sha-1-96;ealg=null"}); e == nil {
		t.Fatal("unoffered cipher selected")
	}
	if _, _, e := wifiSecuritySelection([]string{"ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;q=2"}); e == nil {
		t.Fatal("invalid preference accepted")
	}
}

func TestWiFiRegistrationAuthorization(t *testing.T) {
	s := &wifiIMS{identity: "subscriber@example.invalid", domain: "example.invalid"}
	a, n := s.authorization("sip:example.invalid")
	if n != 0 || !strings.Contains(a, "integrity-protected=no") {
		t.Fatal("initial request must not claim protection")
	}
	s.protection = &wifiChild{}
	a, n = s.authorization("sip:example.invalid")
	if n != 0 || !strings.Contains(a, `response=""`) || !strings.Contains(a, "integrity-protected=yes") {
		t.Fatal("protected refresh without reusable challenge")
	}
	s.credentials = map[string]string{"realm": s.domain, "nonce": "nonce", "qop": "auth", "cnonce": "cnonce"}
	s.res = []byte("sample")
	a, n = s.authorization("sip:example.invalid")
	b, next := s.authorization("sip:example.invalid")
	if n != 1 || next != 2 || a == b || !strings.Contains(b, "nc=00000002") {
		t.Fatal("refresh reused nonce count")
	}
}

func TestWiFiRegistrarBackoff(t *testing.T) {
	for _, tc := range []struct {
		value   string
		seconds int
	}{{"1585", 1585}, {"817 (busy);duration=10", 817}, {"0", 5}, {"-1", 30}, {"bad", 30}, {"999999999999999", 30}} {
		if got := wifiRetryAfter([]string{tc.value}); got != time.Duration(tc.seconds)*time.Second {
			t.Fatal(tc.value, got)
		}
	}
	s := &wifiIMS{retryAfter: time.Now().Add(time.Minute)}
	_, e := s.exchange(context.Background(), 0)
	var delay *wifiRegistrarDelay
	if !errors.As(e, &delay) || !wifiRetryable(e) || wifiTransportFallback(false, e) {
		t.Fatal("backoff bypassed", e)
	}
}

func TestWiFiOwnContactExpiry(t *testing.T) {
	s := &wifiIMS{contact: "<sip:192.0.2.1:49162>;+sip.instance=\"<urn:uuid:sample>\""}
	r := wifiSIP{headers: map[string][]string{"expires": {"600"}, "contact": {"<sip:192.0.2.9:49162>;expires=0, <sip:192.0.2.1:49162>;expires=3600"}}}
	if e := s.setExpiry(r); e != nil {
		t.Fatal(e)
	}
	if d := time.Until(s.renewAt); d < 1799*time.Second || d > 1801*time.Second {
		t.Fatal("ignored actual grant", d)
	}
	r.headers["contact"] = []string{"<sip:192.0.2.9:49162>;expires=3600"}
	if s.setExpiry(r) == nil {
		t.Fatal("another device counted as registered")
	}
}
