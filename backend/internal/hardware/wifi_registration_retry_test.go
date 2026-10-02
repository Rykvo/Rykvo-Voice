package hardware

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func temporaryRegistrarState() vowifi.State {
	return vowifi.State{Phase: vowifi.PhaseFailed, SIMReady: true, AccessReady: true, LastErrorClass: "ims_ready", LastError: "initial REGISTER was rejected: SIP 480", IMSRetryAfter: time.Minute}
}

func TestVocatRegistrarCooldownAndLimits(t *testing.T) {
	s := temporaryRegistrarState()
	for attempt, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		got, ok := vocatRegistrationRetryDelay(s, vocatRetryDelays, 0, attempt)
		if !ok || got != want {
			t.Fatal(attempt, got, ok)
		}
	}
	s.IMSRetryAfter = time.Hour
	if got, ok := vocatRegistrationRetryDelay(s, vocatRetryDelays, 0, 0); !ok || got != time.Hour {
		t.Fatal("shortened registrar cooldown", got, ok)
	}
	for _, kind := range []string{"limit", "cleanup", "sim", "radio", "class", "disabled"} {
		s := temporaryRegistrarState()
		delays, attempt := vocatRetryDelays, 0
		switch kind {
		case "limit":
			attempt = 3
		case "cleanup":
			s.CleanupErrors = []string{"unsafe"}
		case "sim":
			s.SIMReady = false
		case "radio":
			s.AccessReady = false
		case "class":
			s.LastErrorClass = "tunnel_runtime"
		case "disabled":
			delays = nil
		}
		if _, ok := vocatRegistrationRetryDelay(s, delays, 0, attempt); ok {
			t.Fatal(kind)
		}
	}
}

type temporaryRegistrarFake struct {
	retryVocatFake
	at      []time.Time
	recover bool
}

func (f *temporaryRegistrarFake) Enable(context.Context) (vowifi.State, error) {
	f.calls++
	f.at = append(f.at, time.Now())
	s := temporaryRegistrarState()
	s.Sequence = uint64(f.calls)
	if f.recover && f.calls == 2 {
		return vowifi.State{Phase: vowifi.PhaseIMSReady, Sequence: s.Sequence, IMSReady: true, TunnelReady: true}, nil
	}
	return s, errors.New(s.LastError)
}

func TestVocatTemporaryRegistrarRetriesAreBoundedAndCancellable(t *testing.T) {
	for _, mode := range []string{"exhausted", "recovered", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &temporaryRegistrarFake{retryVocatFake: retryVocatFake{states: make(chan vowifi.State)}, recover: mode == "recovered"}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				connected := false
				err := watchVocatRegistration(ctx, f, f.states, func(stage string) {
					if stage == "retry-failed" {
						t.Error("registrar cooldown escalated to RF reset")
					}
					if stage == "connected" {
						connected = true
						cancel()
					}
					if mode == "cancelled" && stage == "diagnostic:network-retry-scheduled" {
						cancel()
					}
				}, vocatRetryDelays)
				if err == nil {
					t.Fatal("expected exit")
				}
				want := 4
				if mode == "recovered" {
					want = 2
					if !connected {
						t.Fatal("did not recover")
					}
				}
				if mode == "cancelled" {
					want = 1
				}
				if f.calls != want {
					t.Fatal(f.calls, want)
				}
				for i := 1; i < len(f.at); i++ {
					if got, want := f.at[i].Sub(f.at[i-1]), time.Minute<<(i-1); got != want {
						t.Fatal(got, want)
					}
				}
			})
		})
	}
}
