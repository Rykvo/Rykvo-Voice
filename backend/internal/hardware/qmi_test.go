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
