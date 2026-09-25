package hardware

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/modem"
	"rykvo.local/auth/internal/vocat/vowifi"
)

type vocatLifecycleFake struct {
	states     chan vowifi.State
	enableErr  error
	cleanupErr error
	stopped    bool
}

func (f *vocatLifecycleFake) Subscribe(int) (<-chan vowifi.State, func()) {
	return f.states, func() { f.stopped = true }
}
func (f *vocatLifecycleFake) Enable(context.Context) (vowifi.State, error) {
	if f.enableErr != nil {
		return vowifi.State{Phase: vowifi.PhaseFailed}, f.enableErr
	}
	return vowifi.State{Phase: vowifi.PhaseIMSReady, Sequence: 3, IMSReady: true, TunnelReady: true, CarrierProfile: "standard-3gpp"}, nil
}
func (f *vocatLifecycleFake) Disable(ctx context.Context) (vowifi.State, error) {
	if ctx.Err() != nil {
		panic("cleanup used cancelled request")
	}
	return vowifi.State{}, f.cleanupErr
}
func TestVocatLifecycleCancelsAndCleansBeforeReturning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &vocatLifecycleFake{states: make(chan vowifi.State, 3)}
	f.states <- vowifi.State{Sequence: 1, Phase: vowifi.PhaseSIMReady}
	var stages []string
	err := runVocatRegistration(ctx, f, func(s string) {
		stages = append(stages, s)
		if s == "connected" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || !f.stopped || stages[len(stages)-1] != "radio-restored" {
		t.Fatal(err, stages)
	}
}
func TestVocatDiagnosticIncludesSetupStageWithoutProviderText(t *testing.T) {
	for _, tt := range []struct{ class, message, want string }{
		{"ims_ready", "ims: initial REGISTER was rejected: SIP 403 Forbidden; private subscriber identity", "diagnostic:ims-initial-403"},
		{"ims_ready", "ims: authenticated REGISTER was rejected: SIP 403 Forbidden", "diagnostic:ims-authenticated-403"},
		{"tunnel_ready", "private peer connection reset", "diagnostic:tunnel-reset"},
		{"ims_registration", "SIP 503", "diagnostic:ims-throttled"},
	} {
		if got := vocatDiagnostic(vowifi.State{LastErrorClass: tt.class, LastError: tt.message}); got != tt.want {
			t.Errorf("diagnostic = %q, want %q", got, tt.want)
		}
		if !wifiWorkerStage(tt.want) || wifiWorkerStage(tt.want+"; private subscriber identity") {
			t.Errorf("worker diagnostic allowlist mismatch: %q", tt.want)
		}
	}
}

func TestVocatLifecycleDoesNotLeakProviderErrorsOrHideCleanup(t *testing.T) {
	for _, cleanup := range []error{nil, errors.New("private cleanup info")} {
		f := &vocatLifecycleFake{states: make(chan vowifi.State), enableErr: errors.New("secret subscriber identity"), cleanupErr: cleanup}
		err := runVocatRegistration(context.Background(), f, func(string) {})
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatal(err)
		}
		if cleanup != nil && err.Error() != "WIFI_RADIO_RESTORE_UNCONFIRMED" {
			t.Fatal(err)
		}
	}
}
func TestVocatFailureRevokesRegistration(t *testing.T) {
	f := &vocatLifecycleFake{states: make(chan vowifi.State, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var stages []string
	err := runVocatRegistration(ctx, f, func(s string) {
		stages = append(stages, s)
		if s == "connected" {
			f.states <- vowifi.State{Sequence: 4, Phase: vowifi.PhaseFailed, LastErrorClass: "ims_runtime"}
		}
	})
	if err == nil || stages[len(stages)-3] != "reconnecting" {
		t.Fatal(err, stages)
	}
}
func TestVocatMetadataUsesReferenceSizingAndEncoding(t *testing.T) {
	if vocatSIMSize(modem.Response{Lines: []string{`+CRSM: 144,0,"6203800101"`}}) != 1 {
		t.Fatal("GID length not read from FCP")
	}
	if vocatSIMGroup([]byte{255}) != "" || vocatSIMGroup([]byte{1, 2}) != "0102" {
		t.Fatal("GID padding")
	}
	if vocatSIMSPN(modem.Response{Lines: []string{`+CRSM: 144,0,"0080004C00650062006100720061FFFF"`}}) != "Lebara" {
		t.Fatal("UCS2 SPN")
	}
}

func TestVocatPhoneStoreBindsOnlyNetworkAssociatedNumber(t *testing.T) {
	var stages []string
	store := vocatPhoneStore{iccid: "89123456789012345678", emit: func(s string) { stages = append(stages, s) }}
	record := vowifi.PhoneRecord{ICCID: store.iccid, Number: "+12025550123", Source: vowifi.PhoneSourcePAssociatedURI}
	if err := store.SaveAssociatedNumber(context.Background(), record); err != nil || len(stages) != 1 || stages[0] != "phone:+12025550123" {
		t.Fatal("associated number lost", err)
	}
	for _, kind := range []string{"wrong-card", "untrusted-source", "imsi", "cancelled"} {
		r := record
		ctx := context.Background()
		switch kind {
		case "wrong-card":
			r.ICCID = "89123456789012345679"
		case "untrusted-source":
			r.Source = "imsi"
		case "imsi":
			r.Number = "310026123456789"
		case "cancelled":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		if store.SaveAssociatedNumber(ctx, r) == nil {
			t.Fatal("accepted invalid phone source", kind)
		}
	}
	if len(stages) != 1 {
		t.Fatal("rejected identity was emitted")
	}
}

type retryVocatFake struct {
	states chan vowifi.State
	calls  int
}

func (f *retryVocatFake) Subscribe(int) (<-chan vowifi.State, func()) { return f.states, func() {} }
func (f *retryVocatFake) Disable(context.Context) (vowifi.State, error) {
	panic("retry must retain the radio checkpoint")
}
func (f *retryVocatFake) Enable(context.Context) (vowifi.State, error) {
	f.calls++
	return vowifi.State{Phase: vowifi.PhaseFailed, Sequence: uint64(f.calls), LastErrorClass: "tunnel_runtime", LastError: "read timeout"}, errors.New("timeout")
}
func TestVocatRetriesContinuePastThreeUntilCancelledWithoutRadioRestoration(t *testing.T) {
	f := &retryVocatFake{states: make(chan vowifi.State)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := watchVocatRegistration(ctx, f, f.states, func(stage string) {
		if f.calls == 8 && stage == "diagnostic:network-retry-scheduled" {
			cancel()
		}
	}, []time.Duration{time.Millisecond, time.Millisecond})
	if !errors.Is(err, context.Canceled) || f.calls != 8 {
		t.Fatal(err, f.calls)
	}
}
func TestVocatRetryClassification(t *testing.T) {
	for _, message := range []string{"EOF", "ims: refresh registration: EOF", "connection closed", "registration has expired", "read timeout"} {
		if !vocatTransientFailure(vowifi.State{LastErrorClass: "ims_runtime", LastError: message}) {
			t.Fatal(message)
		}
	}
	for _, ready := range []bool{false, true} {
		if got := vocatTransientFailure(vowifi.State{LastErrorClass: "network_timeout", LastError: "i/o timeout", SIMReady: ready, AccessReady: ready}); got != ready {
			t.Fatal(ready, got)
		}
	}
	for _, message := range []string{"SIP 403 timeout", "SIP 401 EOF", "DEVICE_CHANGED timeout", "READ_TIMEOUT", "SIM_NOT_READY timeout"} {
		if vocatTransientFailure(vowifi.State{LastErrorClass: "ims_runtime", LastError: message}) {
			t.Fatal(message)
		}
	}
	if len(vocatRetryDelays) != 5 || vocatRetryDelays[0] != 2*time.Second || vocatRetryDelays[4] != 30*time.Second {
		t.Fatal(vocatRetryDelays)
	}
}
func TestVocatRetryDoesNotLoopOnOperatorOrCleanupFailure(t *testing.T) {
	for _, cause := range []string{"SIP 503 timeout", "certificate timeout", "authentication timeout", "SIP Retry-After: 300", "registration rejected", "unknown"} {
		if vocatTransientFailure(vowifi.State{LastErrorClass: "ims_runtime", LastError: cause}) {
			t.Fatal(cause)
		}
	}
	if vocatTransientFailure(vowifi.State{LastErrorClass: "ims_runtime", LastError: "read timeout", CleanupErrors: []string{"cleanup failed"}}) {
		t.Fatal("dirty session retried")
	}
	f := &retryVocatFake{states: make(chan vowifi.State)}
	ctx, cancel := context.WithCancel(context.Background())
	err := watchVocatRegistration(ctx, f, f.states, func(string) { cancel() }, []time.Duration{time.Hour})
	if !errors.Is(err, context.Canceled) || f.calls != 1 {
		t.Fatal(err, f.calls)
	}
}

type recoverVocatFake struct{ retryVocatFake }

func (f *recoverVocatFake) Enable(ctx context.Context) (vowifi.State, error) {
	if f.calls == 0 {
		return f.retryVocatFake.Enable(ctx)
	}
	f.calls++
	return vowifi.State{Phase: vowifi.PhaseIMSReady, Sequence: uint64(f.calls), IMSReady: true, TunnelReady: true}, nil
}
func TestVocatTransientFailureReturnsToConnected(t *testing.T) {
	f := &recoverVocatFake{retryVocatFake{states: make(chan vowifi.State)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := false
	err := watchVocatRegistration(ctx, f, f.states, func(stage string) {
		if stage == "connected" {
			connected = true
			cancel()
		}
	}, []time.Duration{time.Millisecond})
	if !connected || f.calls != 2 || !errors.Is(err, context.Canceled) {
		t.Fatal("transient fault did not reconnect", f.calls, err)
	}
}

func TestRadioPolicyKeepsAirplaneUntilExplicitDisable(t *testing.T) {
	for _, restore := range []bool{false, true} {
		card := "+QCCID: 89123456789012345678\r\nOK"
		replies := []string{card, "+CFUN: 4\r\nOK", "+CFUN: 4\r\nOK", "+CGACT: 1,0\r\nOK", "+CGACT: 1,0\r\nOK"}
		if restore {
			replies = []string{card, "+CFUN: 4\r\nOK", "OK", "+CFUN: 1\r\nOK", "+CGACT: 1,0\r\nOK", "+CGACT: 1,0\r\nOK"}
		}
		port := &wifiDataTranscript{replies: replies}
		at := &vocatAT{session: &atSession{port: port}, device: "fixture", iccid: "89123456789012345678"}
		adapter, _ := vowifi.NewEC20Adapter(at, vowifi.EC20AdapterOptions{})
		radio := vocatRadio{EC20Adapter: adapter, at: at, restoreCellular: func() bool { return restore }}
		if err := radio.Restore(context.Background(), "fixture", vowifi.RadioSnapshot{OperatingMode: 1}); err != nil {
			t.Fatal(err)
		}
		hasOn := false
		for _, cmd := range port.commands {
			if cmd == "AT+CFUN=1" {
				hasOn = true
			}
			if strings.HasPrefix(cmd, "AT+CGACT=1") {
				t.Fatal("activated cellular data")
			}
		}
		if hasOn != restore {
			t.Fatal("radio policy changed")
		}
	}
}

type extendedRecoverVocatFake struct{ retryVocatFake }

func (f *extendedRecoverVocatFake) Enable(ctx context.Context) (vowifi.State, error) {
	if f.calls < 7 {
		return f.retryVocatFake.Enable(ctx)
	}
	f.calls++
	return vowifi.State{Phase: vowifi.PhaseIMSReady, Sequence: uint64(f.calls), IMSReady: true, TunnelReady: true}, nil
}
func TestVocatReconnectsAfterSevenConsecutiveNetworkFailures(t *testing.T) {
	f := &extendedRecoverVocatFake{retryVocatFake{states: make(chan vowifi.State)}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connected := false
	err := watchVocatRegistration(ctx, f, f.states, func(stage string) {
		if stage == "connected" {
			connected = true
			cancel()
		}
	}, []time.Duration{time.Millisecond, time.Millisecond})
	if !connected || f.calls != 8 || !errors.Is(err, context.Canceled) {
		t.Fatal(connected, f.calls, err)
	}
}
