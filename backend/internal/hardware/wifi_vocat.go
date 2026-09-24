package hardware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"rykvo.local/auth/internal/vocat/device"
	"rykvo.local/auth/internal/vocat/modem"
	"rykvo.local/auth/internal/vocat/vowifi"
	"rykvo.local/auth/internal/vocat/vowifi/ike"
	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

// VocatWiFi bridges the imported protocol core to Rykvo's exclusive device gate.
// Select at service startup; it never falls back to the legacy engine mid-session.
type VocatWiFi struct {
	System          *System
	RestoreCellular func() bool
}

type vocatAT struct {
	mu            sync.Mutex
	session       *atSession
	device, iccid string
}

func (a *vocatAT) ExecuteAT(ctx context.Context, device, command string) (modem.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if device != a.device {
		return modem.Response{}, errors.New("DEVICE_CHANGED")
	}
	lines, err := a.session.exchange(ctx, command, 15*time.Second)
	if err != nil {
		return modem.Response{}, errors.New(errorCode(err))
	}
	// Do not retain commands: CGLA/CSIM may contain sensitive AKA material.
	return modem.Response{Lines: lines, Final: "OK"}, nil
}
func (a *vocatAT) ExecuteSensitiveAT(ctx context.Context, device, command string) (modem.Response, error) {
	return a.ExecuteAT(ctx, device, command)
}
func (a *vocatAT) verify(ctx context.Context) error {
	r, err := a.ExecuteAT(ctx, a.device, "AT+QCCID")
	if err != nil || digits(r.Lines, 18, 20) != a.iccid {
		return errors.New("DEVICE_CHANGED")
	}
	return nil
}
func (a *vocatAT) ReadSIMMetadata(ctx context.Context, device string) (vowifi.SIMMetadata, error) {
	if err := a.verify(ctx); err != nil {
		return vowifi.SIMMetadata{}, err
	}
	read := func(file int) []byte {
		header, err := a.ExecuteAT(ctx, device, fmt.Sprintf("AT+CRSM=192,%d,0,0,0", file))
		if err != nil {
			return nil
		}
		size := vocatSIMSize(header)
		if size < 1 || size > 64 {
			return nil
		}
		r, err := a.ExecuteAT(ctx, device, fmt.Sprintf("AT+CRSM=176,%d,0,0,%d", file, size))
		if err != nil {
			return nil
		}
		return vocatSIMPayload(r)
	}
	spn, _ := a.ExecuteAT(ctx, device, "AT+CRSM=176,28486,0,0,17")
	metadata := vowifi.SIMMetadata{SPN: vocatSIMSPN(spn), GID1: vocatSIMGroup(read(0x6f3e)), GID2: vocatSIMGroup(read(0x6f3f))}
	return metadata, a.verify(ctx)
}

// Keep the executor's device identifier distinct from the codec package name.
func vocatSIMSize(r modem.Response) int       { return device.SIMFileSize(device.SIMPayload(r)) }
func vocatSIMPayload(r modem.Response) []byte { return device.SIMPayload(r) }
func vocatSIMSPN(r modem.Response) string     { return device.SIMSPN(r) }
func vocatSIMGroup(b []byte) string           { return device.SIMGroupID(b) }

type vocatRadio struct {
	*vowifi.EC20Adapter
	at              *vocatAT
	network         string
	restoreCellular func() bool
}

func (r vocatRadio) Snapshot(ctx context.Context, id string) (vowifi.RadioSnapshot, error) {
	if err := r.at.verify(ctx); err != nil {
		return vowifi.RadioSnapshot{}, err
	}
	if err := wifiHostData(r.network); err != nil {
		return vowifi.RadioSnapshot{}, err
	}
	state, err := r.EC20Adapter.Snapshot(ctx, id)
	if err == nil && state.OperatingMode != 1 && state.OperatingMode != 4 {
		err = errors.New("WIFI_RADIO_STATE_UNKNOWN")
	}
	return state, err
}
func (r vocatRadio) Restore(ctx context.Context, id string, state vowifi.RadioSnapshot) error {
	if err := r.at.verify(ctx); err != nil {
		return err
	}
	state.PureAirplanePolicy = r.restoreCellular == nil || !r.restoreCellular()
	if !state.PureAirplanePolicy {
		state.OperatingMode = 1
	}
	return r.EC20Adapter.Restore(ctx, id, state)
}
func (r vocatRadio) EnterVoWiFiRFOff(ctx context.Context, id string) error {
	if err := r.at.verify(ctx); err != nil {
		return err
	}
	return r.EC20Adapter.EnterVoWiFiRFOff(ctx, id)
}
func (r vocatRadio) StopCellularData(ctx context.Context, id string) error {
	if err := r.at.verify(ctx); err != nil {
		return err
	}
	return r.EC20Adapter.StopCellularData(ctx, id)
}

type vocatDirectRoute struct{}

func (vocatDirectRoute) Resolve(context.Context, vowifi.ProxyRequest) (vowifi.ProxyRoute, error) {
	return vowifi.ProxyRoute{Mode: vowifi.ProxyModeDirect}, nil
}

// Registration-only integration has no phone/SMS delivery service yet.
// Fail unsupported delivery rather than silently acknowledge and discard messages.
type vocatPhoneStore struct {
	iccid string
	emit  func(string)
}

func (s vocatPhoneStore) SaveAssociatedNumber(ctx context.Context, record vowifi.PhoneRecord) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if record.ICCID != s.iccid || !ValidAssociatedNumber(record.Number) || s.emit == nil ||
		(record.Source != vowifi.PhoneSourceAssociatedMSISDN && record.Source != vowifi.PhoneSourcePAssociatedURI) {
		return errors.New("invalid associated number binding")
	}
	// Private IPC only; the device manager owns persistence and public views.
	// Never infer a phone number from IMSI/ICCID or include it in diagnostics.
	s.emit("phone:" + record.Number)
	return ctx.Err()
}

func ValidAssociatedNumber(number string) bool {
	return strings.HasPrefix(number, "+") && decimal(strings.TrimPrefix(number, "+"), 5, 15)
}

func (engine *VocatWiFi) WiFi(ctx context.Context, c Candidate, identity, iccid string, emit func(string)) error {
	if engine == nil || engine.System == nil || !decimal(iccid, 18, 20) {
		return errors.New("DEVICE_CHANGED")
	}
	found, err := engine.System.Discover(ctx)
	if err != nil {
		return err
	}
	valid := false
	for _, current := range found {
		if current.Key == c.Key && current.Generation == c.Generation {
			c, valid = current, true
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
	at := &vocatAT{session: session, device: "rv-" + Digest(identity)[:12], iccid: iccid}
	if err := at.verify(ctx); err != nil {
		return err
	}
	adapter, err := vowifi.NewEC20Adapter(at, vowifi.EC20AdapterOptions{PureAirplanePolicy: func(string) bool { return true }})
	if err != nil {
		return errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	if err := wifiHostData(c.Network); err != nil {
		return err
	}
	if _, err := at.ExecuteAT(ctx, at.device, "AT+CFUN=4"); err != nil {
		return errors.New("WIFI_RADIO_STATE_UNKNOWN")
	}
	if mode, err := wifiRadioMode(ctx, session); err != nil || mode != 4 {
		return errors.New("WIFI_RADIO_STATE_UNKNOWN")
	}
	emit("radio-off")
	// Reference diagnostics can contain subscriber identities. Only state/error
	// allowlists cross this boundary; do not forward raw SIP or provider logs.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tunnel, err := ike.NewProvider(ike.Config{APN: "ims", Logger: logger, AutoProposalFallback: true})
	if err != nil {
		return errors.New("WIFI_CONNECTION_FAILED")
	}
	registration, err := ims.NewProvider(adapter, ims.Config{
		Transport: "tcp", AutoTransportFallback: true, Logger: logger,
		OnDeregistrationUnconfirmed: func() { emit("diagnostic:ims-deregistration-unconfirmed") },
		OnSMS:                       func(context.Context, ims.ReceivedSMS) error { return errors.New("SMS storage not connected") },
		OnSIMDataDownload:           func(context.Context, ims.SIMDataDownload) error { return errors.New("SIM download not connected") },
	})
	if err != nil {
		return errors.New("WIFI_CONNECTION_FAILED")
	}
	o, err := vowifi.New(vowifi.Dependencies{SIM: carrierSIM{adapter, emit}, AKA: adapter, Radio: vocatRadio{adapter, at, c.Network, engine.RestoreCellular}, Proxy: vocatDirectRoute{}, Tunnel: tunnel, IMS: registration, Phones: vocatPhoneStore{iccid: iccid, emit: emit}}, vowifi.Options{DeviceID: at.device, AllowIMSWithoutSMS: true, CleanupTimeout: 25 * time.Second})
	if err != nil {
		return errors.New("WIFI_CONNECTION_FAILED")
	}
	return runVocatRegistration(ctx, o, emit)
}

type vocatLifecycle interface {
	Enable(context.Context) (vowifi.State, error)
	Disable(context.Context) (vowifi.State, error)
	Subscribe(int) (<-chan vowifi.State, func())
}

func runVocatRegistration(ctx context.Context, o vocatLifecycle, emit func(string)) (err error) {
	states, stop := o.Subscribe(8)
	defer stop()
	defer func() {
		emit("reconnecting") // Revoke readiness before any teardown work.
		clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		state, e := o.Disable(clean)
		if e != nil || len(state.CleanupErrors) != 0 {
			for _, failure := range state.CleanupErrors {
				component := "unknown"
				if strings.HasPrefix(failure, "close IMS:") {
					component = "ims"
				}
				if strings.HasPrefix(failure, "close tunnel:") {
					component = "tunnel"
				}
				if strings.HasPrefix(failure, "restore radio:") {
					component = "radio"
				}
				emit("diagnostic:cleanup-" + component)
			}
			err = errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
			return
		}
		emit("ims-cleaned")
		emit("radio-restored")
	}()
	return watchVocatRegistration(ctx, o, states, emit, vocatRetryDelays)
}

// Only one coordinator retries. Preserve the reference radio checkpoint between
// attempts; explicit shutdown performs the sole final radio restoration.
var vocatRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

func watchVocatRegistration(ctx context.Context, o vocatLifecycle, states <-chan vowifi.State, emit func(string), delays []time.Duration) error {
	failure := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		setup, cancel := context.WithTimeout(ctx, 120*time.Second)
		state, cause := o.Enable(setup)
		cancel()
		for draining := true; draining; {
			select {
			case s, ok := <-states:
				if !ok {
					return errors.New("WIFI_CONNECTION_FAILED")
				}
				if s.Sequence > state.Sequence {
					state = s
				}
			default:
				draining = false
			}
		}
		if cause == nil && state.Phase != vowifi.PhaseFailed {
			if state.IMSReady && state.TunnelReady {
				failure = 0
			}
			vocatEmitState(state, emit)
			for cause == nil && state.Phase != vowifi.PhaseFailed {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case s, ok := <-states:
					if !ok {
						return errors.New("WIFI_CONNECTION_FAILED")
					}
					if s.Sequence <= state.Sequence {
						continue
					}
					state = s
					if state.IMSReady && state.TunnelReady {
						failure = 0
					}
					vocatEmitState(state, emit)
				}
			}
		}
		emit("reconnecting")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		emit(vocatDiagnostic(state))
		if len(delays) == 0 || !vocatTransientFailure(state) {
			return vocatStateError(state, cause)
		}
		emit("diagnostic:network-retry-scheduled")
		timer := time.NewTimer(delays[failure])
		if failure < len(delays)-1 {
			failure++
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func vocatTransientFailure(s vowifi.State) bool {
	if len(s.CleanupErrors) != 0 {
		return false
	}
	switch s.LastErrorClass {
	case "tunnel_runtime", "ims_runtime":
	case "network_timeout", "timeout", "tunnel_ready", "ims_ready":
		// SIM, RF and serial failures are not network retries.
		if !s.SIMReady || !s.AccessReady {
			return false
		}
	default:
		return false
	}
	message := strings.ToLower(s.LastError)
	// Authentication, policy and server throttling are not network retries.
	for _, denied := range []string{"auth", "certificate", "security", "reject", "forbidden", "sip 401", "sip 403", "503", "retry-after", "429", "device_changed", "sim_not_ready", "read_timeout", "permission_denied"} {
		if strings.Contains(message, denied) {
			return false
		}
	}
	for _, transient := range []string{"timeout", "timed out", "connection reset", "broken pipe", "eof", "connection closed", "use of closed network connection", "registration has expired", "network is unreachable", "no route to host", "connection refused", "temporary failure in name resolution"} {
		if strings.Contains(message, transient) {
			return true
		}
	}
	return false
}

func vocatEmitState(state vowifi.State, emit func(string)) {
	if carrierID.MatchString(state.CarrierProfile) {
		emit("carrier:" + state.CarrierProfile)
	}
	if state.IMSReady && state.TunnelReady && state.Phase != vowifi.PhaseFailed && state.Phase != vowifi.PhaseStopping {
		emit("connected")
	} else {
		emit("reconnecting")
	}
}
func vocatStateError(state vowifi.State, cause error) error {
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if cause != nil {
		for _, code := range []string{"DEVICE_CHANGED", "WIFI_DATA_ACTIVE", "WIFI_DATA_STATE_UNKNOWN"} {
			if strings.Contains(cause.Error(), code) {
				return errors.New(code)
			}
		}
	}
	if len(state.CleanupErrors) != 0 {
		return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
	}
	// Do not expose upstream error text (may contain IMSI, APDU or network data).
	return errors.New("WIFI_CONNECTION_FAILED")
}

// Explicit user disable restores RF only; it never activates a PDP context.
func (engine *VocatWiFi) RestoreRadio(ctx context.Context, c Candidate, identity, iccid string) error {
	found, err := engine.System.Discover(ctx)
	if err != nil {
		return err
	}
	valid := false
	for _, candidate := range found {
		if candidate.Key == c.Key && candidate.Generation == c.Generation {
			c = candidate
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
	at := &vocatAT{session: session, device: c.Key, iccid: iccid}
	if err = at.verify(ctx); err != nil {
		return err
	}
	if _, err = at.ExecuteAT(ctx, c.Key, "AT+CFUN=1"); err != nil {
		return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
	}
	mode, err := wifiRadioMode(ctx, session)
	if err != nil || mode != 1 {
		return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
	}
	return at.verify(ctx)
}

func vocatDiagnostic(s vowifi.State) string {
	kind := "other"
	if s.LastErrorClass == "ims_runtime" {
		kind = "ims"
	}
	if s.LastErrorClass == "tunnel_runtime" {
		kind = "tunnel"
	}
	reason := "other"
	message := strings.ToLower(s.LastError)
	for _, v := range []struct{ pattern, code string }{{"timeout", "timeout"}, {"timed out", "timeout"}, {"closed", "closed"}, {"expired", "expired"}, {"eof", "eof"}, {"refresh", "refresh"}, {"403", "rejected"}, {"401", "auth"}, {"503", "throttled"}, {"connection reset", "reset"}} {
		if strings.Contains(message, v.pattern) {
			reason = v.code
			break
		}
	}
	return "diagnostic:" + kind + "-" + reason
}
