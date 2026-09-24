package carrierconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogHomeNetworkAndMVNO(t *testing.T) {
	id := Identity{MCC: "234", MNC: "10", IMSI: "234101234567890", SPN: "giffgaff"}
	s := Match(id)
	if !s.Valid() || s.Data.Status != "matched" || s.MMS.Status != "matched" || s.Data.Profile.APN != "giffgaff.com" || s.MMS.Profile.MMSC != "http://mmsc.mediamessaging.co.uk:8002" {
		t.Fatalf("giffgaff not matched: %+v", s)
	}
	public := s.Public()
	b, _ := json.Marshal(public)
	if strings.Contains(string(b), `"password"`) {
		t.Fatal("password exposed")
	}
	if s.Data.Profile.Password == "" {
		t.Fatal("public view mutated secret config")
	}
	id.MNC = "010"
	if Match(id).Data.Status == "matched" {
		t.Fatal("MNC length ignored")
	}
	id.MCC = "460"
	id.MNC = "00"
	if Match(id).Data.Status != "identity_required" {
		t.Fatal("serving network matched instead of home SIM")
	}
}
func TestAmbiguityAndMVNOSelectors(t *testing.T) {
	id := Identity{MCC: "234", MNC: "10", IMSI: "234101234567890", SPN: "Brand", GID1: "AB01", ICCID: "8944101234567890123"}
	base := Profile{MCC: "234", MNC: "10", APN: "base", Types: "default"}
	for _, tc := range []struct{ kind, match string }{{"spn", "brand"}, {"gid", "ab"}, {"imsi", "23410xxxx"}, {"iccid", "89999,894410"}} {
		p := base
		p.APN = "specific"
		p.MVNOType = tc.kind
		p.MVNOMatch = tc.match
		c := choose([]Profile{base, p}, id, "default")
		if c.Status != "matched" || c.Profile.APN != "specific" {
			t.Fatal(tc, c)
		}
	}
	other := base
	other.APN = "other"
	if choose([]Profile{base, other}, id, "default").Status != "ambiguous" {
		t.Fatal("guessed ambiguous APN")
	}
	base.Types = "ims,sos"
	if choose([]Profile{base}, id, "default").Status != "not_found" {
		t.Fatal("IMS context included")
	}
	other.Types = "mms"
	other.MMSC = "file:///etc/passwd"
	if choose([]Profile{other}, id, "mms").Status != "not_found" {
		t.Fatal("invalid MMSC accepted")
	}
	if len(catalog) < 2000 {
		t.Fatal("catalog missing")
	}
}
