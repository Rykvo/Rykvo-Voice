package hardware

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"rykvo.local/auth/internal/carrierconfig"
	"strings"
	"testing"
	"time"
)

type workerFake func(context.Context, Candidate, string, string, func(string)) error

func (f workerFake) WiFi(ctx context.Context, c Candidate, id, iccid string, emit func(string)) error {
	return f(ctx, c, id, iccid, emit)
}
func workerRequest() wifiWorkerRequest {
	return wifiWorkerRequest{Endpoint: "usb:test", Generation: "generation", HardwareKey: "imei:" + strings.Repeat("a", 64), ICCID: "89123456789012345678"}
}

func TestWiFiWorkerCancellationWaitsForCleanupACK(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanupStarted, allowCleanup := make(chan struct{}), make(chan struct{})
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- serveWiFiWorker(context.Background(), server, server, workerFake(func(ctx context.Context, c Candidate, id, iccid string, emit func(string)) error {
			if c.Key != workerRequest().Endpoint || c.Generation != "generation" {
				return errors.New("bad identity")
			}
			emit("connected")
			<-ctx.Done()
			close(cleanupStarted)
			<-allowCleanup
			emit("radio-restored")
			return ctx.Err()
		}))
	}()
	done := make(chan error, 1)
	go func() {
		done <- wifiWorkerExchange(ctx, client, workerRequest(), func(s string) {
			if s == "connected" {
				cancel()
			}
		})
	}()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("cancel was not delivered")
	}
	select {
	case err := <-done:
		t.Fatalf("gate released before cleanup: %v", err)
	default:
	}
	close(allowCleanup)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup ACK not received")
	}
	<-workerDone
}
func TestWiFiWorkerEOFIsNotCleanupSuccess(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() { var request wifiWorkerRequest; _ = json.NewDecoder(server).Decode(&request); server.Close() }()
	err := wifiWorkerExchange(context.Background(), client, workerRequest(), func(string) {})
	if err == nil || err.Error() != "WIFI_RADIO_RESTORE_UNCONFIRMED" {
		t.Fatal(err)
	}
}

func TestWiFiWorkerPhoneEventSurvivesPrivateIPC(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- serveWiFiWorker(context.Background(), server, server, workerFake(func(_ context.Context, _ Candidate, _, _ string, emit func(string)) error {
			emit("phone:+12025550123")
			return nil
		}))
	}()
	var events []string
	err := wifiWorkerExchange(context.Background(), client, workerRequest(), func(s string) { events = append(events, s) })
	if err != nil || len(events) != 1 || events[0] != "phone:+12025550123" {
		t.Fatal("phone event missing", err)
	}
	<-done
}
func TestWiFiWorkerRejectsCommandInjectionAndLeaks(t *testing.T) {
	request, _ := json.Marshal(workerRequest())
	malformed := strings.TrimSuffix(string(request), "}") + `,"command":"reboot"}` + "\n"
	var out strings.Builder
	called := false
	err := serveWiFiWorker(context.Background(), strings.NewReader(malformed), &out, workerFake(func(context.Context, Candidate, string, string, func(string)) error { called = true; return nil }))
	if err == nil || called {
		t.Fatal("unknown RPC field accepted")
	}
	if wifiWorkerCode(errors.New("private IMSI and authentication keys")) != "WIFI_CONNECTION_FAILED" || wifiWorkerStage("carrier:bad\nsecret") {
		t.Fatal("unfiltered IPC")
	}
}
func TestWiFiWorkerDisconnectCancelsSession(t *testing.T) {
	request, _ := json.Marshal(workerRequest())
	done := make(chan error, 1)
	go func() {
		done <- serveWiFiWorker(context.Background(), strings.NewReader(string(request)+"\n"), io.Discard, workerFake(func(ctx context.Context, _ Candidate, _, _ string, _ func(string)) error {
			<-ctx.Done()
			return ctx.Err()
		}))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("orphan session")
	}
}

func TestWiFiWorkerStopCarriesExplicitRadioChoice(t *testing.T) {
	for _, restore := range []bool{false, true} {
		client, server := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		received := make(chan string, 1)
		go func() {
			defer server.Close()
			scanner := bufio.NewScanner(server)
			scanner.Scan()
			encoder := json.NewEncoder(server)
			encoder.Encode(wifiWorkerEvent{Stage: "connected"})
			scanner.Scan()
			received <- scanner.Text()
			encoder.Encode(wifiWorkerEvent{Done: true, Code: "CANCELLED"})
		}()
		err := wifiWorkerExchange(ctx, client, workerRequest(), func(string) { cancel() }, func() bool { return restore })
		client.Close()
		cancel()
		want := "stop"
		if restore {
			want = "stop-cellular"
		}
		if got := <-received; got != want || !errors.Is(err, context.Canceled) {
			t.Fatal(got, err)
		}
	}
}
func TestWiFiWorkerCarrierConfigIPC(t *testing.T) {
	selection := carrierconfig.Match(carrierconfig.Identity{MCC: "234", MNC: "10", SPN: "giffgaff", IMSI: "234101234567890"})
	b, _ := json.Marshal(selection)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- serveWiFiWorker(context.Background(), server, server, workerFake(func(_ context.Context, _ Candidate, _, _ string, emit func(string)) error {
			emit("config:" + string(b))
			return nil
		}))
	}()
	got := ""
	if err := wifiWorkerExchange(context.Background(), client, workerRequest(), func(s string) { got = s }); err != nil || got != "config:"+string(b) {
		t.Fatal("carrier config lost", err)
	}
	<-done
	if wifiWorkerStage("config:private") || wifiWorkerStage("diagnostic:ims-secret-identity") {
		t.Fatal("private event exposed")
	}
}
