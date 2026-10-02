package ims

import (
	"context"
	"strings"
	"time"
)

func (session *Session) validIncomingACK(call *imsCall, req *sipRequest) bool {
	a, b := strings.Fields(call.invite.value("CSeq")), strings.Fields(req.value("CSeq"))
	return len(a) == 2 && len(b) == 2 && a[0] == b[0] && b[1] == "ACK" &&
		headerParameter(req.value("From"), "tag") == headerParameter(call.invite.value("From"), "tag") &&
		headerParameter(req.value("To"), "tag") == session.fromTag
}

// Retransmit the same final answer until the carrier acknowledges this dialog.
func (session *Session) watchIncomingAnswer(call *imsCall) {
	parent := session.refreshContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	session.callMu.Lock()
	if call.incomingACK || call.terminated {
		session.callMu.Unlock()
		cancel()
		return
	}
	call.incomingCancel = cancel
	session.callMu.Unlock()
	go func() {
		defer cancel()
		deadline := time.NewTimer(32 * time.Second)
		defer deadline.Stop()
		delay := 500 * time.Millisecond
		repeat := time.NewTimer(delay)
		defer repeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				end, done := context.WithTimeout(context.Background(), 5*time.Second)
				_ = session.HangupCall(end, call.callID)
				done()
				return
			case <-repeat.C:
				call.operation.Lock()
				session.callMu.Lock()
				pending := !call.incomingACK && !call.terminated
				response := append([]byte(nil), call.lastResponse...)
				session.callMu.Unlock()
				if pending {
					_ = call.respond(response)
				}
				call.operation.Unlock()
				if !pending {
					return
				}
				delay = min(4*time.Second, delay*2)
				repeat.Reset(delay)
			}
		}
	}()
}
