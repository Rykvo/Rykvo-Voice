package hardware

import "testing"

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
