package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"rykvo.local/auth/internal/hardware"
)

func TestSIPGatewayConfirmedMediaEndWaitsForCarrierState(t *testing.T) {
	for _, err := range []error{hardware.ErrVoiceEnded, fmt.Errorf("PCM: %w", hardware.ErrVoiceEnded), io.EOF, fmt.Errorf("VOICE_MEDIA_FAILED"), hardware.ErrDeviceUnavailable, fmt.Errorf("PCM: %w", hardware.ErrDeviceUnavailable)} {
		terminal := errors.Is(err, hardware.ErrVoiceEnded)
		want := "media_error"
		if errors.Is(err, hardware.ErrDeviceUnavailable) {
			want = "module_disconnected"
		}
		ctx, cancel := context.WithCancel(context.Background())
		out := &sipOutgoing{ctx: ctx, cancel: cancel}
		out.mediaFailed("uplink", err)
		if terminal && (ctx.Err() != nil || out.outcome != "") {
			t.Fatalf("confirmed end preempted carrier result: %v %s", err, out.outcome)
		}
		if !terminal && (ctx.Err() == nil || out.outcome != want) {
			t.Fatalf("real media failure hidden: %v %s", err, out.outcome)
		}
		cancel()
		ctx, cancel = context.WithCancel(context.Background())
		in := &sipIncoming{ctx: ctx, cancel: cancel}
		in.mediaFailed("uplink", err)
		if terminal && (ctx.Err() != nil || in.reason != "") {
			t.Fatalf("incoming end preempted carrier result: %v %s", err, in.reason)
		}
		if !terminal && (ctx.Err() == nil || in.reason != want) {
			t.Fatalf("incoming media failure hidden: %v %s", err, in.reason)
		}
		cancel()
	}
}
