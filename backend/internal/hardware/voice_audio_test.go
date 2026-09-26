package hardware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testVoiceAudio() voiceAudioState {
	return voiceAudioState{Identity: "imei:" + Digest("voice-test"), Key: "usb:test", PCM: "0,0", GPS: "usbnmea"}
}

func voiceTestModem() (*mmsTestPort, map[string]string) {
	state := map[string]string{"pcm": "1,0", "gps": "none", "mic": "0,14567"}
	p := &mmsTestPort{onWrite: func(b []byte) string {
		cmd := strings.TrimSuffix(string(b), "\r")
		for _, kind := range []string{"pcm", "gps", "mic"} {
			query, prefix, set := voiceAudioCommand(kind)
			if cmd == query {
				value := state[kind]
				if kind == "gps" {
					value = `"outport",` + value
				}
				return prefix + " " + value + "\r\nOK\r\n"
			}
			if strings.HasPrefix(cmd, set) {
				value := strings.Trim(strings.TrimPrefix(cmd, set), `"`)
				if kind == "pcm" && value == state[kind] {
					return "ERROR\r\n"
				}
				state[kind] = value
				return "OK\r\n"
			}
		}
		return "OK\r\n"
	}}
	return p, state
}

func TestVoiceAudioParsing(t *testing.T) {
	for _, v := range []string{"0,0", "65535,65535", "20577,14567"} {
		if !voiceAudioValue("mic", v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"", "0", "0,1,2", "65536,0", "-1,0", "1,2\rAT", "1,+2", "00,2", "1,2.0"} {
		if voiceAudioValue("mic", v) {
			t.Fatal("accepted", v)
		}
	}
	for _, tc := range []struct{ kind, lines, want string }{
		{"mic", "+QMIC: 20577, 14567", "20577,14567"},
		{"mic", "+QMIC: 0,0\r\n+QMIC: 1,1", ""},
		{"mic", "+QMIC: 65536,0", ""},
		{"gps", `+QGPSCFG: "outport",usbnmea`, "usbnmea"},
		{"gps", `+QGPSCFG: "other",usbnmea`, ""},
		{"pcm", "+QPCMV: 1\r\n+QPCMV: 0,0\r\n+QPCMV: 0", "0,0"},
		{"pcm", "+QPCMV: 2,0", ""},
	} {
		p := &mmsTestPort{onWrite: func([]byte) string { return tc.lines + "\r\nOK\r\n" }}
		got, err := readVoiceAudio(context.Background(), &atSession{port: p}, tc.kind)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatal(tc, got, err)
		}
	}
}

func TestVoiceMicKeepsDigitalGainAndVerifies(t *testing.T) {
	p, state := voiceTestModem()
	state["mic"] = "20577,14567"
	a := &atSession{port: p}
	if err := setVoiceAudio(context.Background(), a, "mic", "0,14567"); err != nil || state["mic"] != "0,14567" {
		t.Fatal(err, state)
	}
	write := p.onWrite
	p.onWrite = func(b []byte) string {
		if strings.HasPrefix(string(b), "AT+QMIC=") {
			return "OK\r\n" // An OK without matching readback is not success.
		}
		return write(b)
	}
	if err := setVoiceAudio(context.Background(), a, "mic", "20577,14567"); err == nil {
		t.Fatal("unverified gain accepted")
	}
}

func TestVoiceAudioPreparationCheckpointsBeforeMutation(t *testing.T) {
	for _, failure := range []string{"", "AT+QMIC=0,14567", `AT+QGPSCFG="outport","none"`, "AT+QPCMV=1,0"} {
		t.Run(failure, func(t *testing.T) {
			s := &System{VoiceStateDir: t.TempDir()}
			p, state := voiceTestModem()
			original := testVoiceAudio()
			state["pcm"], state["gps"], state["mic"] = original.PCM, original.GPS, "20577,14567"
			write := p.onWrite
			p.onWrite = func(b []byte) string {
				if strings.Contains(string(b), "=") {
					saved, err := loadVoiceState(s.voiceStatePath(original.Identity))
					if err != nil || saved != original {
						t.Fatal("changed before checkpoint", err)
					}
				}
				result := write(b)
				if strings.TrimSpace(string(b)) == failure {
					return "ERROR\r\n"
				}
				return result
			}
			v := &CellularCall{at: &atSession{port: p}, audio: original}
			if err := s.prepareVoiceAudio(context.Background(), v); (err != nil) != (failure != "") {
				t.Fatal(failure, err)
			}
			p.onWrite = write
			if err := v.Hangup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if state["mic"] != "0,14567" || state["pcm"] != original.PCM || state["gps"] != original.GPS {
				t.Fatal(state)
			}
		})
	}
}

func TestVoiceAudioNoMutationWithoutJournal(t *testing.T) {
	p, _ := voiceTestModem()
	v := &CellularCall{at: &atSession{port: p}, audio: testVoiceAudio()}
	if err := (&System{}).prepareVoiceAudio(context.Background(), v); err == nil || len(p.commands) != 0 {
		t.Fatal("unrecoverable audio change", err, p.commands)
	}
	s := &System{VoiceStateDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.prepareVoiceAudio(ctx, v); err == nil || len(p.commands) != 0 {
		t.Fatal("cancelled audio change", err)
	}
	if _, err := loadVoiceState(v.journal); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceAudioRecoveryRejectsDifferentIdentity(t *testing.T) {
	s := &System{VoiceStateDir: t.TempDir()}
	v := testVoiceAudio()
	path, err := s.saveVoiceState(v)
	if err != nil {
		t.Fatal(err)
	}
	other := "imei:" + Digest("other")
	if err = os.Rename(path, s.voiceStatePath(other)); err != nil {
		t.Fatal(err)
	}
	if err = s.recoverVoiceAudio(context.Background(), Candidate{}, other); err == nil || err.Error() != "VOICE_AUDIO_RESTORE_PENDING" {
		t.Fatal("foreign restoration accepted", err)
	}
}

func TestVoiceAudioJournalSurvivesRestartAndRepeatedHangup(t *testing.T) {
	s := &System{VoiceStateDir: filepath.Join(t.TempDir(), "audio")}
	original := testVoiceAudio()
	path, err := s.saveVoiceState(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.saveVoiceState(original); !errors.Is(err, os.ErrExist) {
		t.Fatal("overwrote unfinished restoration", err)
	}
	loaded, err := loadVoiceState(path)
	if err != nil || loaded != original {
		t.Fatal(loaded, err)
	}
	p, state := voiceTestModem()
	v := &CellularCall{at: &atSession{port: p}, audio: loaded, journal: path}
	for i := 0; i < 2; i++ {
		if err = v.Hangup(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) || v.journal != "" {
		t.Fatal("completed journal retained", err)
	}
	if state["mic"] != "0,14567" || state["pcm"] != original.PCM || state["gps"] != original.GPS {
		t.Fatal(state)
	}
	if strings.Count(strings.Join(p.commands, "|"), "AT+QPCMV=0,0") != 1 {
		t.Fatal("repeated non-idempotent PCM write", p.commands)
	}
}

func TestVoiceAudioRestoreFailureKeepsRecovery(t *testing.T) {
	for _, failure := range []string{"AT+QPCMV=0,0", `AT+QGPSCFG="outport","usbnmea"`, "AT+QMIC=0,14567"} {
		t.Run(failure, func(t *testing.T) {
			s := &System{VoiceStateDir: t.TempDir()}
			path, err := s.saveVoiceState(testVoiceAudio())
			if err != nil {
				t.Fatal(err)
			}
			p, state := voiceTestModem()
			state["mic"] = "20577,14567"
			write := p.onWrite
			p.onWrite = func(b []byte) string {
				if string(b) == failure+"\r" {
					return "ERROR\r\n"
				}
				return write(b)
			}
			v := &CellularCall{at: &atSession{port: p}, audio: testVoiceAudio(), journal: path}
			if v.Hangup(context.Background()) == nil {
				t.Fatal("failed restore released")
			}
			if _, err = loadVoiceState(path); err != nil {
				t.Fatal("recovery lost", err)
			}
			p.onWrite = write
			if err = v.Hangup(context.Background()); err != nil {
				t.Fatal("retry", err)
			}
		})
	}
}

func TestVoiceMicPermanentPolicy(t *testing.T) {
	for _, mic := range []string{"20577,14567", "0,73", "65535,65535", "1,0"} {
		t.Run(mic, func(t *testing.T) {
			p, state := voiceTestModem()
			state["mic"] = mic
			a := &atSession{port: p}
			want := "0," + strings.Split(mic, ",")[1]
			if err := muteIdleVoiceMic(context.Background(), a); err != nil || state["mic"] != want {
				t.Fatal(err, state)
			}
			p.commands = nil
			for i := 0; i < 3; i++ {
				if err := muteIdleVoiceMic(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			for _, command := range p.commands {
				if strings.Contains(command, "=") {
					t.Fatal("polling rewrote persistent settings", p.commands)
				}
			}
			// A reset must be detected rather than trusted from a cached flag.
			state["mic"] = "20577," + strings.Split(mic, ",")[1]
			if err := muteIdleVoiceMic(context.Background(), a); err != nil || state["mic"] != want {
				t.Fatal("reset left analog input on", err, state)
			}
		})
	}
}

func TestVoiceMicPolicyDoesNotTouchLiveOrUnknownCalls(t *testing.T) {
	for _, reply := range []string{`+CLCC: 1,0,0,0,0,"12345",129`, `+CLCC: 1,0,2,0,0,"12345",129`, `+CLCC: 1,1,4,0,0,"12345",129`, "+CLCC: malformed", "ERROR"} {
		p := &mmsTestPort{onWrite: func([]byte) string { return reply + "\r\nOK\r\n" }}
		_ = muteIdleVoiceMic(context.Background(), &atSession{port: p})
		if len(p.commands) != 1 || p.commands[0] != "AT+CLCC\r" {
			t.Fatal("reconfigured busy or unknown module", p.commands)
		}
	}
}

func TestVoiceMicPolicyRejectsUnknownGainAndFailedReadback(t *testing.T) {
	for _, mic := range []string{"", "20577", "1,2,3", "65536,1", "0,invalid", "1,14567"} {
		p, state := voiceTestModem()
		state["mic"] = mic
		write := p.onWrite
		p.onWrite = func(b []byte) string {
			if strings.HasPrefix(string(b), "AT+QMIC=") {
				return "OK\r\n" // Ignored hardware write.
			}
			return write(b)
		}
		if muteVoiceMic(context.Background(), &atSession{port: p}) == nil {
			t.Fatal("unverified mute accepted", mic)
		}
		if state["mic"] != mic {
			t.Fatal("unknown digital gain changed")
		}
	}
}

func TestVoiceAudioOldJournalNeverReopensAnalogMic(t *testing.T) {
	s := &System{VoiceStateDir: t.TempDir()}
	original := testVoiceAudio()
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	// Migration from the previous per-call microphone restore journal.
	data = append(data[:len(data)-1], []byte(`,"mic":"20577,14567"}`)...)
	path := s.voiceStatePath(original.Identity)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadVoiceState(path)
	if err != nil {
		t.Fatal(err)
	}
	p, state := voiceTestModem()
	state["mic"] = "20577,99"
	v := &CellularCall{at: &atSession{port: p}, audio: loaded, journal: path}
	if err = v.Hangup(context.Background()); err != nil || state["mic"] != "0,99" {
		t.Fatal("old journal reopened mic or reset digital gain", err, state)
	}
	for _, command := range p.commands {
		if strings.HasPrefix(command, "AT+QMIC=") && command != "AT+QMIC=0,99\r" {
			t.Fatal(command)
		}
	}
	data, err = json.Marshal(loaded)
	if err != nil || strings.Contains(string(data), `"mic"`) {
		t.Fatal("obsolete mic restore state retained", err)
	}
}

func TestVoiceAudioJournalRejectsInvalidState(t *testing.T) {
	s := &System{VoiceStateDir: t.TempDir()}
	v := testVoiceAudio()
	v.Identity = "imei:../../" + strings.Repeat("0", 56)
	if _, err := s.saveVoiceState(v); err == nil {
		t.Fatal("unsafe identity")
	}
	v = testVoiceAudio()
	v.PCM = "1,0"
	if _, err := s.saveVoiceState(v); err == nil {
		t.Fatal("saved active PCM")
	}
	path := s.voiceStatePath(testVoiceAudio().Identity)
	for _, data := range []string{"{", `{ "identity":"other" }`, strings.Repeat(" ", 4097)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadVoiceState(path); err == nil {
			t.Fatal("invalid restoration accepted")
		}
	}
	other := testVoiceAudio()
	other.Identity = "imei:" + Digest("different-module")
	if s.voiceStatePath(other.Identity) == path {
		t.Fatal("module states share path")
	}
}

type voiceDiscardPort struct{ reads atomic.Int32 }

func (p *voiceDiscardPort) Read(b []byte) (int, error) {
	time.Sleep(time.Millisecond)
	p.reads.Add(1)
	b[0] = 0
	return 1, nil
}
func (*voiceDiscardPort) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (*voiceDiscardPort) Close() error              { return nil }

func TestVoiceAudioCleanupDrainsAndStops(t *testing.T) {
	p := &voiceDiscardPort{}
	stop := discardCellularPCM(context.Background(), p)
	deadline := time.Now().Add(time.Second)
	for p.reads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	before := p.reads.Load()
	time.Sleep(5 * time.Millisecond)
	if before == 0 || p.reads.Load() != before {
		t.Fatal("discard leaked or never started")
	}
}

func TestCellularHangupDrainsBeforeATConfirmation(t *testing.T) {
	p, _ := voiceTestModem()
	pcm := &voiceDiscardPort{}
	write := p.onWrite
	p.onWrite = func(b []byte) string {
		if string(b) == "AT+CHUP\r" {
			deadline := time.Now().Add(time.Second)
			for pcm.reads.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if pcm.reads.Load() == 0 {
				t.Fatal("AT cleanup did not drain USB")
			}
		}
		return write(b)
	}
	v := &CellularCall{at: &atSession{port: p}, pcm: pcm, audio: testVoiceAudio()}
	if err := v.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := pcm.reads.Load()
	time.Sleep(5 * time.Millisecond)
	if pcm.reads.Load() != before {
		t.Fatal("cleanup reader continued after hangup")
	}
}
