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

func TestCarrierIDOnlyAPNsResolveAlternateHomePLMN(t *testing.T) {
	id := Identity{MCC: "310", MNC: "240", IMSI: "310240000000001"}
	s := Match(id)
	if s.MMS.Status != "matched" || s.MMS.Profile == nil || s.MMS.Profile.MMSC != "http://mms.msg.eng.t-mobile.com/mms/wapenc" || s.MMS.Profile.MCC != "310" || s.MMS.Profile.MNC != "240" {
		t.Fatalf("carrier-id resolution: %+v", s)
	}
	if !s.Valid() {
		t.Fatal("normalized identity invalid")
	}
	if carrierRank("1", Identity{MCC: "234", MNC: "10", IMSI: "234100000000001"}) >= 0 {
		t.Fatal("carrier ID broadened to unrelated PLMN")
	}
}

func TestMMSAPNSeparatesCellularAndIWLAN(t *testing.T) {
	for _, mnc := range []string{"240", "260"} {
		s := Match(Identity{MCC: "310", MNC: mnc, IMSI: "310" + mnc + "000000001"})
		if !s.Valid() || s.MMS.Profile == nil || s.MMSWiFi.Profile == nil || s.MMS.Profile.APN != "fast.t-mobile.com" || s.MMSWiFi.Profile.APN != "TMUS" || s.MMSWiFi.Profile.Protocol != "IPV6" {
			t.Fatalf("bearer-specific APN lost: %+v", s)
		}
		if s.MMSWiFi.Profile.MNC != mnc || s.MMSWiFi.Profile.MMSC != s.MMS.Profile.MMSC {
			t.Fatal("SIM identity or MMSC changed")
		}
	}
	s := Match(Identity{MCC: "234", MNC: "10", IMSI: "234101234567890", SPN: "giffgaff"})
	if !s.Valid() || *s.MMS.Profile != *s.MMSWiFi.Profile || s.MMSWiFi.Profile.APN == "TMUS" {
		t.Fatal("IWLAN override leaked to another carrier")
	}
	p := s.Public()
	if p.MMSWiFi.Profile.Password != "" || s.MMSWiFi.Profile.Password == "" {
		t.Fatal("IWLAN public view exposed or erased credentials")
	}
	s.MMSWiFi.Profile = nil
	if s.Valid() {
		t.Fatal("missing matched IWLAN profile accepted")
	}
}

func TestChinaMMSProfilesUseSIMHomeCarrier(t *testing.T) {
	for _, tc := range []struct{ mnc, apn, mmsc, proxy string }{
		{"00", "cmwap", "http://mmsc.monternet.com", "10.0.0.172"},
		{"07", "cmwap", "http://mmsc.monternet.com", "10.0.0.172"},
		{"01", "3gwap", "http://mmsc.myuni.com.cn", "10.0.0.172"},
		{"06", "3gwap", "http://mmsc.myuni.com.cn", "10.0.0.172"},
		{"03", "ctwap", "http://mmsc.vnet.mobi", "10.0.0.200"},
		{"11", "ctwap", "http://mmsc.vnet.mobi", "10.0.0.200"},
	} {
		s := Match(Identity{MCC: "460", MNC: tc.mnc, IMSI: "460" + tc.mnc + "0000000001"})
		p := s.MMS.Profile
		if !s.Valid() || s.MMS.Status != "matched" || p == nil || p.APN != tc.apn || p.MMSC != tc.mmsc || p.MMSProxy != tc.proxy || p.MMSPort != "80" {
			t.Fatalf("%s: %+v", tc.mnc, s)
		}
	}
}

func TestHongKongSIMKeepsHomeConfigWhenRoaming(t *testing.T) {
	for _, mnc := range []string{"03", "04", "05"} {
		s := Match(Identity{MCC: "454", MNC: mnc, IMSI: "454" + mnc + "0000000001"})
		for _, wifi := range []bool{false, true} {
			c := s.MMSForBearer(wifi, true)
			if c.Status != "matched" || c.Profile == nil || c.Profile.MCC != "454" || c.Profile.MNC != mnc || c.Profile.APN != "mobile.three.com.hk" || c.Profile.MMSC != "http://mms.um.three.com.hk:10021/mmsc" || c.Profile.MMSProxy != "mms.three.com.hk" {
				t.Fatal(mnc, wifi, c)
			}
		}
		// A visited PLMN must not replace the home identity.
		if Match(Identity{MCC: "460", MNC: "01", IMSI: "454" + mnc + "0000000001"}).MMS.Status != "identity_required" {
			t.Fatal("visited network selected")
		}
	}
}

func TestMMSBearerSelectionDoesNotMutateSIMProfile(t *testing.T) {
	s := Selection{MMS: Choice{Status: "matched", Profile: &Profile{APN: "cell", Protocol: "IPV6", RoamingProtocol: "IP"}}, MMSWiFi: Choice{Status: "matched", Profile: &Profile{APN: "wifi", Protocol: "IPV4V6", RoamingProtocol: "IP"}}}
	if s.MMSForBearer(false, true).Profile.Protocol != "IP" || s.MMSForBearer(false, false).Profile.Protocol != "IPV6" || s.MMSForBearer(true, true).Profile.Protocol != "IPV4V6" {
		t.Fatal("wrong bearer protocol")
	}
	c := s.MMSForBearer(false, true)
	c.Profile.APN = "changed"
	if s.MMS.Profile.APN != "cell" || s.MMS.Profile.Protocol != "IPV6" {
		t.Fatal("cached config mutated")
	}
}
