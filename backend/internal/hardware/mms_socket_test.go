package hardware

import (
	"context"
	"strings"
	"testing"
)

type mmsConnectTimeoutPort struct {
	mmsTestPort
	cancel context.CancelFunc
}

func (p *mmsConnectTimeoutPort) Read(data []byte) (int, error) {
	if p.Len() == 0 {
		p.cancel()
		return 0, nil
	}
	return p.Buffer.Read(data)
}

func TestMMSSocketTimeoutClosesOnlyAcknowledgedConnections(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost-command-ack", true: "lost-connect-urc"}[acknowledged], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &mmsConnectTimeoutPort{cancel: cancel}
			p.onWrite = func(data []byte) string {
				if strings.HasPrefix(string(data), "AT+QIOPEN=") && !acknowledged {
					return ""
				}
				if strings.HasPrefix(string(data), "AT+QICLOSE=") {
					return "+QIOPEN: 11,0\r\nOK\r\n"
				}
				return "OK\r\n"
			}
			b := &mmsBearer{at: &atSession{port: p}, cid: 16, healthy: true, active: true, created: true}
			result := MMSSubmitResult{Stage: "connect"}
			_, err := b.httpExchange(ctx, testMMSProfile(), "POST", testMMSProfile().MMSC, []byte("test"), &result)
			b.close()
			if err == nil || result.Attempted || result.Accepted {
				t.Fatal("timeout reported submission", result, err)
			}
			commands := strings.Join(p.commands, "\n")
			for _, cmd := range []string{"AT+QICLOSE=11,10", "AT+QIDEACT=16", "AT+CGDCONT=16"} {
				if strings.Contains(commands, cmd) != acknowledged {
					t.Fatalf("cleanup %q acknowledged=%t commands=%q", cmd, acknowledged, commands)
				}
			}
			if strings.Contains(commands, "AT+QISEND=") {
				t.Fatal("sent after failed connect")
			}
		})
	}
}

func TestMMSSocketResultAssociation(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		code           int
		failed         bool
	}{
		{"normal", "OK\r\n+QIOPEN: 11,0\r\n", 0, false},
		{"early", "+QIOPEN: 11,0\r\nOK\r\n", 0, false},
		{"other-socket", "OK\r\n+QIOPEN: 10,566\r\n+QIOPEN: 11,0\r\n", 0, false},
		{"rejected", "OK\r\n+QIOPEN: 11,566\r\n", 566, true},
		{"malformed", "OK\r\n+QIOPEN: 11,invalid\r\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &mmsTestPort{onWrite: func([]byte) string { return tc.response }}
			b := &mmsBearer{at: &atSession{port: p}, cid: 16, healthy: true}
			var result MMSSubmitResult
			err := b.openSocket(context.Background(), 11, "10.0.0.172", 80, &result)
			if (err != nil) != tc.failed || result.ConnectCode != tc.code || !b.healthy || b.at.pending != "" {
				t.Fatal(result, err, b.healthy, b.at.pending)
			}
		})
	}
}
