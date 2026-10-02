package hardware

import (
	"context"
	"errors"
	"testing"
)

func TestManualRestartUsesVerifiedATOnce(t *testing.T) {
	for _, scenario := range []string{"ok", "lost-reply", "rejected", "wrong-imei", "busy", "cancelled", "replaced", "replaced-after-open", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			c := Candidate{Key: "usb:1-1", Kind: "usb", Vendor: "2c7c", Product: "0125", Model: "EC20F", Control: "/dev/cdc-wdm0", Generation: "1:9", Ports: []Port{{Path: "old-port"}}}
			fresh := c
			fresh.Ports = []Port{{Path: "current-port"}}
			calls, opened := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			port := &voiceClosingPort{atPort: &mmsTestPort{onWrite: func(b []byte) string {
				if string(b) != "AT+CFUN=1,1\r" {
					t.Fatalf("unexpected command: %q", b)
				}
				if scenario == "lost-reply" {
					return ""
				}
				if scenario == "rejected" {
					return "ERROR\r\n"
				}
				return "OK\r\n"
			}}}
			if scenario == "unsupported" {
				c.Model = "OTHER"
			}
			err := restartModuleAT(ctx, c, "123456789012345", func(context.Context) ([]Candidate, error) {
				calls++
				if scenario == "replaced" || (scenario == "replaced-after-open" && calls == 2) {
					return nil, nil
				}
				return []Candidate{fresh}, nil
			}, func(_ context.Context, candidate Candidate, imei string) (*atSession, error) {
				opened++
				if candidate.Ports[0].Path != "current-port" || imei != "123456789012345" {
					t.Fatal("stale port or missing expected IMEI")
				}
				if scenario == "wrong-imei" {
					return nil, errors.New("DEVICE_CHANGED")
				}
				if scenario == "busy" {
					return nil, errors.New("DEVICE_BUSY")
				}
				if scenario == "cancelled" {
					cancel()
				}
				return &atSession{port: port}, nil
			})
			wantWrites := 0
			if scenario == "ok" || scenario == "lost-reply" || scenario == "rejected" {
				wantWrites = 1
			}
			if writes := len(port.atPort.(*mmsTestPort).commands); writes != wantWrites || (err == nil) != (scenario == "ok") {
				t.Fatalf("writes=%d want=%d error=%v", writes, wantWrites, err)
			}
			if opened > 0 && scenario != "wrong-imei" && scenario != "busy" && port.closed != 1 {
				t.Fatal("AT port retained after restart")
			}
		})
	}
}

func TestRecoveryOnlyForCommunicationTimeout(t *testing.T) {
	for _, tc := range []struct {
		reading Reading
		stalled bool
	}{
		{Reading{Issue: "READ_TIMEOUT"}, true},
		{Reading{Issue: "QMI_READ_FAILED", Warnings: []string{"AT:READ_TIMEOUT", "--dms-get-ids:READ_TIMEOUT"}}, true},
		{Reading{Responsive: true, Warnings: []string{"AT:READ_TIMEOUT"}, ESIM: &ESIMInfo{Issue: "READ_TIMEOUT"}}, true},
		{Reading{Responsive: true, SIM: "absent"}, false},
		{Reading{Responsive: true, SIM: "SIM PIN"}, false},
		{Reading{Responsive: true, Registration: "denied"}, false},
		{Reading{Responsive: true, Registration: "searching"}, false},
		{Reading{Issue: "QMI_READ_FAILED"}, false},
		{Reading{Issue: "PERMISSION_DENIED"}, false},
		{Reading{Issue: "DEVICE_BUSY"}, false},
		{Reading{Issue: "OPERATION_ACTIVE"}, false},
		{Reading{Responsive: true, ESIM: &ESIMInfo{Issue: "ESIM_INTERRUPTED"}}, false},
		{Reading{Responsive: true, ESIM: &ESIMInfo{Issue: "READ_TIMEOUT"}}, false},
		{Reading{Issue: "QMI_READ_FAILED", Warnings: []string{"AT:PERMISSION_DENIED", "--dms-get-ids:READ_TIMEOUT"}}, false},
	} {
		if CommunicationStalled(tc.reading) != tc.stalled {
			t.Fatalf("%+v", tc.reading)
		}
	}
	good := Reading{Responsive: true, IMEI: "123456789012345", SIM: "absent"}
	if !CommunicationHealthy(good) {
		t.Fatal("no SIM is not a modem fault")
	}
	good.Warnings = []string{"AT:READ_TIMEOUT"}
	if CommunicationHealthy(good) {
		t.Fatal("QMI alone is not AT recovery")
	}
}

func TestRecoveryTargetAllowlist(t *testing.T) {
	c := Candidate{Kind: "usb", Vendor: "2c7c", Product: "0125", Model: "EC20-CE", Control: "/dev/cdc-wdm3", Generation: "2:13"}
	if !RecoverySupported(c) {
		t.Fatal("EC20")
	}
	for _, change := range []string{"reader", "vendor", "product", "model", "generation", "control"} {
		n := c
		switch change {
		case "reader":
			n.Kind = "reader"
		case "vendor":
			n.Vendor = "other"
		case "product":
			n.Product = "other"
		case "model":
			n.Model = "unknown"
		case "generation":
			n.Generation = ""
		case "control":
			n.Control = ""
		}
		if RecoverySupported(n) {
			t.Fatal(change)
		}
	}
}
