package hardware

import (
	"context"
	"errors"
	"testing"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestCleanupFailuresKeepComponentAndInterlock(t *testing.T) {
	for _, tc := range []struct {
		failures []string
		want     string
	}{
		{[]string{"close IMS: private details"}, "WIFI_IMS_CLEANUP_UNCONFIRMED"},
		{[]string{"close tunnel: private details"}, "WIFI_TUNNEL_CLEANUP_UNCONFIRMED"},
		{[]string{"restore radio: private details"}, "WIFI_RADIO_RESTORE_UNCONFIRMED"},
		{[]string{"close IMS: x", "restore radio: y"}, "WIFI_CLEANUP_UNCONFIRMED"},
		{[]string{"unknown private details"}, "WIFI_CLEANUP_UNCONFIRMED"},
	} {
		err := vocatStateError(vowifi.State{CleanupErrors: tc.failures}, context.Canceled)
		if err == nil || err.Error() != tc.want || !WiFiCleanupUnconfirmed(tc.want) {
			t.Fatal("cleanup cause hidden or cancellation accepted as success", err)
		}
		if wifiWorkerCode(err) != tc.want || WiFiIssue(err) != tc.want {
			t.Fatal("cleanup code lost in IPC")
		}
	}
	if WiFiCleanupUnconfirmed("WIFI_CONNECTION_FAILED") || wifiWorkerCode(errors.New("private provider text")) != "WIFI_CONNECTION_FAILED" {
		t.Fatal("cleanup classification too broad")
	}
	if !wifiWorkerStage("diagnostic:tunnel-delete-unconfirmed") || wifiWorkerStage("diagnostic:tunnel-delete-unconfirmed private") {
		t.Fatal("remote cleanup diagnostic allowlist")
	}
}
