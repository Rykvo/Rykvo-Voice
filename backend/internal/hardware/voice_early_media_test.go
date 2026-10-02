package hardware

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestWiFiStreamEarlyMediaAndStateWake(t *testing.T) {
	f := newStreamController()
	v, w := streamTestOpen(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := v.Dial(ctx, "+12025550123"); err != nil {
		t.Fatal(err)
	}
	// Drain initial state events, independently of operation acknowledgements.
	for len(v.events) > 0 {
		<-v.events
	}
	f.mu.Lock()
	f.calls[0].State, f.calls[0].Codec, f.calls[0].MediaReady = "early_media", "PCMU", true
	f.mu.Unlock()
	select {
	case <-v.StateChanges():
	case <-time.After(350 * time.Millisecond):
		t.Fatal("state waited for heartbeat")
	}
	if state, err := v.State(ctx); err != nil || state != "early_media" {
		t.Fatal(state, err)
	}
	p := make([]int16, 160)
	for i := range p {
		p[i] = 2000
	}
	f.read <- p
	got, err := v.ReadPCM(ctx)
	if err != nil || !reflect.DeepEqual(p, got) {
		t.Fatal("ringback lost", err)
	}
	if err := v.WritePCM(p); err == nil {
		t.Fatal("microphone opened before answer")
	}
	if _, err := f.AnswerCall(ctx, "carrier-call"); err != nil {
		t.Fatal(err)
	}
	if err := v.wait(ctx, func(s voiceStreamState) bool { return s.State == "active" }); err != nil {
		t.Fatal(err)
	}
	if err := v.WritePCM(p); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.written:
	case <-ctx.Done():
		t.Fatal("answered uplink absent")
	}
	if err := v.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
	waitStreamClosed(t, w)
}

func TestWiFiStateNotificationDoesNotConsumeOperationWake(t *testing.T) {
	v := &WiFiCall{events: make(chan struct{}, 1), changed: make(chan struct{}, 1)}
	v.changed <- struct{}{}
	for i := 0; i < 100; i++ {
		v.notifyState()
	}
	if len(v.events) != 1 || len(v.changed) != 1 {
		t.Fatal("unbounded or shared notification")
	}
	<-v.StateChanges()
	if len(v.changed) != 1 {
		t.Fatal("operation acknowledgement stolen")
	}
}
