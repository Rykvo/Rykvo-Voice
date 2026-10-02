package mms

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/carrierconfig"
	"testing"
)

func TestMM1RoundTripAndConfirmation(t *testing.T) {
	b, e := SendRequest("test-request-123", "+12025550123", "中文 MMS", &Part{"image/jpeg", []byte{0xff, 0xd8, 0xff}})
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte{0x8a, 0x80, 0x86, 0x80, 0x84}) {
		t.Fatal("delivery report not requested")
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

func TestWSPImageTypeAssignments(t *testing.T) {
	// OMNA assigns image/*=0x1c, GIF=0x1d, JPEG=0x1e and PNG=0x20.
	for _, tc := range []struct {
		code byte
		kind string
	}{
		{0x1c, "application/octet-stream"},
		{0x1d, "image/gif"},
		{0x1e, "image/jpeg"},
		{0x1f, "application/octet-stream"},
		{0x20, "image/png"},
	} {
		for _, wire := range [][]byte{{tc.code | 0x80}, {1, tc.code | 0x80}} {
			c := cursor{b: wire}
			kind, err := c.contentType()
			if err != nil || kind != tc.kind || c.i != len(wire) {
				t.Fatalf("code=%x kind=%q err=%v", tc.code, kind, err)
			}
		}
	}
}

func TestRetrieveAnimatedGIFPreservesFrames(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(first.Rect, palette)
	second.SetColorIndex(0, 0, 1)
	var body bytes.Buffer
	if err := gif.EncodeAll(&body, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{5, 10}, LoopCount: 0}); err != nil {
		t.Fatal(err)
	}
	for _, header := range [][]byte{{0x9d}, {1, 0x9d}, txt("image/gif")} {
		wire := []byte{0x8c, 0x84, 0x8d, 0x92, 0x84, 0xb3, 1}
		wire = append(wire, uv(len(header))...)
		wire = append(wire, uv(body.Len())...)
		wire = append(wire, header...)
		wire = append(wire, body.Bytes()...)
		p, err := Parse(wire)
		if err != nil || len(p.Parts) != 1 || p.Parts[0].Type != "image/gif" || !bytes.Equal(p.Parts[0].Data, body.Bytes()) {
			t.Fatalf("GIF lost: parts=%d err=%v", len(p.Parts), err)
		}
		animation, err := gif.DecodeAll(bytes.NewReader(p.Parts[0].Data))
		if err != nil || len(animation.Image) != 2 || animation.Delay[1] != 10 {
			t.Fatal("animation changed", err)
		}
	}
}
func TestHostTransportRejectsLocalDestinations(t *testing.T) {
	if _, err := NewClient(carrierconfig.Profile{MMSC: "http://mms.example"}, nil); err == nil {
		t.Fatal("unbound host transport accepted")
	}
	for _, s := range []string{"file:///etc/passwd", "http://a:b@x/", "http://x:65536/", "https://x/#token"} {
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
