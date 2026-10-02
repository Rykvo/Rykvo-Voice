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
	read      chan []int16
	readError chan error
	written   chan []int16
}

func newStreamController() *streamTestController {
	return &streamTestController{testVoiceController: &testVoiceController{}, read: make(chan []int16, 4), readError: make(chan error, 1), written: make(chan []int16, 4)}
}
func (c *streamTestController) CallMedia(context.Context, string) (vowifi.CallMedia, error) {
	return c, nil
}
func (c *streamTestController) Codec() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codec != "" {
		return c.codec
	}
	return "PCMU"
}
func (c *streamTestController) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case p := <-c.read:
		return p, nil
	case err := <-c.readError:
		return nil, err
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
	for _, codec := range []string{"PCMU", "AMR", "AMR-WB"} {
		t.Run(codec, func(t *testing.T) { testWiFiVoicePCMAndConfirmedHangup(t, codec) })
	}
}

func testWiFiVoicePCMAndConfirmedHangup(t *testing.T, codec string) {
	f := newStreamController()
	f.codec = codec
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

func TestWiFiPCMConfirmedEndIsNotMediaFailure(t *testing.T) {
	for _, closed := range []bool{false, true} {
		v := &WiFiCall{state: voiceStreamState{State: "ended", Call: "carrier-call"}, closed: make(chan struct{}), pcm: make(chan []int16, 1)}
		if closed {
			close(v.closed)
		}
		if err := v.WritePCM(make([]int16, 160)); !errors.Is(err, ErrVoiceEnded) {
			t.Fatalf("closed=%t: confirmed hangup reported as %v", closed, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := v.ReadPCM(ctx)
		cancel()
		if !errors.Is(err, ErrVoiceEnded) {
			t.Fatalf("closed=%t: confirmed hangup reader returned %v", closed, err)
		}
	}
	for _, state := range []voiceStreamState{{State: "active", Call: "carrier-call"}, {State: "ended"}, {State: "ended", Call: "carrier-call", Fault: "media_write"}} {
		v := &WiFiCall{state: state, closed: make(chan struct{}), pcm: make(chan []int16)}
		close(v.closed)
		if err := v.WritePCM(make([]int16, 160)); err == nil || errors.Is(err, ErrVoiceEnded) {
			t.Fatalf("unconfirmed or failed media hidden: %+v %v", state, err)
		}
		if _, err := v.ReadPCM(context.Background()); err == nil || errors.Is(err, ErrVoiceEnded) {
			t.Fatalf("reader hid actual media failure: %+v %v", state, err)
		}
		if state.Fault != "" {
			if _, err := v.State(context.Background()); err == nil {
				t.Fatal("polling hid a confirmed media fault")
			}
		}
	}
}

func TestWiFiStreamMediaFaultRequiresPriorCarrierEnd(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		call    vowifi.Call
		failure error
		ended   bool
	}{
		{"active", vowifi.Call{ID: "call", State: "active"}, nil, false},
		{"ending", vowifi.Call{ID: "call", State: "ending"}, nil, false},
		{"unconfirmed", vowifi.Call{ID: "call", State: "ended"}, nil, false},
		{"confirmed", vowifi.Call{ID: "call", State: "ended", EndedAt: &now}, nil, true},
		{"failed", vowifi.Call{ID: "call", State: "failed", EndedAt: &now}, nil, false},
		{"other call", vowifi.Call{ID: "other", State: "ended", EndedAt: &now}, nil, false},
		{"lookup failed", vowifi.Call{ID: "call", State: "ended", EndedAt: &now}, io.EOF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamController()
			f.calls, f.failure = []vowifi.Call{tc.call}, tc.failure
			s := &voiceStream{controller: f}
			for _, fault := range []string{"media_read", "media_write", "stream_read", "invalid_pcm"} {
				want := fault
				if tc.ended && (fault == "media_read" || fault == "media_write") {
					want = ""
				}
				if got := s.mediaFault("call", fault); got != want {
					t.Fatalf("%s: got %q, want %q", fault, got, want)
				}
			}
		})
	}
}

func TestWiFiVoiceRemoteHangupAndActualMediaLoss(t *testing.T) {
	for _, carrierEnded := range []bool{true, false} {
		f := newStreamController()
		v, w := streamTestOpen(t, f)
		streamTestActive(t, v, f)
		if carrierEnded {
			f.mu.Lock()
			now := time.Now()
			f.calls[0].State, f.calls[0].EndedAt = "ended", &now
			f.mu.Unlock()
		}
		f.readError <- io.EOF
		select {
		case <-v.closed:
		case <-time.After(3 * time.Second):
			t.Fatal("media EOF did not settle")
		}
		waitStreamClosed(t, w)
		_, err := v.ReadPCM(context.Background())
		if carrierEnded != errors.Is(err, ErrVoiceEnded) || err == nil {
			t.Fatalf("carrierEnded=%t: wrong read result %v", carrierEnded, err)
		}
		err = v.WritePCM(make([]int16, 160))
		if carrierEnded != errors.Is(err, ErrVoiceEnded) || err == nil {
			t.Fatalf("carrierEnded=%t: wrong write result %v", carrierEnded, err)
		}
		wantFault := ""
		if !carrierEnded {
			wantFault = "media_read"
		}
		if v.MediaFault() != wantFault {
			t.Fatalf("cleanup changed original fault: %q", v.MediaFault())
		}
	}
}

func TestWiFiPCMWriteFailureReadsFinalCarrierStatus(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	v := &WiFiCall{state: voiceStreamState{State: "active", Call: "call"}, wire: &voiceStreamIO{conn: a}, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Close()
		time.Sleep(5 * time.Millisecond)
		v.mu.Lock()
		v.state.State = "ended"
		v.mu.Unlock()
		close(v.closed)
	}()
	if err := v.WritePCM(make([]int16, 160)); !errors.Is(err, ErrVoiceEnded) {
		t.Errorf("lost final status after write error: %v", err)
	}
	<-done
}

func TestWiFiIncomingAttachAnswersOnlyBoundCall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract UNIX stream")
	}
	f := newStreamController()
	f.calls = []vowifi.Call{{ID: "incoming", Number: "+12025550123", Direction: "incoming", State: "ringing"}}
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := api.OpenWiFiIncoming(ctx, c, card, "other"); err == nil {
		t.Fatal("attached another call")
	}
	voiceIdle(t, w)
	v, err := api.OpenWiFiIncoming(ctx, c, card, "incoming")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if s, _ := v.State(ctx); s != "ringing" {
		t.Fatal(s)
	}
	if v.Dial(ctx, "12345") == nil {
		t.Fatal("incoming stream dialed")
	}
	if v.WritePCM(make([]int16, 160)) == nil {
		t.Fatal("pre-answer PCM forwarded")
	}
	if err = v.Answer(ctx); err != nil {
		t.Fatal(err)
	}
	if err = v.wait(ctx, func(s voiceStreamState) bool { return s.State == "active" }); err != nil {
		t.Fatal(err)
	}
	if f.dials != 0 {
		t.Fatal("carrier dialed on incoming path")
	}
	if err = v.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
	waitStreamClosed(t, w)
}

func TestWiFiIncomingRefusesOtherLiveCallAndNoAnswerOnDisconnect(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract UNIX stream")
	}
	f := newStreamController()
	f.calls = []vowifi.Call{{ID: "incoming", Direction: "incoming", State: "ringing"}, {ID: "old", Direction: "outgoing", State: "active"}}
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := api.OpenWiFiIncoming(ctx, c, card, "incoming"); err == nil {
		t.Fatal("occupied module attached")
	}
	voiceIdle(t, w)
	f.mu.Lock()
	f.calls = f.calls[:1]
	f.mu.Unlock()
	v, err := api.OpenWiFiIncoming(ctx, c, card, "incoming")
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	waitStreamClosed(t, w)
	calls, _ := f.Calls()
	if calls[0].EndedAt == nil || calls[0].AnsweredAt != nil {
		t.Fatal("disconnect answered or leaked incoming")
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

func TestWiFiVoiceFailureCauseSurvivesPrivateStream(t *testing.T) {
	f := newStreamController()
	v, w := streamTestOpen(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := v.Dial(ctx, "12345"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	now := time.Now()
	f.calls[0].SIPCode, f.calls[0].Failure = 606, "rejected"
	f.calls[0].EndedAt, f.calls[0].State = &now, "failed"
	f.mu.Unlock()
	if err := v.wait(ctx, func(s voiceStreamState) bool { return s.State == "ended" }); err != nil {
		t.Fatal(err)
	}
	if v.SIPCode() != 606 || v.FailureReason() != "rejected" {
		t.Fatal("carrier cause lost", v.SIPCode(), v.FailureReason())
	}
	waitStreamClosed(t, w)
}

func TestVoiceStreamRejectsUnclassifiedFailure(t *testing.T) {
	if (voiceStreamState{State: "ended", Failure: "raw SIP header"}).valid() {
		t.Fatal("raw diagnostic accepted")
	}
	if !(voiceStreamState{State: "ended", Failure: "rejected"}).valid() {
		t.Fatal("classified cause rejected")
	}
}
