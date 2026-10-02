package hardware

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type voiceClosingPort struct {
	atPort
	closed int
}

func (p *voiceClosingPort) Close() error { p.closed++; return p.atPort.Close() }

func TestCellularHangupReopensFailedSessionAndKeepsJournal(t *testing.T) {
	s := &System{VoiceStateDir: t.TempDir()}
	state := testVoiceAudio()
	journal, err := s.saveVoiceState(state)
	if err != nil {
		t.Fatal(err)
	}
	old := &voiceClosingPort{atPort: &mmsTestPort{onWrite: func([]byte) string { return "" }}}
	fresh, values := voiceTestModem()
	v := &CellularCall{at: &atSession{port: old}, audio: state, journal: journal}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v.Hangup(ctx) == nil || !v.reconnect {
		t.Fatal("failed cleanup did not request a fresh session")
	}
	opened := 0
	v.reopen = func(context.Context) (*atSession, atPort, error) {
		opened++
		if opened == 1 {
			return nil, nil, errors.New("DEVICE_CHANGED")
		}
		return &atSession{port: fresh}, nil, nil
	}
	if err := v.Hangup(context.Background()); err == nil || err.Error() != "DEVICE_CHANGED" {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); err != nil || old.closed != 1 || len(fresh.commands) != 0 {
		t.Fatal("identity mismatch released recovery or changed hardware", err, old.closed)
	}
	if err := v.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) || values["pcm"] != "0,0" || values["gps"] != "usbnmea" || old.closed != 1 || v.reconnect {
		t.Fatal("cleanup did not restore the verified module", err, values, old.closed)
	}
	v.Close()
	if !errors.Is(v.Hangup(context.Background()), io.EOF) || opened != 2 {
		t.Fatal("closed call reconnected")
	}
}

func TestCellularHangupAfterReenumerationDoesNotEndNewCall(t *testing.T) {
	for _, incoming := range []string{"", "3"} {
		p, values := voiceTestModem()
		write := p.onWrite
		busy := true
		p.onWrite = func(b []byte) string {
			if string(b) == "AT+CLCC\r" && busy {
				return "+CLCC: 3,1,4,0,0,\"12345\",129\r\nOK\r\n"
			}
			return write(b)
		}
		v := &CellularCall{at: &atSession{port: p}, detached: true, incoming: incoming, audio: testVoiceAudio()}
		if v.Hangup(context.Background()) == nil || len(p.commands) != 1 || p.commands[0] != "AT+CLCC\r" || values["pcm"] != "1,0" {
			t.Fatal("changed a call with a reused ID", p.commands, values)
		}
		busy = false
		if err := v.Hangup(context.Background()); err != nil || values["pcm"] != "0,0" {
			t.Fatal(err, values)
		}
		for _, command := range p.commands {
			if strings.Contains(command, "CHUP") || strings.Contains(command, "CHLD") {
				t.Fatal("reused a stale call ID", command)
			}
		}
	}
}

func TestCellularFailedSetupReasons(t *testing.T) {
	for _, tc := range []struct{ dial, report, want string }{
		{"NO CARRIER", "Operator determined barring", "carrier_rejected"},
		{"NO CARRIER", "User busy", "busy"},
		{"NO CARRIER", "Call rejected", "rejected"},
		{"NO CARRIER", "User alerting, no answer", "no_answer"},
		{"NO CARRIER", "Normal call clearing", "remote_cancelled"},
		{"NO CARRIER", "Unassigned/unallocated number", "number_not_found"},
		{"NO CARRIER", "Unknown vendor reason", "dial_failed"},
		{"ERROR", "", "dial_failed"},
		{"BUSY", "Operator determined barring", "busy"},
		{"NO ANSWER", "Operator determined barring", "no_answer"},
		{"+CME ERROR: 13", "", "card_error"},
		{"+CME ERROR: 14", "", "module_busy"},
		{"+CME ERROR: 30", "", "not_registered"},
		{"+CME ERROR: 32", "", "carrier_rejected"},
	} {
		t.Run(tc.dial+"/"+tc.report, func(t *testing.T) {
			p := &mmsTestPort{onWrite: func(b []byte) string {
				if strings.HasPrefix(string(b), "ATD") {
					return tc.dial + "\r\n"
				}
				if string(b) == "AT+CEER\r" && tc.report != "" {
					return "+CEER: " + tc.report + "\r\nOK\r\n"
				}
				return "ERROR\r\n"
			}}
			v := &CellularCall{at: &atSession{port: p}}
			v.at.onLine = v.voiceLine
			if err := v.Dial(context.Background(), "12345"); err == nil || v.FailureReason() != tc.want {
				t.Fatal(err, v.FailureReason(), tc.want)
			}
			if tc.dial == "BUSY" || tc.dial == "NO ANSWER" || strings.HasPrefix(tc.dial, "+CME") {
				if len(p.commands) != 1 {
					t.Fatal("explicit result was replaced by a stale report", p.commands)
				}
			}
		})
	}
}

func TestCellularSetupEndReadsCauseOnce(t *testing.T) {
	for _, connected := range []bool{false, true} {
		dialed := false
		wasConnected := connected
		p := &mmsTestPort{onWrite: func(b []byte) string {
			if strings.HasPrefix(string(b), "ATD") {
				dialed = true
			}
			switch string(b) {
			case "AT+CEER\r":
				return "+CEER: Network out of order\r\nOK\r\n"
			case "AT+CLCC\r":
				if dialed && connected {
					connected = false
					return "+CLCC: 3,0,0,0,0\r\nOK\r\n"
				}
			}
			return "OK\r\n"
		}}
		v := &CellularCall{at: &atSession{port: p}}
		v.at.onLine = v.voiceLine
		if _, e := v.State(context.Background()); e != nil || strings.Contains(strings.Join(p.commands, "|"), "CEER") {
			t.Fatal("read stale cause before a dial", e, p.commands)
		}
		if e := v.Dial(context.Background(), "12345"); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 3; i++ {
			if _, e := v.State(context.Background()); e != nil {
				t.Fatal(e)
			}
		}
		wantReads, wantReason := 1, "carrier_unavailable"
		if wasConnected {
			wantReads, wantReason = 0, ""
		}
		if strings.Count(strings.Join(p.commands, "|"), "AT+CEER") != wantReads || v.FailureReason() != wantReason {
			t.Fatal(p.commands, v.FailureReason())
		}
	}
}

func TestCellularCancelledDialDoesNotReportFailure(t *testing.T) {
	p := &mmsTestPort{onWrite: func([]byte) string { return "OK\r\n" }}
	v := &CellularCall{at: &atSession{port: p}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := v.Dial(ctx, "12345"); !errors.Is(err, context.Canceled) || v.FailureReason() != "" || len(p.commands) != 0 {
		t.Fatal(err, p.commands, v.FailureReason())
	}
	for _, line := range []string{"NO CARRIER", "+CEER: 21", "+CEER: 0,21", "+CEER: " + strings.Repeat("a", 300), "+CEER: token=secret"} {
		if voiceReleaseResult(line) != "" {
			t.Fatal("guessed numeric or unknown cause", line)
		}
	}
}

func TestCellularStateSeparatesPacketData(t *testing.T) {
	data := `+CLCC: 1,1,0,1,0,"",128`
	for _, tc := range []struct{ line, want string }{
		{data, "idle"}, {`+CLCC: 3,0,2,0,0,"12345",129`, "dialing"},
		{`+CLCC: 3,0,3,0,0,"12345",129`, "ringing"}, {`+CLCC: 3,0,0,0,0,"12345",129`, "active"},
		{`+CLCC: 3,1,4,0,0,"12345",129`, "incoming"},
	} {
		got, err := cellularCallState([]string{data, tc.line})
		if err != nil || got != tc.want {
			t.Fatal(got, err, tc)
		}
	}
	for _, line := range []string{`+CLCC: 3,0,1,0,0`, `+CLCC: malformed`, `+CLCC: 3,0,0,9,0`, `+CLCC: 3,0,0,1,0,"12345",129`} {
		if _, err := cellularCallState([]string{line}); err == nil {
			t.Fatal(line)
		}
	}
}

func TestCellularPCMFlowDropsAreObservable(t *testing.T) {
	v := &CellularCall{}
	if err := v.WritePCM(make([]int16, 800)); err != nil || v.DroppedPCMSamples() != 800 {
		t.Fatal(err, v.DroppedPCMSamples())
	}
	v.closed.Store(true)
	if v.WritePCM(make([]int16, 800)) == nil || v.DroppedPCMSamples() != 800 {
		t.Fatal("closed stream counted as flow control")
	}
}
func TestCellularHangupPreservesDataAndRestoresPCM(t *testing.T) {
	p, _ := voiceTestModem()
	write := p.onWrite
	p.onWrite = func(b []byte) string {
		if string(b) == "AT+CLCC\r" {
			return "+CLCC: 1,1,0,1,0,\"\",128\r\nOK\r\n"
		}
		return write(b)
	}
	c := &CellularCall{at: &atSession{port: p}, audio: testVoiceAudio()}
	if err := c.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "AT+CHUP\r|AT+CLCC\r|AT+QPCMV?\r|AT+QPCMV=0,0\r|AT+QPCMV?\r|AT+QGPSCFG=\"outport\"\r|AT+QGPSCFG=\"outport\",\"usbnmea\"\r|AT+QGPSCFG=\"outport\"\r|AT+QMIC?\r"
	if got := strings.Join(p.commands, "|"); got != want {
		t.Fatal(got)
	}
}
func TestCellularHangupDoesNotReleaseLiveVoice(t *testing.T) {
	p := &mmsTestPort{onWrite: func(b []byte) string {
		if string(b) == "AT+CLCC\r" {
			return "+CLCC: 3,0,0,0,0\r\nOK\r\n"
		}
		return "OK\r\n"
	}}
	c := &CellularCall{at: &atSession{port: p}, audio: testVoiceAudio()}
	if c.Hangup(context.Background()) == nil || len(p.commands) != 2 {
		t.Fatal(p.commands)
	}
}
func TestATLateReplyCannotConfirmHangup(t *testing.T) {
	p := &mmsTestPort{onWrite: func(b []byte) string { return "ERROR\r\n" }}
	// The late OK belongs to the previous interrupted dial, not CHUP.
	a := &atSession{port: p, pending: "ATD12345;", buffer: "OK\r\n"}
	if _, err := a.exchange(context.Background(), "AT+CHUP", time.Second); err == nil {
		t.Fatal("late reply confirmed hangup")
	}
	if a.pending != "" || len(p.commands) != 1 {
		t.Fatal(a.pending, p.commands)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.exchange(ctx, "AT+CHUP", time.Second); err == nil || a.pending != "" || len(p.commands) != 1 {
		t.Fatal("cancelled command queued")
	}
}

func TestCellularPeerAlreadyEndedStillConfirmsIdle(t *testing.T) {
	p, _ := voiceTestModem()
	write := p.onWrite
	p.onWrite = func(b []byte) string {
		if string(b) == "AT+CHUP\r" {
			return "ERROR\r\n"
		}
		return write(b)
	}
	c := &CellularCall{at: &atSession{port: p}, audio: testVoiceAudio()}
	if err := c.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCellularIncomingAnswerRechecksCallAndWaitingIsSeparate(t *testing.T) {
	for _, line := range []string{`+CLCC: 3,1,4,0,0,"12345",129`, `+CLCC: 4,1,4,0,0,"12345",129`, `+CLCC: 3,1,0,0,0,"12345",129`, ""} {
		p := &mmsTestPort{onWrite: func(b []byte) string {
			if string(b) == "AT+CLCC\r" {
				return line + "\r\nOK\r\n"
			}
			return "OK\r\n"
		}}
		v := &CellularCall{at: &atSession{port: p}, incoming: "3"}
		err := v.Answer(context.Background())
		valid := strings.Contains(line, ": 3,1,4,")
		if (err == nil) != valid {
			t.Fatal(line, err)
		}
		if strings.Contains(strings.Join(p.commands, "|"), "ATA\r") != valid {
			t.Fatal(p.commands)
		}
	}
	waiting := `+CLCC: 4,1,5,0,0,"99999",129`
	active := `+CLCC: 3,0,0,0,0,"12345",129`
	for _, lines := range [][]string{{waiting, active}, {active, waiting}} {
		p := &mmsTestPort{onWrite: func(b []byte) string {
			if string(b) == "AT+CLCC\r" {
				return strings.Join(lines, "\r\n") + "\r\nOK\r\n"
			}
			return "OK\r\n"
		}}
		v := &CellularCall{at: &atSession{port: p}}
		if state, e := v.State(context.Background()); e != nil || state != "active" {
			t.Fatal(state, e)
		}
		if got := strings.Join(p.commands, "|"); got != "AT+CLCC\r|AT+CHLD=14\r" {
			t.Fatal("waiting disturbed original", got)
		}
	}
}

func TestCellularIncomingHangupDoesNotEndOtherCall(t *testing.T) {
	p := &mmsTestPort{onWrite: func(b []byte) string {
		if string(b) == "AT+CLCC\r" {
			return "+CLCC: 2,0,0,0,0,\"12345\",129\r\nOK\r\n"
		}
		return "OK\r\n"
	}}
	v := &CellularCall{at: &atSession{port: p}, incoming: "3", audio: testVoiceAudio()}
	if v.Hangup(context.Background()) == nil {
		t.Fatal("other call reported idle")
	}
	for _, cmd := range p.commands {
		if cmd != "AT+CLCC\r" {
			t.Fatal("changed another call", cmd)
		}
	}
	for _, line := range []string{`+CLCC: x,1,4,0,0`, `+CLCC: 3,9,4,0,0`, `+CLCC: 3,1,9,0,0`, `+CLCC: 3,1,4,9,0`} {
		if _, e := cellularCalls([]string{line}); e == nil {
			t.Fatal(line)
		}
	}
	calls, e := cellularCalls([]string{`+CLCC: 3,1,4,0,0,"12025550123",145`})
	if e != nil || calls[0].number != "+12025550123" {
		t.Fatal(calls, e)
	}
}
