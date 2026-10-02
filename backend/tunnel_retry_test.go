package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTunnelStartupRetry(t *testing.T) {
	manager := &tunnelManager{binding: tunnelBinding{Resume: true, Status: "connecting"}}
	calls := 0
	var delays []time.Duration
	err := manager.connectWithRetry(context.Background(), func(context.Context) error {
		calls++
		if calls <= 8 {
			return &cfRequestError{code: "CLOUDFLARE_UNAVAILABLE"}
		}
		return nil
	}, func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil })
	if err != nil || calls != 9 || len(delays) != 8 {
		t.Fatalf("retry: calls=%d err=%v", calls, err)
	}
	for i, want := range []time.Duration{2, 4, 8, 16, 30, 30, 30, 30} {
		if delays[i] != want*time.Second {
			t.Fatalf("delay %d: %s", i, delays[i])
		}
	}
}

func TestTunnelStartupPermanentErrors(t *testing.T) {
	for _, err := range []error{tunnelError("AUTH_REQUIRED"), tunnelError("DNS_CONFLICT"), &cfRequestError{code: "CLOUDFLARE_PERMISSION", status: 403}, tunnelError("CREDENTIALS_INVALID")} {
		manager := &tunnelManager{binding: tunnelBinding{Resume: true}}
		got := manager.connectWithRetry(context.Background(), func(context.Context) error { return err }, func(context.Context, time.Duration) error { t.Fatal("permanent error retried"); return nil })
		if got != err {
			t.Fatal(got)
		}
	}
	for _, binding := range []tunnelBinding{{}, {Resume: true, Removing: true}} {
		manager := &tunnelManager{binding: binding}
		manager.connectWithRetry(context.Background(), func(context.Context) error { return &cfRequestError{code: "CLOUDFLARE_UNAVAILABLE"} }, func(context.Context, time.Duration) error { t.Fatal("inactive tunnel retried"); return nil })
	}
}

func TestTunnelStartupRetryCancellationAndRateLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &tunnelManager{binding: tunnelBinding{Resume: true}}
	calls := 0
	err := manager.connectWithRetry(ctx, func(context.Context) error {
		calls++
		return &cfRequestError{code: "CLOUDFLARE_RATE_LIMITED", status: 429, retryAfter: 10 * time.Minute}
	}, func(ctx context.Context, delay time.Duration) error {
		if delay != 5*time.Minute {
			t.Fatalf("delay=%s", delay)
		}
		cancel()
		return waitCleanup(ctx, delay)
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
