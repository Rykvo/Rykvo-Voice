package hardware

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestIncomingSMSAdmissionIncludesStorageACK(t *testing.T) {
	for _, admitted := range []bool{true, false} {
		client, worker := net.Pipe()
		var stored, released atomic.Int32
		entered := make(chan struct{}, 1)
		s := newSMSClientSession(context.Background(), client, func(context.Context, SMSDelivery) error { stored.Add(1); return nil })
		s.receiveWork = func() func() {
			entered <- struct{}{}
			if !admitted {
				return nil
			}
			return func() { released.Add(1) }
		}
		done := make(chan bool, 1)
		go func() { done <- s.event(wifiWorkerEvent{SMS: &SMSDelivery{ID: "delivery", From: "123", TPDU: "0011"}}) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("admission not checked")
		}
		if released.Load() != 0 {
			t.Fatal("module released before storage ACK")
		}
		_ = worker.SetReadDeadline(time.Now().Add(time.Second))
		var ack smsCommand
		if err := json.NewDecoder(worker).Decode(&ack); err != nil {
			t.Fatal(err)
		}
		if !<-done || ack.Op != "sms-ack" || ack.Stored != admitted {
			t.Fatalf("incorrect acknowledgement: %+v", ack)
		}
		want := int32(0)
		if admitted {
			want = 1
		}
		if released.Load() != want || stored.Load() != want {
			t.Fatal("unadmitted storage or lease leak")
		}
		client.Close()
		worker.Close()
	}
}
