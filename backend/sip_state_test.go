package main

import (
	"context"
	"errors"
	"testing"

	"rykvo.local/auth/internal/telephony"
)

func TestSIPGatewayStatusReadRetriesOnlyBoundedTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		errors []string
		state  string
		calls  int
		failed bool
	}{
		{"late-status", []string{"READ_TIMEOUT"}, "active", 2, false},
		{"late-idle", []string{"READ_TIMEOUT", "READ_TIMEOUT"}, "idle", 3, false},
		{"stalled", []string{"READ_TIMEOUT", "READ_TIMEOUT", "READ_TIMEOUT"}, "", 3, true},
		{"removed", []string{"DEVICE_CHANGED"}, "", 1, true},
		{"invalid", []string{"VOICE_STATE_UNKNOWN"}, "", 1, true},
		{"media-lost", []string{"VOICE_STREAM_CLOSED"}, "", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			state, err := readCallState(context.Background(), func(ctx context.Context) (string, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded read")
				}
				calls++
				if calls <= len(tc.errors) {
					return "", errors.New(tc.errors[calls-1])
				}
				return tc.state, nil
			})
			if calls != tc.calls || state != tc.state || (err != nil) != tc.failed {
				t.Fatal(calls, state, err)
			}
		})
	}
}

func TestSIPGatewayStatusReadStopsOnHangup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	state, err := readCallState(ctx, func(context.Context) (string, error) {
		calls++
		cancel()
		return "", errors.New("READ_TIMEOUT")
	})
	if calls != 1 || state != "" || !errors.Is(err, context.Canceled) {
		t.Fatal(calls, state, err)
	}
	_, _ = readCallState(ctx, func(context.Context) (string, error) { t.Fatal("read after hangup"); return "", nil })
}

func TestSIPGatewayEndingIsNotActiveAccountBusy(t *testing.T) {
	c := newSIPGateway(&server{}).calls
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	c.active[1] = &sipOutgoing{ctx: ctx, call: telephony.Call{Module: "module-02"}, reg: telephony.Registration{Account: "a"}}
	if c.endingModuleLocked("a") != "" {
		t.Fatal("live call marked ending")
	}
	stop()
	if c.endingModuleLocked("a") != "module-02" || c.endingModuleLocked("b") != "" {
		t.Fatal("wrong cleanup owner")
	}
	delete(c.active, 1)
	in := &sipIncoming{ctx: ctx, call: telephony.Call{Module: "module-03"}, legs: map[telephony.ID]*sipIncomingLeg{1: {reg: telephony.Registration{Account: "a"}}}}
	c.incoming[1] = in
	if c.endingModuleLocked("a") != "module-03" || c.endingModuleLocked("b") != "" {
		t.Fatal("wrong incoming cleanup owner")
	}
}
