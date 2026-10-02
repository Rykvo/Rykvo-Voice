package hardware

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// One module gate covers AT and its USB NMEA PCM port for the whole call.
type CellularCall struct {
	mu          sync.Mutex
	at          *atSession
	pcm         atPort
	audio       voiceAudioState
	journal     string
	flow        atomic.Bool
	flowDropped atomic.Uint64
	failure     atomic.Pointer[string]
	closed      atomic.Bool
	writeMu     sync.Mutex
	incoming    string
	caller      string
	dialed      bool
	connected   bool
	releaseRead bool
	reconnect   bool
	detached    bool
	reopen      func(context.Context) (*atSession, atPort, error)
}

func CellularVoiceSupported(c Candidate) bool {
	if !WiFiSupported(c) {
		return false
	}
	return c.Audio != ""
}
func (s *System) OpenCellularCall(ctx context.Context, c Candidate, identity, card string) (*CellularCall, error) {
	return s.openCellularVoice(ctx, c, identity, card, false)
}

// A nil call with no error means no incoming voice call. The caller holds the module gate.
func (s *System) OpenCellularIncoming(ctx context.Context, c Candidate, identity, card string) (*CellularCall, error) {
	return s.openCellularVoice(ctx, c, identity, card, true)
}
func (v *CellularCall) Caller() string { return v.caller }

func (s *System) openCellularVoice(ctx context.Context, c Candidate, identity, card string, incoming bool) (*CellularCall, error) {
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
	if incoming {
		// Never run crash recovery (CHUP) over a newly observed call.
		if _, err := os.Stat(s.voiceStatePath(identity)); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("VOICE_AUDIO_RESTORE_PENDING")
		}
	} else {
		if err = s.recoverVoiceAudio(ctx, c, identity); err != nil {
			return nil, err
		}
	}
	a, err := openWiFiSession(ctx, c, identity)
	if err != nil {
		return nil, err
	}
	v := &CellularCall{at: a}
	v.reopen = func(ctx context.Context) (*atSession, atPort, error) {
		found, err := s.Discover(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, next := range found {
			if next.Key != c.Key || !CellularVoiceSupported(next) {
				continue
			}
			// USB enumeration may change ports; the original IMEI must still match.
			at, err := openWiFiSession(ctx, next, identity)
			if err != nil {
				return nil, nil, err
			}
			pcm, err := openAT(next.Audio)
			if err != nil {
				at.port.Close()
				return nil, nil, err
			}
			v.detached = v.detached || next.Generation != c.Generation
			return at, pcm, nil
		}
		return nil, nil, errors.New("DEVICE_CHANGED")
	}
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
	if incoming {
		lines, err := a.exchange(ctx, "AT+CLCC", 2*time.Second)
		if err != nil {
			return nil, err
		}
		calls, err := cellularCalls(lines)
		if err != nil {
			return nil, err
		}
		if len(calls) != 1 || calls[0].state != "4" || calls[0].direction != "1" {
			return nil, nil
		}
		v.incoming, v.caller = calls[0].id, calls[0].number
	} else {
		if state, err := v.State(ctx); err != nil || state != "idle" {
			return nil, errors.New("VOICE_BUSY")
		}
	}
	v.audio = voiceAudioState{Identity: identity, Key: c.Key}
	v.audio.PCM, err = readVoiceAudio(ctx, a, "pcm")
	if err != nil {
		return nil, errors.New("VOICE_UNSUPPORTED")
	}
	if !strings.HasPrefix(v.audio.PCM, "0") {
		return nil, errors.New("VOICE_AUDIO_BUSY")
	}
	v.audio.GPS, err = readVoiceAudio(ctx, a, "gps")
	if err != nil {
		return nil, errors.New("VOICE_GPS_STATE_UNKNOWN")
	}
	v.pcm, err = openAT(c.Audio)
	if err != nil || v.pcm == nil {
		return nil, errors.New("VOICE_AUDIO_BUSY")
	}
	stop := discardCellularPCM(ctx, v.pcm)
	defer stop()
	err = s.prepareVoiceAudio(ctx, v)
	if v.journal == "" {
		return nil, err
	}
	failed = false
	if err != nil {
		return v, err
	}
	v.flow.Store(true)
	a.onLine = v.voiceLine
	return v, nil
}

func (v *CellularCall) voiceLine(line string) {
	if reason := voiceATResult(line); reason != "" {
		v.failure.Store(&reason)
	}
	if line == "+QPCMV: 0" {
		v.flow.Store(false)
	}
	if line == "+QPCMV: 1" {
		v.flow.Store(true)
	}
}
func (v *CellularCall) Dial(ctx context.Context, number string) error {
	if v.incoming != "" || !smsRecipient.MatchString(number) {
		return errors.New("VOICE_INVALID_NUMBER")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx.Err() != nil || v.closed.Load() {
		return context.Canceled
	}
	if v.dialed {
		return errors.New("VOICE_STATE")
	}
	v.dialed = true
	_, err := v.at.exchange(ctx, "ATD"+number+";", 8*time.Second)
	if err != nil && ctx.Err() == nil && (err.Error() == "CALL_ENDED" || err.Error() == "COMMAND_UNSUPPORTED") {
		v.readReleaseCause(ctx)
	}
	return err
}

type cellularVoiceCall struct{ id, direction, state, number string }

func cellularCalls(lines []string) ([]cellularVoiceCall, error) {
	var result []cellularVoiceCall
	seen := map[string]bool{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		f := fields(line)
		if len(f) < 5 {
			return nil, errors.New("VOICE_STATE_UNKNOWN")
		}
		if f[3] == "1" && len(f) >= 7 && f[5] == "" && f[6] == "128" {
			continue
		}
		n, err := strconv.Atoi(f[0])
		if err != nil || n < 1 || n > 7 || strconv.Itoa(n) != f[0] || seen[f[0]] || f[3] != "0" || (f[1] != "0" && f[1] != "1") || len(f[2]) != 1 || f[2][0] < '0' || f[2][0] > '5' {
			return nil, errors.New("VOICE_STATE_UNKNOWN")
		}
		seen[f[0]] = true
		c := cellularVoiceCall{id: f[0], direction: f[1], state: f[2]}
		if len(f) >= 7 && smsRecipient.MatchString(f[5]) {
			c.number = f[5]
			if f[6] == "145" && !strings.HasPrefix(c.number, "+") {
				c.number = "+" + c.number
			}
		}
		result = append(result, c)
	}
	return result, nil
}

func (v *CellularCall) Answer(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() || v.incoming == "" {
		return errors.New("VOICE_STATE")
	}
	lines, err := v.at.exchange(ctx, "AT+CLCC", 2*time.Second)
	if err != nil {
		return err
	}
	calls, err := cellularCalls(lines)
	if err != nil || len(calls) != 1 || calls[0].id != v.incoming || calls[0].state != "4" || calls[0].direction != "1" {
		return errors.New("VOICE_STATE")
	}
	_, err = v.at.exchange(ctx, "ATA", 8*time.Second)
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
			if state == "idle" {
				state = "incoming"
			}
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
	calls, err := cellularCalls(lines)
	if err != nil {
		return "", err
	}
	// Reject call waiting without touching the established voice call.
	for _, call := range calls {
		if call.state == "5" {
			_, _ = v.at.exchange(ctx, "AT+CHLD=1"+call.id, 3*time.Second)
		}
	}
	if v.incoming != "" {
		for _, call := range calls {
			if call.id == v.incoming {
				switch call.state {
				case "0":
					return "active", nil
				case "4":
					return "ringing", nil
				}
				return "", errors.New("VOICE_STATE_UNKNOWN")
			}
		}
		return "idle", nil
	}
	state, err := cellularCallState(lines)
	if state == "active" {
		v.connected = true
	}
	if err == nil && state == "idle" && v.dialed && !v.connected && ctx.Err() == nil {
		v.readReleaseCause(ctx)
	}
	return state, err
}
func (v *CellularCall) Hangup(ctx context.Context) (err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return io.EOF
	}
	defer func() { v.reconnect = err != nil }()
	if v.reconnect && v.reopen != nil {
		// Callers have already stopped media; keep the gate and recovery journal
		// until a freshly identity-checked session confirms hangup and restoration.
		if v.at != nil {
			v.at.port.Close()
			v.at = nil
		}
		if v.pcm != nil {
			v.pcm.Close()
			v.pcm = nil
		}
		v.at, v.pcm, err = v.reopen(ctx)
		if err != nil {
			return err
		}
		v.at.onLine = v.voiceLine
	}
	stop := discardCellularPCM(ctx, v.pcm)
	defer stop()
	// Some firmware returns ERROR if the peer already ended the call.
	// The following CLCC confirmation, not that return code, decides cleanup.
	if v.detached {
		// IDs may be reused after USB reconnect. Do not hang up a new call;
		// only restore audio once this same module reports voice idle.
	} else if v.incoming != "" {
		lines, err := v.at.exchange(ctx, "AT+CLCC", 3*time.Second)
		calls, parseErr := cellularCalls(lines)
		if err != nil || parseErr != nil {
			return errors.New("VOICE_HANGUP_UNCONFIRMED")
		}
		for _, call := range calls {
			if call.id == v.incoming {
				command := "AT+CHLD=1" + call.id
				if len(calls) == 1 {
					command = "AT+CHUP"
				}
				_, _ = v.at.exchange(ctx, command, 5*time.Second)
			}
		}
	} else {
		_, _ = v.at.exchange(ctx, "AT+CHUP", 5*time.Second)
	}
	lines, err := v.at.exchange(ctx, "AT+CLCC", 3*time.Second)
	state, stateErr := cellularCallState(lines)
	if err != nil || stateErr != nil || state != "idle" {
		return errors.New("VOICE_HANGUP_UNCONFIRMED")
	}
	return v.restoreAudio(ctx)
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
		v.flowDropped.Add(uint64(len(pcm)))
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
func (v *CellularCall) DroppedPCMSamples() uint64 { return v.flowDropped.Load() }
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

func voiceATResult(line string) string {
	switch line {
	case "BUSY":
		return "busy"
	case "NO ANSWER":
		return "no_answer"
	}
	if strings.HasPrefix(line, "+CME ERROR:") {
		switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "+CME ERROR:"))) {
		case "10", "11", "12", "13", "15", "17", "18", "sim not inserted", "sim pin required", "sim puk required", "sim failure", "sim wrong", "sim pin2 required", "sim puk2 required":
			return "card_error"
		case "14", "sim busy":
			return "module_busy"
		case "30", "no network service":
			return "not_registered"
		case "31", "network timeout":
			return "carrier_unavailable"
		case "32", "network not allowed - emergency calls only":
			return "carrier_rejected"
		}
	}
	return ""
}

// Called under the call's hardware gate, only after a confirmed failed setup.
func (v *CellularCall) readReleaseCause(ctx context.Context) {
	if v.releaseRead || v.FailureReason() != "" {
		return
	}
	v.releaseRead = true
	reason := "dial_failed"
	if lines, err := v.at.exchange(ctx, "AT+CEER", time.Second); err == nil {
		for _, line := range lines {
			if r := voiceReleaseResult(line); r != "" {
				reason = r
				break
			}
		}
	}
	// Keep a terminal BUSY/NO ANSWER observed while reading the report.
	if v.FailureReason() == "" && ctx.Err() == nil {
		v.failure.Store(&reason)
	}
}

// Quectel CEER text causes; numeric-only or unknown reports do not prove barring.
func voiceReleaseResult(line string) string {
	if !strings.HasPrefix(line, "+CEER:") || len(line) > 256 {
		return ""
	}
	report := strings.ToLower(strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "+CEER:")), `"`))
	switch report {
	case "user busy":
		return "busy"
	case "no user responding", "user alerting, no answer":
		return "no_answer"
	case "call rejected":
		return "rejected"
	case "normal call clearing", "normal, unspecified", "client ended call", "network ended call", "non selected user clearing":
		return "remote_cancelled"
	case "unassigned/unallocated number", "invalid/incomplete number", "number changed":
		return "number_not_found"
	case "destination out of order", "no route to destination":
		return "peer_unavailable"
	case "operator determined barring", "no funds available", "requested facility not subscribed", "requested service option not subscribed", "bearer capability not authorized", "access class blocked":
		return "carrier_rejected"
	case "no service", "no service available", "no cell available":
		return "not_registered"
	case "invalid sim", "uim not present":
		return "card_error"
	case "network out of order", "temporary failure", "switching equipment congestion", "no circuit/channel available", "no response received from network", "network failure":
		return "carrier_unavailable"
	}
	return ""
}
func (v *CellularCall) FailureReason() string {
	if p := v.failure.Load(); p != nil {
		return *p
	}
	return ""
}
