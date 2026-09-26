package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

// Control only. IMS registration does not imply that a client audio path exists.
type VoiceCall struct {
	ID         string `json:"id"`
	Number     string `json:"number,omitempty"`
	Direction  string `json:"direction"`
	State      string `json:"state"`
	MediaReady bool   `json:"mediaReady"`
	Codec      string `json:"codec,omitempty"`
	SIPCode    int    `json:"sipCode,omitempty"`
}

type voiceCommand struct {
	Op     string `json:"op"`
	ID     string `json:"id"`
	Call   string `json:"call,omitempty"`
	Number string `json:"number,omitempty"`
}
type voiceReply struct {
	ID    string      `json:"id"`
	Calls []VoiceCall `json:"calls,omitempty"`
	Code  string      `json:"code,omitempty"`
}
type voiceController interface {
	Calls() ([]vowifi.Call, error)
	DialCall(context.Context, string) (vowifi.Call, error)
	AnswerCall(context.Context, string) (vowifi.Call, error)
	HangupCall(context.Context, string) error
}
type voiceWorker struct {
	ctx        context.Context
	emit       func(wifiWorkerEvent)
	mu         sync.Mutex
	controller voiceController
	busy       bool
	used       map[string]bool
	wait       sync.WaitGroup
}

func newVoiceWorker(ctx context.Context, emit func(wifiWorkerEvent)) *voiceWorker {
	return &voiceWorker{ctx: ctx, emit: emit, used: map[string]bool{}}
}
func (w *voiceWorker) setController(c voiceController) {
	w.mu.Lock()
	w.controller = c
	w.mu.Unlock()
}
func validVoiceID(id string) bool {
	return len(id) > 0 && len(id) <= 512 && !strings.ContainsAny(id, "\x00\r\n\t ")
}
func (c voiceCommand) valid() bool {
	if !smsRequestID.MatchString(c.ID) {
		return false
	}
	switch c.Op {
	case "voice-list":
		return c.Call == "" && c.Number == ""
	case "voice-dial":
		return c.Call == "" && smsRecipient.MatchString(c.Number)
	case "voice-answer", "voice-hangup":
		return validVoiceID(c.Call) && c.Number == ""
	}
	return false
}
func voiceCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, vowifi.ErrNotRunning):
		return "VOICE_NOT_READY"
	case errors.Is(err, ims.ErrCallNotFound):
		return "VOICE_NOT_FOUND"
	case errors.Is(err, ims.ErrCallState):
		return "VOICE_STATE"
	case errors.Is(err, ims.ErrCallEnding):
		return "VOICE_ENDING"
	default:
		return "VOICE_OUTCOME_UNKNOWN"
	}
}
func validVoiceCode(code string) bool {
	switch code {
	case "", "VOICE_NOT_READY", "VOICE_NOT_FOUND", "VOICE_STATE", "VOICE_ENDING", "VOICE_OUTCOME_UNKNOWN", "VOICE_BUSY", "VOICE_DUPLICATE_REQUEST":
		return true
	}
	return false
}
func publicVoiceCall(c vowifi.Call) (VoiceCall, bool) {
	v := VoiceCall{ID: c.ID, Number: c.Number, Direction: c.Direction, State: c.State, MediaReady: c.MediaReady, Codec: c.Codec, SIPCode: c.SIPCode}
	if !smsRecipient.MatchString(v.Number) {
		v.Number = ""
	}
	return v, v.valid()
}
func (c VoiceCall) valid() bool {
	if !validVoiceID(c.ID) || (c.Number != "" && !smsRecipient.MatchString(c.Number)) ||
		(c.Direction != "incoming" && c.Direction != "outgoing") || c.SIPCode < 0 || c.SIPCode > 699 {
		return false
	}
	if c.Codec != "" && c.Codec != "PCMA" && c.Codec != "PCMU" {
		return false
	}
	switch c.State {
	case "dialing", "ringing", "early_media", "active", "ending", "ended", "failed":
	default:
		return false
	}
	return !c.MediaReady || (c.Codec != "" && (c.State == "active" || c.State == "early_media"))
}
func (w *voiceWorker) command(raw []byte) bool {
	if len(raw) > 2048 {
		return false
	}
	var c voiceCommand
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || !c.valid() {
		return false
	}
	w.mu.Lock()
	controller := w.controller
	code := ""
	if w.ctx.Err() != nil || controller == nil {
		code = "VOICE_NOT_READY"
	} else if w.busy {
		code = "VOICE_BUSY"
	} else if c.Op != "voice-list" && (w.used[c.ID] || len(w.used) >= 10000) {
		code = "VOICE_DUPLICATE_REQUEST"
	}
	if code != "" {
		w.mu.Unlock()
		w.emit(wifiWorkerEvent{VoiceResult: &voiceReply{ID: c.ID, Code: code}})
		return true
	}
	if c.Op != "voice-list" {
		w.used[c.ID] = true
	}
	w.busy = true
	w.wait.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wait.Done()
		defer func() { w.mu.Lock(); w.busy = false; w.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
		defer cancel()
		calls, err := controller.Calls()
		if err == nil {
			switch c.Op {
			case "voice-dial":
				for _, call := range calls {
					if call.EndedAt == nil {
						code = "VOICE_BUSY"
						break
					}
				}
				if code == "" {
					var call vowifi.Call
					call, err = controller.DialCall(ctx, c.Number)
					calls = []vowifi.Call{call}
				}
			case "voice-answer":
				var call vowifi.Call
				call, err = controller.AnswerCall(ctx, c.Call)
				calls = []vowifi.Call{call}
			case "voice-hangup":
				err = controller.HangupCall(ctx, c.Call)
				if err == nil {
					calls, err = controller.Calls()
				}
			}
		}
		if code == "" {
			code = voiceCode(err)
		}
		r := voiceReply{ID: c.ID, Code: code}
		if code == "" {
			if len(calls) > 64 {
				r.Code = "VOICE_OUTCOME_UNKNOWN"
			} else {
				for _, call := range calls {
					v, ok := publicVoiceCall(call)
					if !ok {
						r.Calls = nil
						r.Code = "VOICE_OUTCOME_UNKNOWN"
						break
					}
					r.Calls = append(r.Calls, v)
				}
			}
		}
		w.emit(wifiWorkerEvent{VoiceResult: &r})
	}()
	return true
}

func (s *smsClientSession) voiceEvent(r voiceReply) bool {
	if !smsRequestID.MatchString(r.ID) || !validVoiceCode(r.Code) || len(r.Calls) > 64 || (r.Code != "" && len(r.Calls) > 0) {
		return false
	}
	for _, c := range r.Calls {
		if !c.valid() {
			return false
		}
	}
	s.mu.Lock()
	ch := s.voicePending[r.ID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
			return false
		}
	}
	return true
}
func (client *VocatWorkerClient) exchangeVoice(ctx context.Context, c Candidate, card string, request voiceCommand) ([]VoiceCall, error) {
	if !request.valid() {
		return nil, errors.New("VOICE_INVALID_REQUEST")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	client.smsMu.Lock()
	s := client.smsSessions[smsSessionKey(c, card)]
	client.smsMu.Unlock()
	if s == nil {
		return nil, errors.New("VOICE_NOT_READY")
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, errors.New("VOICE_NOT_READY")
	}
	ch := make(chan voiceReply, 1)
	s.mu.Lock()
	if len(s.voicePending) != 0 {
		s.mu.Unlock()
		return nil, errors.New("VOICE_BUSY")
	}
	if s.voicePending == nil {
		s.voicePending = map[string]chan voiceReply{}
	}
	s.voicePending[request.ID] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.voicePending, request.ID); s.mu.Unlock() }()
	s.writeMu.Lock()
	if ctx.Err() != nil {
		s.writeMu.Unlock()
		return nil, ctx.Err()
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := json.NewEncoder(s.conn).Encode(request)
	s.writeMu.Unlock()
	if err != nil {
		return nil, errors.New("VOICE_OUTCOME_UNKNOWN")
	}
	select {
	case r := <-ch:
		if r.Code != "" {
			return nil, errors.New(r.Code)
		}
		return r.Calls, nil
	case <-ctx.Done():
		return nil, errors.New("VOICE_OUTCOME_UNKNOWN")
	case <-s.closed:
		return nil, errors.New("VOICE_OUTCOME_UNKNOWN")
	}
}
func (client *VocatWorkerClient) WiFiCalls(ctx context.Context, c Candidate, card, id string) ([]VoiceCall, error) {
	return client.exchangeVoice(ctx, c, card, voiceCommand{Op: "voice-list", ID: id})
}
func (client *VocatWorkerClient) DialWiFiCall(ctx context.Context, c Candidate, card, id, number string) ([]VoiceCall, error) {
	return client.exchangeVoice(ctx, c, card, voiceCommand{Op: "voice-dial", ID: id, Number: number})
}
func (client *VocatWorkerClient) AnswerWiFiCall(ctx context.Context, c Candidate, card, id, call string) ([]VoiceCall, error) {
	return client.exchangeVoice(ctx, c, card, voiceCommand{Op: "voice-answer", ID: id, Call: call})
}
func (client *VocatWorkerClient) HangupWiFiCall(ctx context.Context, c Candidate, card, id, call string) ([]VoiceCall, error) {
	return client.exchangeVoice(ctx, c, card, voiceCommand{Op: "voice-hangup", ID: id, Call: call})
}
