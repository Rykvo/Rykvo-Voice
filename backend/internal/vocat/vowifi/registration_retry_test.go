package vowifi

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type temporaryIMSError struct{}

func (temporaryIMSError) Error() string                            { return "initial REGISTER was rejected: SIP 480" }
func (temporaryIMSError) IMSRegistrationRetryAfter() time.Duration { return 15 * time.Minute }

type temporaryIMSProvider struct{ IMSProvider }

func (temporaryIMSProvider) Start(context.Context, IMSRequest) (IMSSession, error) {
	return nil, fmt.Errorf("register: %w", temporaryIMSError{})
}

func TestIMSRegistrationRetryEvidenceAndRecovery(t *testing.T) {
	for _, mode := range []string{"initial", "runtime"} {
		t.Run(mode, func(t *testing.T) {
			env := newFakeEnvironment()
			env.imsFailures = make(chan error, 1)
			o := newTestOrchestrator(t, env, false)
			t.Cleanup(func() { _, _ = o.Disable(context.Background()) })
			provider := o.deps.IMS
			if mode == "initial" {
				o.deps.IMS = temporaryIMSProvider{provider}
			}
			state, err := o.Enable(context.Background())
			if mode == "initial" {
				if err == nil {
					t.Fatal("expected register failure")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				env.imsFailures <- fmt.Errorf("refresh: %w", temporaryIMSError{})
				deadline := time.Now().Add(time.Second)
				for state.Phase != PhaseFailed && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
					state = o.State()
				}
			}
			if state.Phase != PhaseFailed || state.IMSRetryAfter != 15*time.Minute || state.IMSReady || state.TunnelReady {
				t.Fatal(state)
			}
			o.deps.IMS = provider
			state, err = o.Enable(context.Background())
			if err != nil || !state.IMSReady || state.IMSRetryAfter != 0 {
				t.Fatal("stale retry after recovery", state, err)
			}
			if env.callCount("radio.restore") != 0 {
				t.Fatal("retry restored cellular radio")
			}
		})
	}
}
