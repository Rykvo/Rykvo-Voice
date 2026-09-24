package hardware

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

func WiFiSupported(c Candidate) bool {
	if c.Kind != "usb" || c.Vendor != "2c7c" || c.Product != "0125" || c.Generation == "" {
		return false
	}
	for _, p := range c.Ports {
		if p.Interface == 3 {
			return true
		}
	}
	return false
}
func wifiRadioMode(ctx context.Context, s *atSession) (int, error) {
	lines, err := s.exchange(ctx, "AT+CFUN?", 5*time.Second)
	if err != nil {
		return -1, err
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "+CFUN:") {
			v := fields(line)
			if len(v) > 0 {
				if mode := integer(v[0]); mode != nil {
					return *mode, nil
				}
			}
		}
	}
	return -1, errors.New("WIFI_RADIO_STATE_UNKNOWN")
}
func openWiFiSession(ctx context.Context, c Candidate, identity string) (*atSession, error) {
	if !WiFiSupported(c) || !strings.HasPrefix(identity, "imei:") || len(identity) != 69 {
		return nil, errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	ports := make([]Port, 0, 1)
	for _, p := range c.Ports {
		if p.Interface == 3 {
			ports = append(ports, p)
		}
	}
	c.Ports = ports
	s, err := openATSession(ctx, c, "")
	if err != nil {
		return nil, err
	}
	lines, err := s.exchange(ctx, "AT+CGSN", 5*time.Second)
	if err != nil || c.Identity(Reading{IMEI: digits(lines, 14, 17)}) != identity {
		s.port.Close()
		return nil, errors.New("DEVICE_CHANGED")
	}
	return s, nil
}

// Caller holds the module gate until RF and SIM cleanup have finished.
func (s *System) WiFi(ctx context.Context, c Candidate, identity, iccid string, emit func(string)) error {
	if !decimal(iccid, 18, 20) {
		return errors.New("DEVICE_CHANGED")
	}
	found, err := s.Discover(ctx)
	if err != nil {
		return err
	}
	valid := false
	for _, v := range found {
		if v.Key == c.Key && v.Generation == c.Generation {
			c = v
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("DEVICE_CHANGED")
	}
	session, err := openWiFiSession(ctx, c, identity)
	if err != nil {
		return err
	}
	defer session.port.Close()
	return runWiFi(ctx, c, session, iccid, 0, true, emit)
}
func runWiFiSIM(ctx context.Context, c Candidate, session *atSession, iccid string, hold time.Duration) error {
	emit := func(stage string) {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"stage": stage, "registered": stage == "connected" || stage == "renewed"})
	}
	return runWiFi(ctx, c, session, iccid, hold, false, emit)
}
func runWiFi(ctx context.Context, c Candidate, session *atSession, iccid string, hold time.Duration, live bool, emit func(string)) (err error) {
	if !WiFiSupported(c) {
		return errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	before, err := wifiRadioMode(ctx, session)
	if err != nil || before != 1 && before != 4 {
		return errors.New("WIFI_RADIO_STATE_UNKNOWN")
	}
	probe, err := inspectWiFiSIM(ctx, session, iccid)
	if err != nil {
		return err
	}
	if err = probe.close(); err != nil {
		return err
	}
	if before == 1 {
		if err := wifiHostData(c.Network); err != nil {
			return err
		}
	}

	restore := before
	if live {
		restore = 1
	}
	if before == 1 || live {
		defer func() {
			clean, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			lines, e := session.exchange(clean, "AT+QCCID", 5*time.Second)
			if e != nil || digits(lines, 18, 20) != iccid {
				err = errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
				return
			}
			mode, e := wifiRadioMode(clean, session)
			if e != nil {
				err = errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
				return
			}
			if mode == 4 && restore == 1 {
				if _, e = session.exchange(clean, "AT+CFUN=1", 15*time.Second); e != nil {
					err = errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
					return
				}
			}
			mode, e = wifiRadioMode(clean, session)
			if e != nil || mode != restore {
				err = errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
				return
			}
			emit("radio-restored")
		}()
	}
	if before == 1 {
		emit("flight")
	}
	if err = wifiRFOff(ctx, session, before); err != nil {
		return err
	}

	sim, err := inspectWiFiSIM(ctx, session, iccid)
	if err != nil {
		return err
	}
	defer func() {
		if e := sim.close(); e != nil && err == nil {
			err = errors.New("WIFI_SIM_CLEANUP_UNCONFIRMED")
		}
	}()
	emit("sim-ready")
	for attempt := 0; ; attempt++ {
		connectedAt := time.Now()
		err = wifiConnect(ctx, sim, hold, live, emit)
		if ctx.Err() != nil {
			if err != nil && strings.Contains(err.Error(), "_UNCONFIRMED") {
				return err
			}
			return nil
		}
		if !live || err == nil || !wifiRetryable(err) || attempt >= 3 {
			return err
		}
		if time.Since(connectedAt) > 5*time.Minute {
			attempt = 0
		}
		emit("reconnecting")
		timer := time.NewTimer(time.Duration(30<<attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if e := sim.verifyCard(ctx); e != nil {
			return e
		}
	}
}
func wifiRetryable(err error) bool {
	s := err.Error()
	return strings.HasPrefix(s, "WIFI_NETWORK_") || s == "WIFI_IMS_TIMEOUT" || s == "WIFI_TUNNEL_CLOSED" || s == "WIFI_REKEY_REQUIRED" || s == "WIFI_IMS_REGISTRATION_EXPIRED"
}
func wifiConnect(ctx context.Context, sim *wifiSIM, hold time.Duration, live bool, emit func(string)) (err error) {
	call, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	ike, err := openWiFiIKE(call, sim)
	if err != nil {
		return err
	}
	defer ike.close()
	emit("ike-ready")
	parts, spi, err := ike.authenticate(call)
	if err != nil {
		return err
	}
	emit("ike-authenticated")
	child, err := newWiFiChild(ike, parts, spi)
	if err != nil {
		return err
	}
	defer child.close()
	emit("tunnel-ready")
	ims, err := newWiFiIMS(child, sim)
	if err != nil {
		return err
	}
	defer func() {
		if e := ims.close(); e != nil {
			err = e
		} else {
			emit("ims-cleaned")
		}
	}()
	emit("ims-registering")
	if _, err = ims.register(call); err != nil {
		return err
	}
	cancel()
	emit("connected")
	until := time.Time{}
	if !live {
		if hold <= 0 {
			return nil
		}
		until = time.Now().Add(hold)
		ims.renewAt = time.Now().Add(30 * time.Second)
	}
	return ims.maintain(ctx, emit, until)
}
func WiFiIssue(err error) string {
	if err == nil {
		return ""
	}
	code := err.Error()
	for _, v := range []string{"DEVICE_CHANGED", "SIM_NOT_READY", "DEVICE_BUSY", "PERMISSION_DENIED", "READ_TIMEOUT", "WIFI_RADIO_RESTORE_UNCONFIRMED", "WIFI_RADIO_UNCONFIRMED", "WIFI_SIM_CLEANUP_UNCONFIRMED", "WIFI_IMS_DEREGISTER_UNCONFIRMED", "WIFI_MODEM_UNSUPPORTED", "WIFI_DATA_ACTIVE", "WIFI_DATA_STATE_UNKNOWN", "WIFI_CERTIFICATE_INVALID", "WIFI_PEER_AUTH_FAILED", "WIFI_AUTH_REJECTED", "AKA_REJECTED", "WIFI_IMS_AKA_RESYNC_REQUIRED", "WIFI_IMS_SECURITY_UNSUPPORTED", "WIFI_IMS_AUTH_UNSUPPORTED"} {
		if code == v {
			return v
		}
	}
	if strings.HasPrefix(code, "WIFI_IMS_STATUS_") {
		return "WIFI_IMS_REJECTED"
	}
	if wifiRetryable(err) {
		return "WIFI_NETWORK_UNAVAILABLE"
	}
	return "WIFI_CONNECTION_FAILED"
}
