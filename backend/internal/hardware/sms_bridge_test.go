package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"rykvo.local/auth/internal/vocat/vowifi"
	"sync"
	"testing"
	"time"
)

func TestSMSWorkerSubmissionIsSingleAndBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	e := &smsEventEncoder{encoder: json.NewEncoder(&out)}
	w := newSMSWorker(ctx, e, cancel)
	started, release := make(chan struct{}), make(chan struct{})
	w.setSender(func(ctx context.Context, r vowifi.SMSSubmitRequest) (vowifi.SMSSubmitResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return vowifi.SMSSubmitResult{AllPartsAccepted: true, PartsTotal: 1, PartsAccepted: 1}, nil
	})
	b := []byte(`{"op":"sms-send","id":"request-123","to":"+12025550123","text":"test"}`)
	if !w.command(b) {
		t.Fatal("valid command rejected")
	}
	<-started
	if !w.command(b) {
		t.Fatal("duplicate must not cancel Wi-Fi")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		w.mu.Lock()
		busy := w.busy
		w.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stuck")
		}
		time.Sleep(time.Millisecond)
	}
	if !w.command(b) {
		t.Fatal("replay handling")
	}
	e.mu.Lock()
	data := out.String()
	e.mu.Unlock()
	if !bytes.Contains([]byte(data), []byte("SMS_DUPLICATE_REQUEST")) {
		t.Fatal("request replay not blocked")
	}
	if w.command([]byte(`{"op":"sms-send","id":"request-999","to":"123;AT","text":"x"}`)) {
		t.Fatal("bad recipient")
	}
}
func TestReceiveAcknowledgesOnlyDurableStorage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &smsEventEncoder{encoder: json.NewEncoder(&bytes.Buffer{})}
	w := newSMSWorker(ctx, e, cancel)
	v := SMSDelivery{ID: "network-call-id", From: "123", TPDU: "001122"}
	done := make(chan error, 1)
	go func() { done <- w.receive(ctx, v) }()
	deadline := time.Now().Add(time.Second)
	for {
		w.mu.Lock()
		ready := w.acks[v.ID] != nil
		w.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ack waiter")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("acknowledged before storage")
	default:
	}
	if !w.command([]byte(`{"op":"sms-ack","id":"network-call-id","stored":true}`)) {
		t.Fatal("ack rejected")
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestSMSWorkerEventEncodingConcurrent(t *testing.T) {
	var b bytes.Buffer
	e := &smsEventEncoder{encoder: json.NewEncoder(&b)}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.Encode(wifiWorkerEvent{Stage: "connected"}) }()
	}
	wg.Wait()
	d := json.NewDecoder(&b)
	for i := 0; i < 20; i++ {
		var event wifiWorkerEvent
		if d.Decode(&event) != nil || event.Stage != "connected" {
			t.Fatal("interleaved event")
		}
	}
}
func TestAbsentSMSSessionNeverSends(t *testing.T) {
	c := &VocatWorkerClient{}
	_, e := c.SendSMS(context.Background(), Candidate{Key: "missing"}, "89012345678901234567", "request-123", "+12025550123", "x")
	if e == nil || e.Error() != "SMS_NOT_READY" {
		t.Fatal(e)
	}
}
