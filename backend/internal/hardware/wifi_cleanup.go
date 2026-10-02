package hardware

import "strings"

// Keep admission and execution consistent. Unconfirmed local cleanup is never
// made safe merely by waiting for the quarantine timer to expire.
func WiFiCleanupUnconfirmed(code string) bool {
	switch code {
	case "WIFI_RADIO_RESTORE_UNCONFIRMED", "WIFI_SIM_CLEANUP_UNCONFIRMED",
		"WIFI_IMS_CLEANUP_UNCONFIRMED", "WIFI_TUNNEL_CLEANUP_UNCONFIRMED", "WIFI_CLEANUP_UNCONFIRMED":
		return true
	}
	return false
}

func vocatCleanupIssue(failures []string) string {
	code := ""
	for _, failure := range failures {
		next := "WIFI_CLEANUP_UNCONFIRMED"
		switch {
		case strings.HasPrefix(failure, "restore radio:"):
			next = "WIFI_RADIO_RESTORE_UNCONFIRMED"
		case strings.HasPrefix(failure, "close IMS:"):
			next = "WIFI_IMS_CLEANUP_UNCONFIRMED"
		case strings.HasPrefix(failure, "close tunnel:"):
			next = "WIFI_TUNNEL_CLEANUP_UNCONFIRMED"
		}
		if code != "" && code != next {
			return "WIFI_CLEANUP_UNCONFIRMED"
		}
		code = next
	}
	if code == "" {
		return "WIFI_CLEANUP_UNCONFIRMED"
	}
	return code
}
