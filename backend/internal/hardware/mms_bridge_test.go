package hardware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"rykvo.local/auth/internal/mms"
)

func TestMMSWorkerRequiresDurableStorage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	worker := newSMSWorker(ctx, &smsEventEncoder{encoder: json.NewEncoder(w)}, cancel)
	acknowledged := make(chan bool, 1)
	worker.setMMSHandler(func(ctx context.Context, c mmsCommand, store func(mms.PDU) error) (string, error) {
		err := store(mms.PDU{Type: 0x84, Parts: []mms.Part{{Type: "text/plain", Data: []byte("fixture")}}})
		acknowledged <- err == nil
		return "", err
	})
	command := []byte(`{"op":"mms-receive","id":"request-123","profile":{"mmsc":"http://mms.example"},"location":"http://mms.example/fixture","transaction":"transaction"}`)
	if !worker.command(command) {
		t.Fatal("request rejected")
	}
	decoder := json.NewDecoder(r)
	var event wifiWorkerEvent
	if err := decoder.Decode(&event); err != nil || event.MMSResult == nil || event.MMSResult.Content == nil {
		t.Fatal(event, err)
	}
	select {
	case <-acknowledged:
		t.Fatal("ack before storage")
	default:
	}
	if !worker.command([]byte(`{"op":"mms-stored","id":"request-123","stored":true}`)) {
		t.Fatal("stored rejected")
	}
	event = wifiWorkerEvent{}
	if err := decoder.Decode(&event); err != nil || event.MMSResult.Code != "" || event.MMSResult.Content != nil {
		t.Fatal(event, err)
	}
	worker.mmsWait.Wait()
	if !<-acknowledged {
		t.Fatal("not acknowledged")
	}
}
func TestMMSInvalidLocationDoesNotTouchWorker(t *testing.T) {
	client := &VocatWorkerClient{}
	err := client.ReceiveWiFiMMS(context.Background(), Candidate{}, "card", "request-123", testMMSProfile(), "http://127.0.0.1/", "t", func(mms.PDU) error { return nil })
	if !errors.Is(err, mms.ErrNetwork) {
		t.Fatal(err)
	}
}
func TestMMSLeaseCancellationPreservesReadinessBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &mmsTunnelProvider{lease: &mmsIMSLease{ctx: ctx}, nextAttempt: time.Now()}
	if _, err := p.exchange(context.Background(), mmsCommand{}, nil); !errors.Is(err, mms.ErrNetwork) {
		t.Fatal(err)
	}
}
