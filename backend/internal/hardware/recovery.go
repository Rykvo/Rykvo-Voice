package hardware

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
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

// Explicit restarts use the responsive AT channel; stalled-device recovery keeps QMI.
func (s *System) RestartModule(ctx context.Context, c Candidate, imei string) error {
	return restartModuleAT(ctx, c, imei, s.Discover, openATSession)
}

func restartModuleAT(ctx context.Context, c Candidate, imei string,
	discover func(context.Context) ([]Candidate, error),
	open func(context.Context, Candidate, string) (*atSession, error)) error {
	if !RecoverySupported(c) || !decimal(imei, 14, 17) {
		return errors.New("RECOVERY_UNSUPPORTED")
	}
	current := func() (Candidate, error) {
		items, err := discover(ctx)
		if err != nil {
			return Candidate{}, err
		}
		for _, item := range items {
			if item.Key == c.Key && item.Generation == c.Generation && RecoverySupported(item) && len(item.Ports) > 0 {
				return item, nil
			}
		}
		return Candidate{}, errors.New("DEVICE_CHANGED")
	}
	next, err := current()
	if err != nil {
		return err
	}
	session, err := open(ctx, next, imei)
	if err != nil {
		return err
	}
	defer session.port.Close()
	if _, err = current(); err != nil {
		return err
	}
	// Send once. A lost reply may mean reset started; never fall back to a second reset.
	_, err = session.exchange(ctx, "AT+CFUN=1,1", 5*time.Second)
	return err
}

// The privileged helper accepts a fixed delayed reboot, never a command string.
func (s *System) RestartHost(ctx context.Context) error {
	_, err := proxyRequest(ctx, qmiSocket, map[string]string{"command": "host-restart"})
	return err
}
