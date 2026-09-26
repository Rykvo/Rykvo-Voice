package hardware

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

type testVoiceController struct {
	mu      sync.Mutex
	calls   []vowifi.Call
	dials   int
	failure error
	ending  bool
}

func (f *testVoiceController) Calls() ([]vowifi.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vowifi.Call(nil), f.calls...), f.failure
}
func (f *testVoiceController) DialCall(ctx context.Context, number string) (vowifi.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	f.calls = []vowifi.Call{{ID: "carrier-call", Number: number, Direction: "outgoing", State: "dialing"}}
	return f.calls[0], nil
}
func (f *testVoiceController) AnswerCall(ctx context.Context, id string) (vowifi.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || f.calls[0].ID != id {
		return vowifi.Call{}, ims.ErrCallNotFound
	}
	f.calls[0].State = "active"
	f.calls[0].MediaReady = true
	f.calls[0].Codec = "PCMU"
	return f.calls[0], nil
}
func (f *testVoiceController) HangupCall(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || f.calls[0].ID != id {
		return ims.ErrCallNotFound
	}
	f.calls[0].MediaReady = false
	if f.ending {
		f.calls[0].State = "ending"
		return ims.ErrCallEnding
	}
	now := time.Now()
	f.calls[0].EndedAt = &now
	f.calls[0].State = "ended"
	return nil
}
func voiceTestPeer(t *testing.T, f voiceController) (*VocatWorkerClient, Candidate, string, *voiceWorker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client, server := net.Pipe()
	e := &smsEventEncoder{encoder: json.NewEncoder(server)}
	w := newVoiceWorker(ctx, func(v wifiWorkerEvent) { _ = e.Encode(v) })
	w.setController(f)
	request := workerRequest()
	c := Candidate{Key: request.Endpoint, Generation: request.Generation}
	s := newSMSClientSession(ctx, client, nil)
	api := &VocatWorkerClient{smsSessions: map[string]*smsClientSession{smsSessionKey(c, request.ICCID): s}}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		scanner := bufio.NewScanner(server)
		if !scanner.Scan() {
			return
		}
		for scanner.Scan() {
			if scanner.Text() == "stop" {
				_ = e.Encode(wifiWorkerEvent{Done: true, Code: "CANCELLED"})
				return
			}
			if !w.command(scanner.Bytes()) {
				return
			}
		}
	}()
	readerDone := make(chan error, 1)
	go func() { defer s.close(); readerDone <- wifiWorkerExchangeSMS(ctx, client, request, func(string) {}, s) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-readerDone:
		case <-time.After(time.Second):
			t.Error("voice exchange failed to stop")
		}
		client.Close()
		server.Close()
		<-serverDone
		w.setController(nil)
		w.wait.Wait()
	})
	return api, c, request.ICCID, w
}
func voiceIdle(t *testing.T, w *voiceWorker) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		busy := w.busy
		w.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("voice operation stuck")
}
func TestVoicePrivateIPCControlsBoundModuleAndRejectsReplay(t *testing.T) {
	f := &testVoiceController{}
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	calls, err := api.DialWiFiCall(ctx, c, card, "dial-0001", "+12025550123")
	if err != nil || len(calls) != 1 || calls[0].State != "dialing" || calls[0].MediaReady {
		t.Fatal(calls, err)
	}
	voiceIdle(t, w)
	if _, err = api.DialWiFiCall(ctx, c, card, "dial-0001", "+12025550123"); err == nil || err.Error() != "VOICE_DUPLICATE_REQUEST" {
		t.Fatal("replay", err)
	}
	if _, err = api.DialWiFiCall(ctx, c, card, "dial-0002", "+12025550124"); err == nil || err.Error() != "VOICE_BUSY" {
		t.Fatal("double dial", err)
	}
	voiceIdle(t, w)
	if _, err = api.WiFiCalls(ctx, Candidate{Key: c.Key, Generation: "replacement"}, card, "list-0001"); err == nil || err.Error() != "VOICE_NOT_READY" {
		t.Fatal("generation not pinned", err)
	}
	if _, err = api.WiFiCalls(ctx, c, "other-card", "list-0002"); err == nil || err.Error() != "VOICE_NOT_READY" {
		t.Fatal("SIM not pinned", err)
	}
	calls, err = api.HangupWiFiCall(ctx, c, card, "hangup-0001", calls[0].ID)
	if err != nil || len(calls) != 1 || calls[0].State != "ended" {
		t.Fatal(calls, err)
	}
	voiceIdle(t, w)
	_, err = api.DialWiFiCall(ctx, c, card, "dial-0003", "+12025550124")
	if err != nil {
		t.Fatal(err)
	}
	voiceIdle(t, w)
	f.mu.Lock()
	n := f.dials
	f.mu.Unlock()
	if n != 2 {
		t.Fatal("duplicate invoked modem", n)
	}
}
func TestVoiceUnconfirmedHangupDoesNotReleaseModule(t *testing.T) {
	f := &testVoiceController{ending: true, calls: []vowifi.Call{{ID: "carrier-call", State: "active", Direction: "outgoing", Codec: "PCMA", MediaReady: true}}}
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := api.HangupWiFiCall(ctx, c, card, "hangup-0001", "carrier-call"); err == nil || err.Error() != "VOICE_ENDING" {
		t.Fatal(err)
	}
	voiceIdle(t, w)
	calls, err := api.WiFiCalls(ctx, c, card, "status-0001")
	if err != nil || len(calls) != 1 || calls[0].State != "ending" || calls[0].MediaReady {
		t.Fatal(calls, err)
	}
	voiceIdle(t, w)
	if _, err := api.DialWiFiCall(ctx, c, card, "dial-0001", "+12025550123"); err == nil || err.Error() != "VOICE_BUSY" {
		t.Fatal(err)
	}
}
func TestVoicePrivateIPCAnswerAndSanitizedFailures(t *testing.T) {
	f := &testVoiceController{calls: []vowifi.Call{{ID: "incoming", State: "ringing", Direction: "incoming", Number: "sip:subscriber-secret@carrier"}}}
	api, c, card, w := voiceTestPeer(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	calls, err := api.AnswerWiFiCall(ctx, c, card, "answer-0001", "incoming")
	if err != nil || len(calls) != 1 || !calls[0].MediaReady || calls[0].Number != "" || calls[0].Codec != "PCMU" {
		t.Fatal(calls, err)
	}
	voiceIdle(t, w)
	f.mu.Lock()
	f.failure = errors.New("secret raw AKA SIP error")
	f.mu.Unlock()
	_, err = api.WiFiCalls(ctx, c, card, "status-0001")
	if err == nil || err.Error() != "VOICE_OUTCOME_UNKNOWN" {
		t.Fatal(err)
	}
	voiceIdle(t, w)
	w.setController(nil)
	_, err = api.WiFiCalls(ctx, c, card, "status-0002")
	if err == nil || err.Error() != "VOICE_NOT_READY" {
		t.Fatal(err)
	}
}
func TestVoiceRejectsUnrecognizedCommandsAndFields(t *testing.T) {
	w := newVoiceWorker(context.Background(), func(wifiWorkerEvent) {})
	for _, raw := range []string{
		`{"op":"voice-dial","id":"request-01","number":"123;ATD"}`,
		`{"op":"voice-dial","id":"request-01","number":"123","command":"ATH"}`,
		`{"op":"voice-list","id":"request-01","number":"123"}`,
		`{"op":"voice-answer","id":"request-01","call":"bad\r\nID"}`,
		`{"op":"voice-hangup","id":"request-01"}`,
		`{"op":"voice-shell","id":"request-01"}`,
		`{"op":"voice-list","id":"request-01"} {}`,
		strings.Repeat(" ", 2049),
	} {
		if w.command([]byte(raw)) {
			t.Fatalf("accepted %q", raw)
		}
	}
}
func TestVoiceReplyValidation(t *testing.T) {
	s := newSMSClientSession(context.Background(), nil, nil)
	for _, r := range []voiceReply{
		{ID: "request-01", Code: "provider secret"},
		{ID: "request-01", Calls: []VoiceCall{{ID: "call", Direction: "outgoing", State: "active", Codec: "AMR", MediaReady: true}}},
		{ID: "request-01", Calls: []VoiceCall{{ID: "call", Direction: "outgoing", State: "ended", Codec: "PCMA", MediaReady: true}}},
		{ID: "request-01", Calls: make([]VoiceCall, 65)},
	} {
		if s.voiceEvent(r) {
			t.Fatalf("accepted %+v", r)
		}
	}
	if !s.voiceEvent(voiceReply{ID: "late-request", Code: "VOICE_ENDING"}) {
		t.Fatal("late valid response must not kill SMS lease")
	}
}
func TestVoiceReadOnlyPollingDoesNotExhaustReplayBudget(t *testing.T) {
	f := &testVoiceController{}
	api, c, card, w := voiceTestPeer(t, f)
	w.mu.Lock()
	for i := 0; i < 10000; i++ {
		w.used[time.Unix(int64(i), 0).String()] = true
	}
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := api.WiFiCalls(ctx, c, card, "status-0001"); err != nil {
		t.Fatal(err)
	}
}

func FuzzVoiceControlBoundary(f *testing.F) {
	f.Add([]byte(`{"op":"voice-list","id":"request-01"}`))
	f.Add([]byte(`{"op":"voice-dial","id":"request-01","number":"+12025550123"}`))
	f.Add([]byte(`{"op":"voice-hangup","id":"request-01","call":"call"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		w := newVoiceWorker(context.Background(), func(e wifiWorkerEvent) {
			if e.VoiceResult == nil || e.VoiceResult.Code != "VOICE_NOT_READY" {
				t.Fatal("unbound controller executed command")
			}
		})
		w.command(raw)
	})
}
