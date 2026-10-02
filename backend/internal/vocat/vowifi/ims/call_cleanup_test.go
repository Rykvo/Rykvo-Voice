package ims

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestCallCleanupDeadlineRequestsSessionRecoveryWithoutReleasingCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		call := &imsCall{public: vowifi.Call{ID: "stalled", State: "dialing"}, callID: "stalled"}
		s := &Session{refreshContext: ctx, failures: make(chan error, 1), calls: map[string]*imsCall{call.callID: call}}
		s.stopCallMedia(call.callID)
		time.Sleep(callCleanupTimeout - time.Second)
		s.stopCallMedia(call.callID) // Retries must not extend the deadline.
		select {
		case err := <-s.failures:
			t.Fatal("premature reset", err)
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case err := <-s.failures:
			if !errors.Is(err, ErrCallEnding) {
				t.Fatal(err)
			}
		default:
			t.Fatal("stalled call did not request recovery")
		}
		if call.public.State != "ending" || call.public.EndedAt != nil {
			t.Fatal("timeout falsely confirmed carrier termination", call.public)
		}
		s.recoverCallTermination(call.callID)
		select {
		case <-s.failures:
			t.Fatal("duplicate recovery")
		default:
		}
	})
}

func TestCallCleanupDeadlineIgnoresConfirmedAndStoppedSessions(t *testing.T) {
	for _, stopSession := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			call := &imsCall{public: vowifi.Call{ID: "call", State: "active"}, callID: "call"}
			s := &Session{refreshContext: ctx, failures: make(chan error, 1), calls: map[string]*imsCall{call.callID: call}}
			s.stopCallMedia(call.callID)
			if stopSession {
				cancel()
			} else {
				s.finishCall(call.callID, "ended", 200, "")
			}
			time.Sleep(callCleanupTimeout + time.Second)
			s.recoverCallTermination(call.callID)
			select {
			case err := <-s.failures:
				t.Fatal("recovered an ended session", err)
			default:
			}
		})
	}
}
