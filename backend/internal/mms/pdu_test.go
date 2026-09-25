package mms

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMM1RoundTripAndConfirmation(t *testing.T) {
	b, e := SendRequest("test-request-123", "+12025550123", "中文 MMS", &Part{"image/jpeg", []byte{0xff, 0xd8, 0xff}})
	if e != nil {
		t.Fatal(e)
	}
	p, e := Parse(b)
	if e != nil || p.Type != 0x80 || p.Transaction != "test-request-123" || len(p.Parts) != 2 || string(p.Parts[0].Data) != "中文 MMS" {
		t.Fatalf("%+v %v", p, e)
	}
	// Fixed OMA M-Send.conf: message-type, transaction-id, version, status, message-id.
	raw, _ := hex.DecodeString("8c81987465737400308d9292808b696400")
	if _, e = Parse(raw); e == nil {
		t.Fatal("invalid extra octet accepted")
	}
	raw = []byte{0x8c, 0x81, 0x98, 't', 'e', 's', 't', 0, 0x8d, 0x92, 0x92, 0x80, 0x8b, 'i', 'd', 0}
	p, e = Parse(raw)
	if e != nil || p.Status != 0x80 || p.MessageID != "id" {
		t.Fatal(p, e)
	}
	for i := 0; i < len(b); i++ {
		_, _ = Parse(b[:i])
	}
	if _, e := Parse(append(raw, 0x8c, 0x81)); e == nil {
		t.Fatal("duplicate singleton header")
	}
	if _, e := SendRequest("request-123", "+123", "\x00", nil); e == nil {
		t.Fatal("NUL accepted")
	}
}
func TestWAPPush(t *testing.T) {
	body := []byte{0x8c, 0x82, 0x98, 'a', 0, 0x8d, 0x92, 0x83}
	body = append(body, txt("http://mmsc.example/message")...)
	p, e := Push(append([]byte{1, 6, 1, 0xbe}, body...))
	if e != nil || p.Type != 0x82 || p.Location != "http://mmsc.example/message" {
		t.Fatal(p, e)
	}
	if _, e := Push([]byte{1, 6, 255}); e == nil {
		t.Fatal("truncation accepted")
	}
}
func TestHostTransportRejectsLocalDestinations(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "::1", "::ffff:127.0.0.1", "fe80::1", "2001:db8::1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Fatal(ip)
		}
	}
	for _, s := range []string{"file:///etc/passwd", "http://a:b@x/", "http://x:22/", "https://x/#token"} {
		if _, e := safeURL(s); e == nil {
			t.Fatal(s)
		}
	}
}
func TestHTTPStatusIsNotMMSAcceptance(t *testing.T) {
	for _, body := range [][]byte{[]byte("OK"), bytes.Repeat([]byte{'x'}, MaxSize+1)} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.wap.mms-message")
			w.Write(body)
		}))
		if _, e := exchange(context.Background(), ts.Client(), "POST", ts.URL, []byte{1}); e == nil {
			t.Fatal("accepted invalid MM1")
		}
		ts.Close()
	}
}
func FuzzPDU(f *testing.F) {
	f.Add([]byte{0x8c, 0x81, 0x8d, 0x92, 0x92, 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > MaxSize {
			return
		}
		_, _ = Parse(b)
		_, _ = Push(b)
	})
}

func TestRedirectAfterSubmissionIsNeverRetried(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/next", http.StatusFound) }))
	defer ts.Close()
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrNetwork }
	_, err := exchange(context.Background(), client, "POST", ts.URL, []byte{1})
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("submitted request became retryable: %v", err)
	}
}
