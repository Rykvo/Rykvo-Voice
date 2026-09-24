package hardware

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQMIActiveSlotAndSIMState(t *testing.T) {
	slots := "Physical slot 1:\nCard status: present\nSlot status: inactive\nICCID: 89123456789012345670\nPhysical slot 2:\nCard status: present\nSlot status: active\nLogical slot: 1\nICCID: 89123456789012345678\n"
	if got := qmiSlotICCID(slots); got != "89123456789012345678" {
		t.Fatal(got)
	}
	if got := qmiSlotICCID(strings.ReplaceAll(slots, "active", "inactive")); got != "" {
		t.Fatal(got)
	}
	for state, want := range map[string]string{"ready": "READY", "pin1-or-upin-pin-required": "SIM PIN", "puk1-or-upin-puk-required": "SIM PUK", "detected": "unknown"} {
		text := "Slot [1]:\nCard state: 'present'\nApplication state: '" + state + "'\nSlot [2]:\nApplication state: 'ready'\n"
		if got := qmiSIMState(text); got != want {
			t.Fatalf("%s: %s", state, got)
		}
	}
	if got := qmiSIMState("Slot [1]:\nCard state: 'absent'\n"); got != "absent" {
		t.Fatal(got)
	}
}

func TestQMIPrivateQuery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket")
	}
	for _, value := range []string{`{"output":"IMEI: 'test'"}`, `{"error":"QMI_READ_FAILED"}`, `not json`, `{"output":"` + strings.Repeat("x", 65537) + `"}`} {
		t.Run(value[:8], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "qmi.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			requests := make(chan map[string]string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var request map[string]string
				json.NewDecoder(conn).Decode(&request)
				requests <- request
				conn.Write([]byte(value))
			}()
			output, err := proxyQuery(context.Background(), path, "/dev/cdc-wdm2", "--dms-get-ids")
			if request := <-requests; request["device"] != "/dev/cdc-wdm2" || request["command"] != "--dms-get-ids" {
				t.Fatal(request)
			}
			if strings.Contains(value, "test") {
				if err != nil || output != "IMEI: 'test'" {
					t.Fatalf("%q %v", output, err)
				}
			} else if err == nil {
				t.Fatal("invalid helper response accepted")
			}
		})
	}
}

func TestQMIQueryCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket")
	}
	path := filepath.Join(t.TempDir(), "qmi.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request map[string]string
		json.NewDecoder(conn).Decode(&request)
		cancel()
		conn.Read(make([]byte, 1))
	}()
	start := time.Now()
	if _, err := proxyQuery(ctx, path, "/dev/cdc-wdm2", "--dms-get-ids"); err == nil {
		t.Fatal("canceled query accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not close socket")
	}
	<-done
}
