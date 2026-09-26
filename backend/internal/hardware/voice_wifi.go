package hardware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

var ErrVoiceEnded = errors.New("VOICE_ENDED")

type WiFiCall struct {
	session *smsClientSession
	id      string
	wire    *voiceStreamIO
	mu      sync.Mutex
	state   voiceStreamState
	at      time.Time
	dialed  bool
	ended   bool
	changed chan struct{}
	pcm     chan []int16
	closed  chan struct{}
	stop    func() bool
}

func voiceRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func (client *VocatWorkerClient) WiFiVoiceReady(c Candidate, card string) bool {
	s := client.voiceSession(c, card)
	if s == nil || s.ctx.Err() != nil {
		return false
	}
	select {
	case <-s.closed:
		return false
	default:
	}
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

func (client *VocatWorkerClient) OpenWiFiCall(ctx context.Context, c Candidate, card string) (*WiFiCall, error) {
	s := client.voiceSession(c, card)
	id := voiceRequestID()
	r, err := s.exchangeVoice(ctx, voiceCommand{Op: "voice-open", ID: id})
	if err != nil {
		return nil, err
	}
	if r.Media == nil || !r.Media.valid() {
		return nil, errors.New("VOICE_NOT_READY")
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", r.Media.Address)
	if err != nil {
		return nil, errors.New("VOICE_NOT_READY")
	}
	key, _ := hex.DecodeString(r.Media.Key)
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	cancel := context.AfterFunc(ctx, func() { conn.Close() })
	n, err := conn.Write(key)
	var ack [1]byte
	if err == nil && n == len(key) {
		_, err = io.ReadFull(conn, ack[:])
	}
	if !cancel() || ctx.Err() != nil || err != nil || n != len(key) || ack[0] != 1 {
		conn.Close()
		return nil, errors.New("VOICE_NOT_READY")
	}
	_ = conn.SetDeadline(time.Time{})
	v := &WiFiCall{session: s, id: id, wire: &voiceStreamIO{conn: conn}, changed: make(chan struct{}, 1), pcm: make(chan []int16, 3), closed: make(chan struct{})}
	v.stop = context.AfterFunc(s.ctx, func() { conn.Close() })
	go v.receive()
	if err := v.wait(ctx, func(s voiceStreamState) bool { return s.State == "idle" }); err != nil {
		v.Close()
		return nil, err
	}
	return v, nil
}

func (v *WiFiCall) receive() {
	defer close(v.closed)
	defer v.wire.conn.Close()
	for {
		f, err := readVoiceFrame(v.wire.conn)
		if err != nil {
			return
		}
		switch f.kind {
		case voiceStatus:
			var s voiceStreamState
			if json.Unmarshal(f.data, &s) != nil || !s.valid() {
				return
			}
			v.mu.Lock()
			if v.state.Call != "" && s.Call != v.state.Call {
				v.mu.Unlock()
				return
			}
			v.state, v.at = s, time.Now()
			v.mu.Unlock()
			select {
			case v.changed <- struct{}{}:
			default:
			}
		case voicePCM:
			p, err := decodeVoicePCM(f.data)
			if err != nil {
				return
			}
			select {
			case v.pcm <- p:
			default:
				select {
				case <-v.pcm:
				default:
				}
				select {
				case v.pcm <- p:
				default:
				}
			}
		default:
			return
		}
	}
}

func (v *WiFiCall) wait(ctx context.Context, match func(voiceStreamState) bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		v.mu.Lock()
		s := v.state
		v.mu.Unlock()
		if match(s) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-v.closed:
			v.mu.Lock()
			s = v.state
			v.mu.Unlock()
			if match(s) {
				return nil
			}
			return errors.New("VOICE_STREAM_CLOSED")
		case <-v.changed:
		}
	}
}

func (v *WiFiCall) Dial(ctx context.Context, number string) error {
	if !smsRecipient.MatchString(number) {
		return errors.New("VOICE_INVALID_REQUEST")
	}
	v.mu.Lock()
	if v.dialed || v.ended {
		v.mu.Unlock()
		return errors.New("VOICE_STATE")
	}
	v.dialed = true
	v.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.wire.write(voiceDial, []byte(number)); err != nil {
		return err
	}
	return v.wait(ctx, func(s voiceStreamState) bool { return s.State != "" && s.State != "idle" })
}

func (v *WiFiCall) State(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	v.mu.Lock()
	s, at := v.state, v.at
	v.mu.Unlock()
	if s.State == "ended" || s.State == "failed" {
		return "idle", nil
	}
	select {
	case <-v.closed:
		return "", errors.New("VOICE_STREAM_CLOSED")
	default:
	}
	if time.Since(at) > 2*time.Second {
		return "", errors.New("VOICE_STATE_STALE")
	}
	return s.State, nil
}

func (v *WiFiCall) Hangup(ctx context.Context) error {
	v.mu.Lock()
	ended := v.ended
	v.ended = true
	v.mu.Unlock()
	if !ended {
		_ = v.wire.write(voiceEnd, nil)
	}
	err := v.wait(ctx, func(s voiceStreamState) bool { return s.State == "ended" || s.State == "failed" })
	if err == nil {
		return nil
	}
	if v.session.voiceClean.Load() {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A lost media ACK does not release a possibly active carrier call.
	r, check := v.session.exchangeVoice(ctx, voiceCommand{Op: "voice-list", ID: voiceRequestID()})
	if check != nil || r.StreamID == v.id {
		return errors.New("VOICE_ENDING")
	}
	for _, c := range r.Calls {
		if c.State != "ended" && c.State != "failed" {
			return errors.New("VOICE_ENDING")
		}
	}
	return nil
}

func (v *WiFiCall) ReadPCM(ctx context.Context) ([]int16, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-v.closed:
		v.mu.Lock()
		terminal := v.state.State == "ended" && v.state.Call != ""
		v.mu.Unlock()
		if terminal {
			return nil, ErrVoiceEnded
		}
		return nil, io.EOF
	case pcm := <-v.pcm:
		return pcm, nil
	}
}

func (v *WiFiCall) WritePCM(pcm []int16) error {
	v.mu.Lock()
	allowed := !v.ended && v.state.State == "active"
	v.mu.Unlock()
	if !allowed {
		return errors.New("VOICE_STATE")
	}
	select {
	case <-v.closed:
		return io.EOF
	default:
	}

	b, err := encodeVoicePCM(pcm)
	if err != nil {
		return err
	}
	return v.wire.write(voicePCM, b)
}

func (v *WiFiCall) Close() {
	v.stop()
	v.wire.conn.Close()
}

func (v *WiFiCall) SIPCode() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state.Code
}

func (v *WiFiCall) MediaFault() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state.Fault
}
