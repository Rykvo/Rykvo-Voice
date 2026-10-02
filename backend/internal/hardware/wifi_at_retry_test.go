package hardware

import (
	"context"
	"errors"
	"testing"
)

type lateQueryPort struct {
	atPort
	reads int
}

func (p *lateQueryPort) Read(b []byte) (int, error) {
	p.reads++
	if p.reads == 1 {
		return 0, errTimeout
	}
	return p.atPort.Read(b)
}

func TestVocatReadRetryDiscardsLateCardReply(t *testing.T) {
	writes := 0
	p := &mmsTestPort{onWrite: func([]byte) string {
		writes++
		if writes == 1 {
			return "+QCCID: 8986001234567890123\r\nOK\r\n"
		}
		return "+QCCID: 8986001234567890124\r\nOK\r\n"
	}}
	a := &vocatAT{session: &atSession{port: &lateQueryPort{atPort: p}}, device: "fixture", iccid: "8986001234567890123"}
	if err := a.verify(context.Background()); err == nil || err.Error() != "DEVICE_CHANGED" || writes != 2 {
		t.Fatal("late identity used instead of fresh query", writes, err)
	}
}

func TestVocatDoesNotReplayMutationsOrSensitiveAuthentication(t *testing.T) {
	for _, command := range []string{"AT+CFUN=4", "AT+CGACT=0,1", "AT+CGLA=1,4,\"0000\"", "AT+CSIM=4,\"0000\""} {
		p := &mmsTestPort{onWrite: func([]byte) string { return "OK\r\n" }}
		a := &vocatAT{session: &atSession{port: &lateQueryPort{atPort: p}}, device: "fixture"}
		_, err := a.ExecuteAT(context.Background(), "fixture", command)
		if err == nil || err.Error() != "READ_TIMEOUT" || len(p.commands) != 1 {
			t.Fatal(command, err, p.commands)
		}
	}
}

func TestVocatVerifyPreservesReadFailure(t *testing.T) {
	p := &mmsTestPort{onWrite: func([]byte) string { return "" }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &vocatAT{session: &atSession{port: p}, device: "fixture"}
	if err := a.verify(ctx); err == nil || err.Error() != "READ_TIMEOUT" || errors.Is(err, context.Canceled) || len(p.commands) != 0 {
		t.Fatal("read failure mislabeled as a SIM change", err, p.commands)
	}
}
