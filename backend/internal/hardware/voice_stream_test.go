package hardware

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"runtime"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

type streamTestController struct {
	*testVoiceController
	read    chan []int16
	written chan []int16
}

func newStreamController() *streamTestController {
	return &streamTestController{testVoiceController: &testVoiceController{}, read: make(chan []int16, 4), written: make(chan []int16, 4)}
}
func (c *streamTestController) CallMedia(context.Context, string) (vowifi.CallMedia, error) {
	return c, nil
}
func (c *streamTestController) Codec() string { return "PCMU" }
func (c *streamTestController) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case p := <-c.read:
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *streamTestController) WritePCM(p []int16) error {
	select {
	case c.written <- append([]int16(nil), p...):
		return nil
	default:
		return errors.New("test output full")
	}
}

func streamTestOpen(t *testing.T, f *streamTestController) (*WiFiCall, *voiceWorker) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("abstract UNIX stream runs on Linux host and CI")
	}
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := api.OpenWiFiCall(ctx, c, card)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v, w
}

func streamTestActive(t *testing.T, v *WiFiCall, f *streamTestController) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := v.Dial(ctx, "+12025550123"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AnswerCall(ctx, "carrier-call"); err != nil {
		t.Fatal(err)
	}
	if err := v.wait(ctx, func(s voiceStreamState) bool { return s.State == "active" }); err != nil {
		t.Fatal(err)
	}
}

func waitStreamClosed(t *testing.T, w *voiceWorker) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		w.mu.Lock()
		active := w.stream != nil
		w.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream lease remains held")
}

func TestWiFiVoicePCMAndConfirmedHangup(t *testing.T) {
	f := newStreamController()
	v, w := streamTestOpen(t, f)
	streamTestActive(t, v, f)
	v.session.writeMu.Lock() // Simulate a blocked SMS/MMS control write.
	defer v.session.writeMu.Unlock()
	p := make([]int16, 160)
	for i := range p {
		p[i] = int16(i*413 - 32000)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := v.WritePCM(p); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-f.written:
		if !reflect.DeepEqual(got, p) {
			t.Fatal("uplink samples changed")
		}
	case <-ctx.Done():
		t.Fatal("uplink missing")
	}
	f.read <- p
	got, err := v.ReadPCM(ctx)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatal("downlink changed", err)
	}
	if err := v.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
	waitStreamClosed(t, w)
	if state, err := v.State(ctx); err != nil || state != "idle" {
		t.Fatal(state, err)
	}
	if err := v.Dial(ctx, "12345"); err == nil {
		t.Fatal("reused closed lease")
	}
	if err := v.WritePCM(p); err == nil {
		t.Fatal("audio after hangup")
	}
}

func TestWiFiVoiceLostMediaConnectionHoldsModuleUntilCarrierEnds(t *testing.T) {
	f := newStreamController()
	v, w := streamTestOpen(t, f)
	streamTestActive(t, v, f)
	f.mu.Lock()
	f.ending = true
	f.mu.Unlock()
	v.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := v.Hangup(ctx); err == nil {
		t.Fatal("unconfirmed carrier hangup released module")
	}
	w.mu.Lock()
	held := w.stream != nil
	w.mu.Unlock()
	if !held {
		t.Fatal("worker released unconfirmed call")
	}
	f.mu.Lock()
	f.ending = false
	f.mu.Unlock()
	waitStreamClosed(t, w)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := v.Hangup(ctx2); err != nil {
		t.Fatal("confirmed cleanup not recovered", err)
	}
}

func TestWiFiVoicePrivateLeaseRejectsWrongKeyAndNewCall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux abstract sockets")
	}
	f := newStreamController()
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := api.OpenWiFiCall(ctx, Candidate{Key: c.Key, Generation: "other"}, card); err == nil {
		t.Fatal("generation escaped")
	}
	if _, err := api.OpenWiFiCall(ctx, c, "other-card"); err == nil {
		t.Fatal("SIM escaped")
	}
	s := api.voiceSession(c, card)
	r, err := s.exchangeVoice(ctx, voiceCommand{Op: "voice-open", ID: "request-stream"})
	if err != nil || r.Media == nil {
		t.Fatal(err)
	}
	voiceIdle(t, w)
	if _, err := s.exchangeVoice(ctx, voiceCommand{Op: "voice-dial", ID: "request-other", Number: "12345"}); err == nil {
		t.Fatal("second controller bypassed lease")
	}
	conn, err := net.Dial("unix", r.Media.Address)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write(make([]byte, 32))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("invalid media key accepted")
	}
	conn.Close()
	waitStreamClosed(t, w)
	f.mu.Lock()
	dials := f.dials
	f.mu.Unlock()
	if dials != 0 {
		t.Fatal("unauthenticated call")
	}
}

func TestWiFiVoiceWorkerLossAndCancellationStopReaders(t *testing.T) {
	f := newStreamController()
	v, w := streamTestOpen(t, f)
	streamTestActive(t, v, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.ReadPCM(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	w.setController(nil)
	select {
	case <-v.closed:
	case <-time.After(time.Second):
		t.Fatal("media leaked on worker retirement")
	}
	waitStreamClosed(t, w)
}

func TestWiFiVoiceInvalidPCMEndsOwnedCall(t *testing.T) {
	f := newStreamController()
	v, w := streamTestOpen(t, f)
	streamTestActive(t, v, f)
	if err := v.wire.write(voicePCM, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := v.wait(ctx, func(s voiceStreamState) bool { return s.State == "ended" }); err != nil {
		t.Fatal(err)
	}
	waitStreamClosed(t, w)
}

func TestVoiceFrameBoundsAndPCMExact(t *testing.T) {
	for _, n := range []int{0, 1, 319, 321, 1025, 65535} {
		if _, err := decodeVoicePCM(make([]byte, n)); err == nil {
			t.Fatal("bad PCM size", n)
		}
	}
	for _, b := range [][]byte{{voicePCM, 4, 1}, {255, 0, 0}, {voicePCM, 1, 64, 0}} {
		if _, err := readVoiceFrame(bytes.NewReader(b)); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	for _, e := range []voiceEndpoint{{Address: "/tmp/other", Key: "abc"}, {Address: "@rykvo-voice-pcm-0000", Key: "abc"}} {
		if e.valid() {
			t.Fatal("arbitrary endpoint")
		}
	}
	p := make([]int16, 160)
	for i := range p {
		p[i] = int16(i*499 - 32768)
	}
	b, err := encodeVoicePCM(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeVoicePCM(b)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatal("PCM mutated")
	}
}

func FuzzVoiceMediaFrames(f *testing.F) {
	f.Add([]byte{voicePCM, 1, 64})
	b, _ := json.Marshal(voiceStreamState{State: "active", Call: "call", Codec: "PCMU"})
	f.Add(append([]byte{voiceStatus, 0, byte(len(b))}, b...))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := readVoiceFrame(bytes.NewReader(b))
		if err == nil && (len(v.data) > 1024 || len(b) < 3 || len(v.data) != int(binary.BigEndian.Uint16(b[1:3]))) {
			t.Fatal("frame bound")
		}
		if _, err := decodeVoicePCM(b); err == nil && len(b) != 320 {
			t.Fatal("PCM bound")
		}
		var s voiceStreamState
		if json.Unmarshal(b, &s) == nil {
			_ = s.valid()
		}
		var e voiceEndpoint
		if json.Unmarshal(b, &e) == nil {
			_ = e.valid()
		}
		_, _ = readVoiceFrame(io.LimitReader(bytes.NewReader(b), 2048))
	})
}
