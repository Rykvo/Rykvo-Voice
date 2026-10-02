package hardware

import (
	"context"
	"encoding/json"
	"net"
	"rykvo.local/auth/internal/entitlement"
	"testing"
	"time"
)

func TestEmergencyWorkerCancellationAndIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	worker := newSMSWorker(ctx, &smsEventEncoder{encoder: json.NewEncoder(a)}, cancel)
	started, stopped := make(chan struct{}), make(chan struct{})
	worker.setEmergencyHandler(func(ctx context.Context) (entitlement.Page, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return entitlement.Page{}, ctx.Err()
	})
	if worker.emergencyCommand([]byte(`{"op":"emergency-start","id":"fixture-id","url":"https://evil.example"}`)) {
		t.Fatal("arbitrary address accepted")
	}
	if !worker.emergencyCommand([]byte(`{"op":"emergency-start","id":"fixture-id"}`)) {
		t.Fatal("start rejected")
	}
	<-started
	if !worker.emergencyCommand([]byte(`{"op":"emergency-cancel","id":"other-id"}`)) {
		t.Fatal("cancel frame rejected")
	}
	select {
	case <-stopped:
		t.Fatal("wrong request cancelled")
	default:
	}
	if !worker.emergencyCommand([]byte(`{"op":"emergency-cancel","id":"fixture-id"}`)) {
		t.Fatal("cancel rejected")
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	var event wifiWorkerEvent
	if err := json.NewDecoder(b).Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.EmergencyResult == nil || event.EmergencyResult.Code != "EMERGENCY_UNAVAILABLE" || event.EmergencyResult.Page != nil {
		t.Fatal("cancel exposed page")
	}
	worker.setEmergencyHandler(nil)
	worker.emergencyWait.Wait()
	if worker.busy || worker.mmsBusy {
		t.Fatal("other business state changed")
	}
}
func TestEmergencyWorkerReplyValidation(t *testing.T) {
	s := &smsClientSession{emergencyPending: map[string]chan emergencyReply{"fixture-id": make(chan emergencyReply, 1)}}
	for _, r := range []emergencyReply{{ID: "fixture-id"}, {ID: "fixture-id", Code: "error", Page: &entitlement.Page{}}, {ID: "x", Code: "error"}} {
		if s.emergencyEvent(r) {
			t.Fatal("invalid reply accepted")
		}
	}
	if !s.emergencyEvent(emergencyReply{ID: "fixture-id", Page: &entitlement.Page{URL: "https://example.test", Token: "fixture"}}) {
		t.Fatal("reply rejected")
	}
}
