package hardware

import (
	"context"
	"errors"
	"slices"
	"strings"
)

func RecoverySupported(c Candidate) bool {
	return c.Kind == "usb" && c.Vendor == "2c7c" && c.Product == "0125" && strings.HasPrefix(strings.ToUpper(c.Model), "EC20") && c.Control != "" && c.Generation != ""
}

func CommunicationStalled(r Reading) bool {
	if r.Issue == "READ_TIMEOUT" {
		return true
	}
	if !slices.Contains(r.Warnings, "AT:READ_TIMEOUT") {
		return false
	}
	return (!r.Responsive && slices.Contains(r.Warnings, "--dms-get-ids:READ_TIMEOUT")) ||
		(r.Responsive && r.Issue == "" && r.ESIM != nil && r.ESIM.Issue == "READ_TIMEOUT")
}

func CommunicationHealthy(r Reading) bool {
	return r.Responsive && len(r.IMEI) >= 14 && r.Issue == "" && !slices.Contains(r.Warnings, "AT:READ_TIMEOUT") &&
		(r.ESIM == nil || r.ESIM.Issue == "" || r.ESIM.Issue == "NO_EUICC")
}

func (s *System) Restart(ctx context.Context, c Candidate) error {
	if !RecoverySupported(c) {
		return errors.New("RECOVERY_UNSUPPORTED")
	}
	_, err := proxyRequest(ctx, qmiSocket, map[string]string{
		"device": c.Control, "command": "--dms-set-operating-mode=reset",
		"endpoint": c.Key, "generation": c.Generation,
	})
	return err
}

// The privileged helper accepts a fixed delayed reboot, never a command string.
func (s *System) RestartHost(ctx context.Context) error {
	_, err := proxyRequest(ctx, qmiSocket, map[string]string{"command": "host-restart"})
	return err
}
