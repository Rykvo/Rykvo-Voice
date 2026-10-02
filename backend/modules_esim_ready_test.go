package main

import (
	"testing"

	"rykvo.local/auth/internal/hardware"
)

func TestModuleEnableWaitsForModemIdentity(t *testing.T) {
	card := "89123456789012345678"
	info := &hardware.ESIMInfo{EID: "89049032001001234500012345678901", Profiles: []hardware.ESIMProfile{{ICCID: card, Enabled: true}}}
	job := moduleJob{Action: "enable", Verification: &moduleVerification{EID: info.EID, ICCID: card, IMEI: "123456789012345"}}
	ready := hardware.Reading{Responsive: true, IMEI: job.Verification.IMEI, ESIM: info, SIM: "READY", ICCID: card}
	if !job.confirmed(ready) {
		t.Fatal("ready target was not confirmed")
	}
	for _, phase := range []string{"refreshing", "pin", "missing-iccid", "old-iccid", "read-error", "offline"} {
		t.Run(phase, func(t *testing.T) {
			reading := ready
			switch phase {
			case "refreshing":
				reading.SIM = "unknown"
			case "pin":
				reading.SIM = "SIM PIN"
			case "missing-iccid":
				reading.ICCID = ""
			case "old-iccid":
				reading.ICCID = "89123456789012345679"
			case "read-error":
				reading.Issue = "READ_TIMEOUT"
			case "offline":
				reading.Responsive = false
			}
			if job.confirmed(reading) {
				t.Fatal("profile inventory alone completed activation before modem refresh")
			}
		})
	}
}

func TestModuleVerifiedProfileDoesNotBypassActivationRead(t *testing.T) {
	for _, action := range []string{"enable", "disable", "download", "rename", "delete", "notifications"} {
		job := moduleJob{Action: action}
		job.finishUnconfirmedESIM(hardware.ESIMResult{Verified: true, Changed: true})
		if action == "enable" {
			if job.State != "uncertain" || job.Issue != "ESIM_RESULT_UNKNOWN" {
				t.Fatal("profile-only fallback bypassed activation read")
			}
		} else if job.State != "succeeded" {
			t.Fatal("unrelated eSIM operation lost its verified result", action)
		}
	}
}

func TestModuleUnconfirmedESIMRetainsOriginalFailure(t *testing.T) {
	for _, issue := range []string{"ESIM_SERVER_REJECTED", "ESIM_CARD_REJECTED", "ESIM_NETWORK_FAILED", "READ_TIMEOUT", ""} {
		job := moduleJob{Action: "download"}
		job.finishUnconfirmedESIM(hardware.ESIMResult{Changed: true, Issue: issue})
		want := issue
		if want == "" {
			want = "ESIM_RESULT_UNKNOWN"
		}
		if job.State != "uncertain" || job.Issue != want {
			t.Fatalf("lost original error or allowed automatic replay: %+v", job)
		}
	}
}
