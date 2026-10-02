package carrierconfig

import "testing"

func TestEmergencyCarrierDetection(t *testing.T) {
	good := Identity{MCC: "310", MNC: "280", IMSI: "310280000000000", GID1: "20FF"}
	if EmergencyMatch(good) != "att-mvno-us" {
		t.Fatal("verified MVNO not detected")
	}
	for _, change := range []func(*Identity){func(i *Identity) { i.MNC = "170" }, func(i *Identity) { i.GID1 = "" }, func(i *Identity) { i.GID1 = "FFFF" }, func(i *Identity) { i.IMSI = "454030000000000" }, func(i *Identity) { i.MCC = "454"; i.MNC = "03"; i.IMSI = "454030000000000" }} {
		id := good
		change(&id)
		if EmergencyMatch(id) != "" {
			t.Fatal("unverified carrier inherited address portal")
		}
	}
	good.GID1 = "20ff"
	s := Match(good)
	if !s.Valid() || s.Public().Emergency != "att-mvno-us" {
		t.Fatal("public capability missing")
	}
	s.Emergency = "untrusted-provider"
	if s.Valid() {
		t.Fatal("unknown route accepted")
	}
}
