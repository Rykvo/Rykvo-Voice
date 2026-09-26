package hardware

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
func TestCellularHangupPreservesDataAndRestoresPCM(t *testing.T) {
	p := &mmsTestPort{onWrite: func(b []byte) string {
		if string(b) == "AT+CLCC\r" {
			return "+CLCC: 1,1,0,1,0,\"\",128\r\nOK\r\n"
		}
		return "OK\r\n"
	}}
	c := &CellularCall{at: &atSession{port: p}, pcmBefore: "0,0", gpsBefore: "usbnmea"}
	if err := c.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "AT+CHUP\r|AT+CLCC\r|AT+QPCMV=0,0\r|AT+QGPSCFG=\"outport\",\"usbnmea\"\r"
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
	c := &CellularCall{at: &atSession{port: p}, pcmBefore: "0,0", gpsBefore: "none"}
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
