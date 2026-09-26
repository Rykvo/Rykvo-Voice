package hardware

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// One module gate covers AT and its USB NMEA PCM port for the whole call.
type CellularCall struct {
	mu                   sync.Mutex
	at                   *atSession
	pcm                  atPort
	pcmBefore, gpsBefore string
	flow                 atomic.Bool
	closed               atomic.Bool
	writeMu              sync.Mutex
}

func CellularVoiceSupported(c Candidate) bool {
	if !WiFiSupported(c) {
		return false
	}
	return c.Audio != ""
}
func (s *System) OpenCellularCall(ctx context.Context, c Candidate, identity, card string) (*CellularCall, error) {
	if !CellularVoiceSupported(c) || !decimal(card, 18, 20) {
		return nil, errors.New("VOICE_UNSUPPORTED")
	}
	devices, err := s.Discover(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, v := range devices {
		if v.Key == c.Key && v.Generation == c.Generation {
			c = v
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("DEVICE_CHANGED")
	}
	a, err := openWiFiSession(ctx, c, identity)
	if err != nil {
		return nil, err
	}
	v := &CellularCall{at: a}
	failed := true
	defer func() {
		if failed {
			v.Close()
		}
	}()
	lines, err := a.exchange(ctx, "AT+QCCID", 3*time.Second)
	if err != nil || digits(lines, 18, 20) != card {
		return nil, errors.New("DEVICE_CHANGED")
	}
	mode, err := wifiRadioMode(ctx, a)
	if err != nil || mode != 1 {
		return nil, errors.New("VOICE_RADIO_UNAVAILABLE")
	}
	if state, err := v.State(ctx); err != nil || state != "idle" {
		return nil, errors.New("VOICE_BUSY")
	}
	lines, err = a.exchange(ctx, "AT+QPCMV?", 3*time.Second)
	if err != nil {
		return nil, errors.New("VOICE_UNSUPPORTED")
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "+QPCMV:") {
			f := fields(line)
			if len(f) == 1 && f[0] == "0" {
				v.pcmBefore = "0"
			}
			if len(f) == 2 && f[0] == "0" && (f[1] == "0" || f[1] == "1" || f[1] == "2") {
				v.pcmBefore = "0," + f[1]
			}
		}
	}
	if v.pcmBefore == "" {
		return nil, errors.New("VOICE_AUDIO_BUSY")
	}
	lines, err = a.exchange(ctx, `AT+QGPSCFG="outport"`, 3*time.Second)
	if err != nil {
		return nil, errors.New("VOICE_GPS_STATE_UNKNOWN")
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "+QGPSCFG:") {
			f := fields(line)
			if len(f) == 2 && f[0] == "outport" && (f[1] == "none" || f[1] == "usbnmea" || f[1] == "uartdebug") {
				v.gpsBefore = f[1]
			}
		}
	}
	if v.gpsBefore == "" {
		return nil, errors.New("VOICE_GPS_STATE_UNKNOWN")
	}
	v.pcm, err = openAT(c.Audio)
	if err != nil || v.pcm == nil {
		return nil, errors.New("VOICE_AUDIO_BUSY")
	}
	if v.gpsBefore != "none" {
		if _, err = a.exchange(ctx, `AT+QGPSCFG="outport","none"`, 3*time.Second); err != nil {
			failed = false
			return v, err
		}
	}
	if _, err = a.exchange(ctx, "AT+QPCMV=1,0", 3*time.Second); err != nil {
		failed = false
		return v, err
	}
	v.flow.Store(true)
	a.onLine = func(line string) {
		if line == "+QPCMV: 0" {
			v.flow.Store(false)
		}
		if line == "+QPCMV: 1" {
			v.flow.Store(true)
		}
	}
	failed = false
	return v, nil
}
func (v *CellularCall) Dial(ctx context.Context, number string) error {
	if !smsRecipient.MatchString(number) {
		return errors.New("VOICE_INVALID_NUMBER")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx.Err() != nil || v.closed.Load() {
		return context.Canceled
	}
	_, err := v.at.exchange(ctx, "ATD"+number+";", 8*time.Second)
	return err
}
func cellularCallState(lines []string) (string, error) {
	state := "idle"
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		f := fields(line)
		if len(f) < 5 {
			return "", errors.New("VOICE_STATE_UNKNOWN")
		}
		// EC20 also lists LTE packet-data contexts; they are not voice calls.
		if f[3] == "1" && len(f) >= 7 && f[5] == "" && f[6] == "128" {
			continue
		}
		if f[3] != "0" {
			return "", errors.New("VOICE_STATE_UNKNOWN")
		}
		switch f[2] {
		case "0":
			state = "active"
		case "2":
			if state != "active" {
				state = "dialing"
			}
		case "3":
			if state != "active" {
				state = "ringing"
			}
		case "4", "5":
			return "incoming", nil
		default:
			return "", errors.New("VOICE_STATE_UNKNOWN")
		}
	}
	return state, nil
}
func (v *CellularCall) State(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return "", io.EOF
	}
	lines, err := v.at.exchange(ctx, "AT+CLCC", 2*time.Second)
	if err != nil {
		return "", err
	}
	return cellularCallState(lines)
}
func (v *CellularCall) Hangup(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return io.EOF
	}
	// Some firmware returns ERROR if the peer already ended the call.
	// The following CLCC confirmation, not that return code, decides cleanup.
	_, _ = v.at.exchange(ctx, "AT+CHUP", 5*time.Second)
	lines, err := v.at.exchange(ctx, "AT+CLCC", 3*time.Second)
	state, stateErr := cellularCallState(lines)
	if err != nil || stateErr != nil || state != "idle" {
		return errors.New("VOICE_HANGUP_UNCONFIRMED")
	}
	if _, err = v.at.exchange(ctx, "AT+QPCMV="+v.pcmBefore, 3*time.Second); err != nil {
		return err
	}
	if _, err = v.at.exchange(ctx, `AT+QGPSCFG="outport","`+v.gpsBefore+`"`, 3*time.Second); err != nil {
		return err
	}
	return nil
}
func (v *CellularCall) ReadPCM(ctx context.Context) ([]int16, error) {
	buf := make([]byte, 320)
	for offset := 0; offset < len(buf); {
		if ctx.Err() != nil || v.closed.Load() {
			return nil, io.EOF
		}
		n, err := v.pcm.Read(buf[offset:])
		if err != nil {
			return nil, err
		}
		offset += n
	}
	pcm := make([]int16, len(buf)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
	}
	return pcm, nil
}
func (v *CellularCall) WritePCM(pcm []int16) error {
	if len(pcm) > 1600 || v.closed.Load() {
		return io.EOF
	}
	if !v.flow.Load() {
		return nil
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	buf := make([]byte, len(pcm)*2)
	for i, n := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(n))
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for len(buf) > 0 {
		if v.closed.Load() || time.Now().After(deadline) {
			return errors.New("VOICE_AUDIO_STALLED")
		}
		n, err := v.pcm.Write(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	return nil
}
func (v *CellularCall) Close() {
	if !v.closed.Swap(true) {
		if v.pcm != nil {
			v.pcm.Close()
		}
		if v.at != nil {
			v.at.port.Close()
		}
	}
}
