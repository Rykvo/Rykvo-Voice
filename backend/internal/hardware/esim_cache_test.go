package hardware

import (
	"context"
	"testing"
	"time"
)

func TestESIMInventoryCacheIsCardAndGenerationBound(t *testing.T) {
	s := NewSystem()
	c := Candidate{Key: "usb:test", Generation: "1", Kind: "modem"}
	r := Reading{Responsive: true, SIM: "READY", IMEI: "123456789012345", ICCID: "8986000000000000001"}
	calls := 0
	read := func(context.Context, ESIMRequest, func(string)) ESIMResult {
		calls++
		return ESIMResult{Info: &ESIMInfo{EID: "fixture", Profiles: []ESIMProfile{{ICCID: r.ICCID, Enabled: true}}}}
	}
	get := func() *ESIMInfo { return s.readESIMInventory(context.Background(), c, r, read) }
	get().Profiles[0].Label = "not stored"
	if v := get(); calls != 1 || v.Profiles[0].Label != "" {
		t.Fatal("cache alias or missed hit", calls, v)
	}
	r.ICCID = "8986000000000000002"
	get()
	c.Generation = "2"
	get()
	s.InvalidateESIM(c)
	get()
	if calls != 4 {
		t.Fatal("stale card/generation/write cache", calls)
	}
	s.esims.entries[c.Key].expires = time.Now().Add(-time.Second)
	get()
	if calls != 5 {
		t.Fatal("expired inventory not refreshed")
	}
	r.ICCID = ""
	get()
	get()
	if calls != 7 || len(s.esims.entries) != 0 {
		t.Fatal("unidentified card cached")
	}
}

func TestESIMInventoryInvalidationDuringRead(t *testing.T) {
	s := NewSystem()
	c := Candidate{Key: "fixture", Generation: "1"}
	r := Reading{Responsive: true, SIM: "READY", IMEI: "123456789012345", ICCID: "8986000000000000001"}
	read := func(context.Context, ESIMRequest, func(string)) ESIMResult {
		s.InvalidateESIM(c)
		return ESIMResult{Info: &ESIMInfo{EID: "previous"}}
	}
	s.readESIMInventory(context.Background(), c, r, read)
	if len(s.esims.entries) != 0 {
		t.Fatal("late inventory restored invalidated entry")
	}
}

func TestESIMInventoryFaultDoesNotGetLongSuccessTTL(t *testing.T) {
	s := NewSystem()
	c := Candidate{Key: "fixture", Generation: "1"}
	r := Reading{Responsive: true, SIM: "READY", IMEI: "123456789012345", ICCID: "8986000000000000001"}
	s.readESIMInventory(context.Background(), c, r, func(context.Context, ESIMRequest, func(string)) ESIMResult { return ESIMResult{Issue: "READ_TIMEOUT"} })
	if wait := time.Until(s.esims.entries[c.Key].expires); wait > 16*time.Second {
		t.Fatal("read error cached as healthy", wait)
	}
}
