package main

import (
	"context"
	"encoding/json"
	"rykvo.local/auth/internal/carrierconfig"
	"testing"
)

func testCarrierSelection() carrierconfig.Selection {
	return carrierconfig.Match(carrierconfig.Identity{MCC: "234", MNC: "10", IMSI: "234101234567890", SPN: "giffgaff"})
}
func TestCarrierConfigRejectsStaleSIM(t *testing.T) {
	m, w, sample, _ := phoneFixture()
	b, _ := json.Marshal(testCarrierSelection())
	m.acceptCarrierConfig(context.Background(), 1, w, sample, string(b))
	if len(m.carrierConfigs) != 1 {
		t.Fatal("not accepted")
	}
	m.carrierConfigs = map[string]carrierconfig.Selection{}
	w.Enabled = false
	m.acceptCarrierConfig(context.Background(), 1, w, sample, string(b))
	if len(m.carrierConfigs) != 0 {
		t.Fatal("disabled accepted")
	}
}
func testCarrierDatabase(t *testing.T, s *server) {
	t.Helper()
	m := newModuleManager(s.db, nil)
	selection := testCarrierSelection()
	m.saveCarrierConfig(context.Background(), "89123456789012345678", selection)
	fresh := newModuleManager(s.db, nil)
	fresh.loadCarrierConfigs(context.Background())
	got, ok := fresh.carrierConfigs["89123456789012345678"]
	if !ok || got.MMS.Status != "matched" {
		t.Fatal("carrier config not persisted")
	}
	b, _ := json.Marshal(fresh.carrierView("89123456789012345678"))
	if string(b) == "null" {
		t.Fatal("public view missing")
	}
}
