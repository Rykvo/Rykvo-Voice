package hardware

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

const (
	voiceDial byte = iota + 1
	voiceEnd
	voicePCM
	voiceStatus
	voiceFrameMax   = 1024
	voicePCMSamples = 160
)

// A separate, authenticated local stream keeps MMS storage off the audio path.
type voiceEndpoint struct {
	Address string `json:"address"`
	Key     string `json:"key"`
}

func (e voiceEndpoint) valid() bool {
	name, ok := strings.CutPrefix(e.Address, "@rykvo-voice-pcm-")
	b, err := hex.DecodeString(name)
	k, ke := hex.DecodeString(e.Key)
	return ok && err == nil && len(b) == 16 && ke == nil && len(k) == 32
}

type voiceStreamState struct {
	State string `json:"state"`
	Call  string `json:"call,omitempty"`
	Codec string `json:"codec,omitempty"`
	Code  int    `json:"code,omitempty"`
}

func (s voiceStreamState) valid() bool {
	if s.Code < 0 || s.Code > 699 || (s.Call != "" && !validVoiceID(s.Call)) || (s.Codec != "" && s.Codec != "PCMA" && s.Codec != "PCMU") {
		return false
	}
	switch s.State {
	case "idle", "ended", "failed":
		return true
	case "dialing", "ringing", "ending":
		return s.Call != ""
	case "active":
		return s.Call != "" && s.Codec != ""
	}
	return false
}

type voiceFrame struct {
	kind byte
	data []byte
}

func readVoiceFrame(r io.Reader) (voiceFrame, error) {
	var h [3]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return voiceFrame{}, err
	}
	n := int(binary.BigEndian.Uint16(h[1:]))
	if n > voiceFrameMax || h[0] < voiceDial || h[0] > voiceStatus {
		return voiceFrame{}, errors.New("VOICE_INVALID_FRAME")
	}
	f := voiceFrame{kind: h[0], data: make([]byte, n)}
	_, err := io.ReadFull(r, f.data)
	return f, err
}

type voiceStreamIO struct {
	conn    net.Conn
	writeMu sync.Mutex
}

func (s *voiceStreamIO) write(kind byte, b []byte) error {
	if len(b) > voiceFrameMax {
		return errors.New("VOICE_INVALID_FRAME")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
	frame := make([]byte, 3+len(b))
	frame[0] = kind
	binary.BigEndian.PutUint16(frame[1:3], uint16(len(b)))
	copy(frame[3:], b)
	for len(frame) > 0 {
		n, err := s.conn.Write(frame)
		if err != nil {
			s.conn.Close()
			return err
		}
		if n == 0 {
			s.conn.Close()
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func encodeVoicePCM(pcm []int16) ([]byte, error) {
	if len(pcm) != voicePCMSamples {
		return nil, errors.New("VOICE_INVALID_PCM")
	}
	b := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b, nil
}

func decodeVoicePCM(b []byte) ([]int16, error) {
	if len(b) != voicePCMSamples*2 {
		return nil, errors.New("VOICE_INVALID_PCM")
	}
	p := make([]int16, voicePCMSamples)
	for i := range p {
		p[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return p, nil
}

type voiceMediaController interface {
	voiceController
	CallMedia(context.Context, string) (vowifi.CallMedia, error)
}

type voiceStream struct {
	id         string
	endpoint   voiceEndpoint
	listener   *net.UnixListener
	ctx        context.Context
	stop       context.CancelFunc
	controller voiceMediaController
}

func newVoiceStream(parent context.Context, id string, controller voiceMediaController) (*voiceStream, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("VOICE_NOT_READY")
	}
	var seed [48]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	e := voiceEndpoint{"@rykvo-voice-pcm-" + hex.EncodeToString(seed[:16]), hex.EncodeToString(seed[16:])}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: e.Address, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(parent)
	return &voiceStream{id: id, endpoint: e, listener: l, ctx: ctx, stop: stop, controller: controller}, nil
}

func (s *voiceStream) run() {
	defer s.stop()
	defer s.listener.Close()
	stop := context.AfterFunc(s.ctx, func() { _ = s.listener.Close() })
	defer stop()
	_ = s.listener.SetDeadline(time.Now().Add(6 * time.Second))
	conn, err := s.listener.AcceptUnix()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = s.listener.Close()
	closeConn := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	defer closeConn()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	key, _ := hex.DecodeString(s.endpoint.Key)
	var supplied [32]byte
	if _, err = io.ReadFull(conn, supplied[:]); err != nil || subtle.ConstantTimeCompare(key, supplied[:]) != 1 {
		return
	}
	if _, err = conn.Write([]byte{1}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	wire := &voiceStreamIO{conn: conn}
	s.serve(wire)
}

func (s *voiceStream) serve(wire *voiceStreamIO) {
	ctx, cancel := context.WithCancel(s.ctx)
	var mediaWait sync.WaitGroup
	var callID string
	last := voiceStreamState{State: "idle"}
	status := func() error { b, _ := json.Marshal(last); return wire.write(voiceStatus, b) }
	defer func() {
		cancel()
		mediaWait.Wait()
		// Occupancy remains held until the carrier confirms termination.
		if callID != "" && !s.terminate(callID) {
			return
		}
		if calls, err := s.controller.Calls(); err == nil {
			for _, call := range calls {
				if call.ID == callID {
					last.Code = call.SIPCode
				}
			}
		}
		last.State = "ended"
		_ = status()
	}()
	if status() != nil {
		return
	}
	frames := make(chan voiceFrame, 2)
	failed := make(chan struct{}, 2)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			f, err := readVoiceFrame(wire.conn)
			if err != nil {
				failed <- struct{}{}
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); _ = wire.conn.SetReadDeadline(time.Now()); <-readDone }()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	initial := time.Now()
	var media vowifi.CallMedia
	for {
		select {
		case <-ctx.Done():
			return
		case <-failed:
			return
		case f := <-frames:
			switch f.kind {
			case voiceDial:
				if callID != "" || !smsRecipient.Match(f.data) {
					return
				}
				calls, err := s.controller.Calls()
				if err != nil {
					return
				}
				for _, c := range calls {
					if c.EndedAt == nil {
						return
					}
				}
				c, err := s.controller.DialCall(ctx, string(f.data))
				callID = c.ID
				if err != nil || !validVoiceID(callID) {
					return
				}
				last = voiceStreamState{State: "dialing", Call: callID}
				if status() != nil {
					return
				}
			case voiceEnd:
				if len(f.data) != 0 {
					return
				}
				return
			case voicePCM:
				p, err := decodeVoicePCM(f.data)
				if err != nil || media == nil || media.WritePCM(p) != nil {
					return
				}
			default:
				return
			}
		case <-tick.C:
			if callID == "" {
				if time.Since(initial) > 10*time.Second {
					return
				}
			} else {
				calls, err := s.controller.Calls()
				if err != nil {
					return
				}
				found := false
				for _, c := range calls {
					if c.ID != callID {
						continue
					}
					found = true
					last.Code = c.SIPCode
					if c.EndedAt != nil {
						return
					}
					last.State = c.State
					if last.State == "early_media" {
						last.State = "ringing"
					}
					if c.State == "active" {
						if !c.MediaReady || (c.Codec != "PCMA" && c.Codec != "PCMU") {
							return
						}
						last.Codec = c.Codec
						if media == nil {
							media, err = s.controller.CallMedia(ctx, callID)
							if err != nil {
								return
							}
							mediaWait.Add(1)
							go func(m vowifi.CallMedia) {
								defer mediaWait.Done()
								defer func() { failed <- struct{}{} }()
								for {
									pcm, err := m.ReadPCM(ctx)
									if err != nil || ctx.Err() != nil {
										return
									}
									b, err := encodeVoicePCM(pcm)
									if err != nil || wire.write(voicePCM, b) != nil {
										return
									}
								}
							}(media)
						}
					}
				}
				if !found {
					return
				}
			}
			if !last.valid() || status() != nil {
				return
			}
		}
	}
}

func (s *voiceStream) terminate(id string) bool {
	for s.ctx.Err() == nil {
		ctx, done := context.WithTimeout(s.ctx, 15*time.Second)
		_ = s.controller.HangupCall(ctx, id)
		done()
		calls, check := s.controller.Calls()
		if check == nil {
			active := false
			for _, c := range calls {
				if c.ID == id && c.EndedAt == nil {
					active = true
				}
			}
			if !active {
				return true
			}
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}
