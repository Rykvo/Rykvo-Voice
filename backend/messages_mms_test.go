package main

import (
	"errors"
	"rykvo.local/auth/internal/hardware"
	"testing"
)

func TestNativeMMSStateDoesNotReplayUncertainSubmissions(t *testing.T) {
	for _, v := range []struct {
		result       hardware.MMSSubmitResult
		issue, state string
	}{
		{hardware.MMSSubmitResult{Attempted: true, Accepted: true}, "", "accepted"},
		{hardware.MMSSubmitResult{Attempted: true}, "MMS_MODEM_775_HTTP_0", "unknown"},
		{hardware.MMSSubmitResult{Attempted: true}, "MMS_REJECTED", "failed"},
		{hardware.MMSSubmitResult{Attempted: true}, "MMS_HTTP_INVALID", "unknown"},
		{hardware.MMSSubmitResult{}, "MMS_SOCKET_FAILED", "failed"},
		{hardware.MMSSubmitResult{}, "MMS_PDP_ACTIVATION_FAILED", "failed"},
		{hardware.MMSSubmitResult{}, "MMS_NOT_READY", "waiting_network"},
	} {
		var e error
		if v.issue != "" {
			e = errors.New(v.issue)
		}
		state, issue := nativeMMSState(v.result, e)
		if state != v.state || issue != v.issue {
			t.Fatal(state, issue, v)
		}
	}
}
