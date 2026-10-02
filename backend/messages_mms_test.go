package main

import (
	"errors"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/mms"
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

func TestMMSDownloadRetriesAreBoundedAndDoNotCountBearerWait(t *testing.T) {
	for _, tc := range []struct {
		code          string
		before, after int
		state         string
	}{
		{"MMS_DOWNLOAD_TIMEOUT", 0, 1, "waiting_network"},
		{"MMS_DOWNLOAD_TIMEOUT", 1, 2, "waiting_network"},
		{"MMS_DOWNLOAD_TIMEOUT", 2, 3, "failed"},
		{"MMS_HTTP_503", 0, 1, "waiting_network"},
		{"MMS_HTTP_403", 0, 1, "failed"},
		{"MMS_INVALID_PDU", 0, 1, "failed"},
		{"MMS_BUSY", 2, 2, "waiting_network"},
		{"MMS_NETWORK_REQUIRED", 2, 2, "waiting_network"},
	} {
		err := errors.New(tc.code)
		if tc.code == mms.ErrNetwork.Error() {
			err = mms.ErrNetwork
		}
		state, n := mmsDownloadFailure(err, tc.before)
		if state != tc.state || n != tc.after {
			t.Fatal(tc, state, n)
		}
	}
}
