package hardware

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type voiceAudioState struct {
	Identity string `json:"identity"`
	Key      string `json:"key"`
	PCM      string `json:"pcm"`
	GPS      string `json:"gps"`
	Mic      string `json:"mic"`
}

func voiceIdentityValid(identity string) bool {
	if !strings.HasPrefix(identity, "imei:") || len(identity) != 69 {
		return false
	}
	_, err := hex.DecodeString(identity[5:])
	return err == nil
}

func voiceAudioValue(kind, value string) bool {
	f := strings.Split(value, ",")
	switch kind {
	case "pcm":
		return (len(f) == 1 || len(f) == 2 && (f[1] == "0" || f[1] == "1" || f[1] == "2")) && (f[0] == "0" || f[0] == "1")
	case "gps":
		return value == "none" || value == "usbnmea" || value == "uartdebug"
	case "mic":
		if len(f) != 2 {
			return false
		}
		for _, v := range f {
			n, err := strconv.ParseUint(v, 10, 16)
			if err != nil || strconv.FormatUint(n, 10) != v {
				return false
			}
		}
		return true
	}
	return false
}

func voiceAudioCommand(kind string) (query, prefix, set string) {
	switch kind {
	case "pcm":
		return "AT+QPCMV?", "+QPCMV:", "AT+QPCMV="
	case "mic":
		return "AT+QMIC?", "+QMIC:", "AT+QMIC="
	default:
		return `AT+QGPSCFG="outport"`, "+QGPSCFG:", `AT+QGPSCFG="outport",`
	}
}

func readVoiceAudio(ctx context.Context, a *atSession, kind string) (string, error) {
	query, prefix, _ := voiceAudioCommand(kind)
	lines, err := a.exchange(ctx, query, 3*time.Second)
	if err != nil {
		return "", err
	}
	value := ""
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		f := fields(line)
		if kind == "gps" && len(f) == 2 && f[0] == "outport" {
			f = f[1:]
		}
		next := strings.Join(f, ",")
		// QPCMV flow URCs have one field; prefer the full query response.
		if kind == "pcm" && value != "" && strings.Contains(value, ",") && len(f) == 1 {
			continue
		}
		if kind == "pcm" && !strings.Contains(value, ",") && len(f) == 2 {
			value = ""
		}
		if value != "" || !voiceAudioValue(kind, next) {
			return "", errors.New("VOICE_AUDIO_STATE_UNKNOWN")
		}
		value = next
	}
	if value == "" {
		return "", errors.New("VOICE_AUDIO_STATE_UNKNOWN")
	}
	return value, nil
}

func setVoiceAudio(ctx context.Context, a *atSession, kind, value string) error {
	if !voiceAudioValue(kind, value) {
		return errors.New("VOICE_AUDIO_STATE_INVALID")
	}
	current, err := readVoiceAudio(ctx, a, kind)
	if err != nil || current == value {
		return err
	}
	_, _, set := voiceAudioCommand(kind)
	arg := value
	if kind == "gps" {
		arg = strconv.Quote(value)
	}
	if _, err = a.exchange(ctx, set+arg, 3*time.Second); err != nil {
		return err
	}
	current, err = readVoiceAudio(ctx, a, kind)
	if err != nil || current != value {
		return errors.New("VOICE_AUDIO_VERIFY_FAILED")
	}
	return nil
}

func (v voiceAudioState) valid() bool {
	return voiceIdentityValid(v.Identity) && v.Key != "" && voiceAudioValue("pcm", v.PCM) &&
		strings.HasPrefix(v.PCM, "0") && voiceAudioValue("gps", v.GPS) && voiceAudioValue("mic", v.Mic)
}

func (s *System) voiceStatePath(identity string) string {
	return filepath.Join(s.VoiceStateDir, Digest(identity)+".json")
}

func syncVoiceStateDir(path string) error {
	if runtime.GOOS == "windows" { // Production state lives on the Linux host.
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Save before changing persistent QMIC. Never overwrite unfinished recovery.
func (s *System) saveVoiceState(state voiceAudioState) (string, error) {
	if s.VoiceStateDir == "" || !state.valid() {
		return "", errors.New("VOICE_AUDIO_STATE_UNAVAILABLE")
	}
	if err := os.MkdirAll(s.VoiceStateDir, 0700); err != nil {
		return "", err
	}
	if err := syncVoiceStateDir(filepath.Dir(s.VoiceStateDir)); err != nil {
		return "", err
	}
	path := s.voiceStatePath(state.Identity)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	err = json.NewEncoder(f).Encode(state)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err == nil {
		err = closed
	}
	if err == nil {
		err = syncVoiceStateDir(s.VoiceStateDir)
	}
	if err != nil {
		// No hardware change has happened yet; only this new file belongs to us.
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func loadVoiceState(path string) (voiceAudioState, error) {
	var v voiceAudioState
	f, err := os.Open(path)
	if err != nil {
		return v, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &v) != nil || !v.valid() {
		return v, errors.New("VOICE_AUDIO_STATE_INVALID")
	}
	return v, nil
}

func (s *System) prepareVoiceAudio(ctx context.Context, v *CellularCall) error {
	path, err := s.saveVoiceState(v.audio)
	if err != nil {
		return err
	}
	v.journal = path
	// USB supplies the voice; isolate analog input, preserving digital gain.
	mic := "0," + strings.Split(v.audio.Mic, ",")[1]
	for _, p := range []struct{ kind, value string }{{"mic", mic}, {"gps", "none"}, {"pcm", "1,0"}} {
		if err = setVoiceAudio(ctx, v.at, p.kind, p.value); err != nil {
			return err
		}
	}
	return nil
}

func (v *CellularCall) restoreAudio(ctx context.Context) error {
	for _, p := range []struct{ kind, value string }{{"pcm", v.audio.PCM}, {"gps", v.audio.GPS}, {"mic", v.audio.Mic}} {
		if err := setVoiceAudio(ctx, v.at, p.kind, p.value); err != nil {
			return err
		}
	}
	if v.journal != "" {
		if err := os.Remove(v.journal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncVoiceStateDir(filepath.Dir(v.journal)); err != nil {
			return err
		}
		v.journal = ""
	}
	return nil
}

// Discard, never forward or record, remaining USB audio during AT cleanup.
func discardCellularPCM(ctx context.Context, pcm atPort) func() {
	if pcm == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf [4096]byte
		for ctx.Err() == nil {
			if _, err := pcm.Read(buf[:]); err != nil {
				return
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *System) recoverVoiceAudio(ctx context.Context, c Candidate, identity string) error {
	if s.VoiceStateDir == "" || !voiceIdentityValid(identity) {
		return nil
	}
	path := s.voiceStatePath(identity)
	state, err := loadVoiceState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || state.Identity != identity {
		return errors.New("VOICE_AUDIO_RESTORE_PENDING")
	}
	pcm, err := openAT(c.Audio)
	if err != nil {
		return err
	}
	defer pcm.Close()
	stop := discardCellularPCM(ctx, pcm)
	defer stop()
	a, err := openWiFiSession(ctx, c, identity)
	if err != nil {
		return err
	}
	defer a.port.Close()
	v := &CellularCall{at: a, audio: state, journal: path}
	return v.Hangup(ctx)
}

// The module gate excludes live calls. IMEI is rechecked before any restore.
func (s *System) recoverVoiceCandidate(ctx context.Context, c Candidate) error {
	if s.VoiceStateDir == "" || !CellularVoiceSupported(c) {
		return nil
	}
	entries, err := os.ReadDir(s.VoiceStateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		state, err := loadVoiceState(filepath.Join(s.VoiceStateDir, entry.Name()))
		if err != nil || state.Key != c.Key || entry.Name() != filepath.Base(s.voiceStatePath(state.Identity)) {
			continue
		}
		if err = s.recoverVoiceAudio(ctx, c, state.Identity); err != nil && err.Error() != "DEVICE_CHANGED" {
			return err
		}
	}
	return nil
}
