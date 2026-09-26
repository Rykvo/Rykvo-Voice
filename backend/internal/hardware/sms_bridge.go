package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"rykvo.local/auth/internal/vocat/device"
	"rykvo.local/auth/internal/vocat/vowifi"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

// Only subscriber message content crosses this private channel, never AKA/IMSI.
type SMSReport struct {
	Reference int `json:"reference"`
	Code      int `json:"code"`
}
type SMSDelivery struct {
	ID          string                `json:"id"`
	From        string                `json:"from"`
	Text        string                `json:"text"`
	At          time.Time             `json:"at"`
	SCTS        *time.Time            `json:"scts,omitempty"`
	Encoding    string                `json:"encoding,omitempty"`
	Concat      *device.SMSConcatInfo `json:"concat,omitempty"`
	TPDU        string                `json:"tpdu"`
	DecodeError string                `json:"decodeError,omitempty"`
	Status      *SMSReport            `json:"status,omitempty"`
}
type SMSReply struct {
	ID     string                 `json:"id"`
	Result vowifi.SMSSubmitResult `json:"result"`
	Code   string                 `json:"code,omitempty"`
}
type smsCommand struct {
	Op     string `json:"op"`
	ID     string `json:"id"`
	To     string `json:"to,omitempty"`
	Text   string `json:"text,omitempty"`
	Stored bool   `json:"stored,omitempty"`
}

var smsRecipient = regexp.MustCompile(`^\+?[0-9]{3,15}$`)
var smsRequestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

func ValidSMS(to, text string) bool {
	return smsRecipient.MatchString(to) && utf8.ValidString(text) && len(text) > 0 && len(text) <= 4096 && !bytes.ContainsRune([]byte(text), 0)
}
func smsSessionKey(c Candidate, iccid string) string {
	return c.Key + "\x00" + c.Generation + "\x00" + iccid
}

type smsEventEncoder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (e *smsEventEncoder) Encode(v any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(v)
}

type smsClientSession struct {
	ctx     context.Context
	conn    net.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan SMSReply
	closed  chan struct{}
	ready   chan struct{}
	once    sync.Once
	receive func(context.Context, SMSDelivery) error
}

func newSMSClientSession(ctx context.Context, c net.Conn, receive func(context.Context, SMSDelivery) error) *smsClientSession {
	return &smsClientSession{ctx: ctx, conn: c, pending: map[string]chan SMSReply{}, closed: make(chan struct{}), ready: make(chan struct{}), receive: receive}
}
func (s *smsClientSession) close() { s.once.Do(func() { close(s.closed) }) }
func (s *smsClientSession) write(c smsCommand) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return json.NewEncoder(s.conn).Encode(c)
}
func (s *smsClientSession) event(e wifiWorkerEvent) bool {
	if e.SMSResult != nil {
		s.mu.Lock()
		ch := s.pending[e.SMSResult.ID]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- *e.SMSResult:
			default:
			}
		}
		return true
	}
	v := e.SMS
	if v == nil || len(v.ID) == 0 || len(v.ID) > 512 || len(v.TPDU) > 1024 || len(v.Text) > 4096 || len(v.From) > 128 {
		return false
	}
	// The carrier receives RP-ACK only after the database confirms durable storage.
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	stored := s.receive != nil && s.receive(ctx, *v) == nil
	return s.write(smsCommand{Op: "sms-ack", ID: v.ID, Stored: stored}) == nil
}
func (client *VocatWorkerClient) SendSMS(ctx context.Context, c Candidate, iccid, id, to, text string) (vowifi.SMSSubmitResult, error) {
	if !ValidSMS(to, text) || !smsRequestID.MatchString(id) {
		return vowifi.SMSSubmitResult{}, errors.New("INVALID_MESSAGE")
	}
	client.smsMu.Lock()
	s := client.smsSessions[smsSessionKey(c, iccid)]
	client.smsMu.Unlock()
	if s == nil {
		return vowifi.SMSSubmitResult{}, errors.New("SMS_NOT_READY")
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return vowifi.SMSSubmitResult{}, errors.New("SMS_NOT_READY")
	case <-s.closed:
		return vowifi.SMSSubmitResult{}, errors.New("SMS_NOT_READY")
	}
	ch := make(chan SMSReply, 1)
	s.mu.Lock()
	if len(s.pending) > 0 {
		s.mu.Unlock()
		return vowifi.SMSSubmitResult{}, errors.New("SMS_BUSY")
	}
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	if s.write(smsCommand{Op: "sms-send", ID: id, To: to, Text: text}) != nil {
		return vowifi.SMSSubmitResult{}, errors.New("SMS_OUTCOME_UNKNOWN")
	}
	select {
	case r := <-ch:
		if r.Code != "" {
			return r.Result, errors.New(r.Code)
		}
		return r.Result, nil
	case <-ctx.Done():
		return vowifi.SMSSubmitResult{}, errors.New("SMS_OUTCOME_UNKNOWN")
	case <-s.closed:
		return vowifi.SMSSubmitResult{}, errors.New("SMS_OUTCOME_UNKNOWN")
	}
}

type smsSender func(context.Context, vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error)
type smsWorker struct {
	ctx     context.Context
	encoder *smsEventEncoder
	cancel  context.CancelFunc
	mu      sync.Mutex
	sender  smsSender
	acks    map[string]chan bool
	busy    bool
	used    map[string]bool
}

func newSMSWorker(ctx context.Context, e *smsEventEncoder, cancel context.CancelFunc) *smsWorker {
	return &smsWorker{ctx: ctx, encoder: e, cancel: cancel, acks: map[string]chan bool{}, used: map[string]bool{}}
}
func (w *smsWorker) setSender(f smsSender) { w.mu.Lock(); w.sender = f; w.mu.Unlock() }
func (w *smsWorker) emit(e wifiWorkerEvent) {
	if w.encoder.Encode(e) != nil {
		w.cancel()
	}
}
func (w *smsWorker) receive(ctx context.Context, v SMSDelivery) error {
	if len(v.ID) == 0 || len(v.ID) > 512 || len(v.TPDU) > 1024 || len(v.Text) > 4096 || len(v.From) > 128 {
		return errors.New("INVALID_MESSAGE")
	}
	ch := make(chan bool, 1)
	w.mu.Lock()
	if len(w.acks) >= 16 || w.acks[v.ID] != nil {
		w.mu.Unlock()
		return errors.New("SMS_STORAGE_BUSY")
	}
	w.acks[v.ID] = ch
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.acks, v.ID); w.mu.Unlock() }()
	w.emit(wifiWorkerEvent{SMS: &v})
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case stored := <-ch:
		if stored {
			return nil
		}
	case <-ctx.Done():
	case <-w.ctx.Done():
	case <-timer.C:
	}
	return errors.New("SMS_STORAGE_UNCONFIRMED")
}
func (w *smsWorker) command(b []byte) bool {
	var c smsCommand
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return false
	}
	if c.Op == "sms-ack" {
		if len(c.ID) > 512 || c.To != "" || c.Text != "" {
			return false
		}
		w.mu.Lock()
		ch := w.acks[c.ID]
		w.mu.Unlock()
		if ch != nil {
			select {
			case ch <- c.Stored:
			default:
			}
		}
		return true
	}
	if c.Op != "sms-send" || !smsRequestID.MatchString(c.ID) || !ValidSMS(c.To, c.Text) || c.Stored {
		return false
	}
	w.mu.Lock()
	f := w.sender
	code := ""
	if w.busy {
		code = "SMS_BUSY"
	} else if f == nil {
		code = "SMS_NOT_READY"
	} else if w.used[c.ID] || len(w.used) >= 10000 {
		code = "SMS_DUPLICATE_REQUEST"
	}
	if code != "" {
		w.mu.Unlock()
		w.emit(wifiWorkerEvent{SMSResult: &SMSReply{ID: c.ID, Code: code}})
		return true
	}
	w.busy = true
	w.used[c.ID] = true
	w.mu.Unlock()
	go func() {
		defer func() { w.mu.Lock(); w.busy = false; w.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(w.ctx, 90*time.Second)
		defer cancel()
		result, err := f(ctx, vowifi.SMSSubmitRequest{Recipient: c.To, Text: c.Text})
		code := ""
		if errors.Is(err, vowifi.ErrSMSNotReady) {
			code = "SMS_NOT_READY"
		} else if errors.Is(err, ims.ErrSMSRejected) {
			code = "SMS_REJECTED"
		} else if err != nil {
			code = "SMS_OUTCOME_UNKNOWN"
		}
		w.emit(wifiWorkerEvent{SMSResult: &SMSReply{ID: c.ID, Result: result, Code: code}})
	}()
	return true
}
