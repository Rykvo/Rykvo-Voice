package hardware

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type wifiMatch struct {
	PLMN  string `json:"plmn"`
	SPN   string `json:"spn"`
	GID1  string `json:"gid1"`
	GID2  string `json:"gid2"`
	ICCID string `json:"iccid"`
}
type wifiCarrier struct {
	ID          string      `json:"id"`
	Match       []wifiMatch `json:"match"`
	EPDG        string      `json:"epdg,omitempty"`
	Transport   string      `json:"transport,omitempty"`
	Contact     string      `json:"contact,omitempty"`
	Agent       string      `json:"agent,omitempty"`
	Country     string      `json:"country,omitempty"`
	Tags        []string    `json:"tags,omitempty"`
	Unsupported string      `json:"unsupported,omitempty"`
}
type wifiCarrierDocument struct {
	Version  int           `json:"version"`
	Profiles []wifiCarrier `json:"profiles"`
}

//go:embed wifi_carriers.json
var wifiCarrierData []byte
var carrierOnce sync.Once
var carrierRules []wifiCarrier
var carrierError error
var carrierID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,100}$`)
var epdgName = regexp.MustCompile(`^epdg[a-z0-9.-]*\.[a-z]{2,}$`)
var carrierHex = regexp.MustCompile(`^[0-9A-Fa-f]+$`)
var carrierCountry = regexp.MustCompile(`^(?:[A-Z]{2}|AUTO)$`)

func parseWiFiCarriers(data []byte) ([]wifiCarrier, error) {
	bad := errors.New("WIFI_CARRIER_CONFIG_INVALID")
	if len(data) > 2<<20 {
		return nil, bad
	}
	var d wifiCarrierDocument
	if json.Unmarshal(data, &d) != nil || d.Version != 1 || len(d.Profiles) > 2000 {
		return nil, bad
	}
	seen := map[string]bool{}
	for _, p := range d.Profiles {
		if !carrierID.MatchString(p.ID) || seen[p.ID] || len(p.Match) == 0 || len(p.Match) > 100 || p.Transport != "" && p.Transport != "tcp" && p.Transport != "udp" || p.EPDG != "" && !epdgName.MatchString(p.EPDG) {
			return nil, bad
		}
		seen[p.ID] = true
		if p.Contact != "" && p.Contact != "gsma" || len(p.EPDG) > 253 || p.Country != "" && !carrierCountry.MatchString(p.Country) || p.Unsupported != "" && p.Unsupported != "eap-method" {
			return nil, bad
		}
		for _, v := range append([]string{p.Agent, p.Country}, p.Tags...) {
			if len(v) > 256 || strings.ContainsAny(v, "\r\n\x00") {
				return nil, bad
			}
		}
		if len(p.Tags) > 16 {
			return nil, bad
		}
		for _, m := range p.Match {
			if !decimal(m.PLMN, 5, 6) || len(m.SPN) > 64 || len(m.GID1) > 32 || len(m.GID2) > 32 || len(m.ICCID) > 20 {
				return nil, bad
			}
			if strings.ContainsAny(m.SPN, "\r\n\x00") || m.GID1 != "" && !carrierHex.MatchString(m.GID1) || m.GID2 != "" && !carrierHex.MatchString(m.GID2) || m.ICCID != "" && !decimal(m.ICCID, 1, 20) {
				return nil, bad
			}
		}
	}
	return d.Profiles, nil
}
func loadWiFiCarriers() {
	carrierRules, carrierError = parseWiFiCarriers(wifiCarrierData)
	for i, p := range carrierRules {
		carrierRules[i] = builtinWiFiCarrierFacts(p)
	}
	if carrierError != nil {
		return
	}
	dir := os.Getenv("RYKVO_CARRIER_DIR")
	if dir == "" {
		dir = "/etc/rykvo-voice/carriers.d"
	}
	entries, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		return
	}
	if e != nil || len(entries) > 128 {
		carrierError = errors.New("WIFI_CARRIER_CONFIG_INVALID")
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, e := entry.Info()
		if e != nil || info.Size() > 2<<20 {
			carrierError = errors.New("WIFI_CARRIER_CONFIG_INVALID")
			return
		}
		data, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			carrierError = errors.New("WIFI_CARRIER_CONFIG_INVALID")
			return
		}
		rules, e := parseWiFiCarriers(data)
		if e != nil {
			carrierError = e
			return
		}
		carrierRules = append(carrierRules, rules...)
	}
}
func builtinWiFiCarrierFacts(p wifiCarrier) wifiCarrier {
	// Narrow interoperability facts. No country-wide transport switch.
	if p.ID == "o2-giffgaff-uk" {
		p.Transport = "tcp"
		p.Contact = "gsma"
		p.Country = "GB"
		p.Agent = "iOS/18.6.2 iPhone"
		p.Tags = []string{"+g.3gpp.mid-call", "+g.3gpp.smsip"}
	}

	if p.ID == "tmobile-ultramint-us" {
		p.Transport = "udp"
		p.Tags = []string{`+g.3gpp.accesstype="wlan1"`, "+g.3gpp.smsip-msisdnless", "+g.3gpp.smsip-msisdn-less"}
	}
	return p
}
func matchWiFiCarrier(m wifiMatch, id wifiIdentity) int {
	if m.PLMN != id.MCC+id.MNC {
		return -1
	}
	score := 100
	for _, v := range []struct {
		want, got string
		weight    int
	}{{m.GID1, id.GID1, 90}, {m.GID2, id.GID2, 85}, {m.ICCID, id.ICCID, 70}} {
		if v.want != "" {
			if !strings.HasPrefix(strings.ToUpper(v.got), strings.ToUpper(v.want)) {
				return -1
			}
			score += v.weight + len(v.want)
		}
	}
	if m.SPN != "" {
		if !strings.EqualFold(m.SPN, id.SPN) {
			return -1
		}
		score += 80
	}
	return score
}
func resolveWiFiCarrier(id wifiIdentity, rules []wifiCarrier) wifiCarrier {
	out := wifiCarrier{ID: "standard-3gpp"}
	best := -1
	for _, p := range rules {
		for _, m := range p.Match {
			if score := matchWiFiCarrier(m, id); score >= 0 && score >= best {
				out = p
				best = score
			}
		}
	}
	if out.Transport == "" {
		out.Transport = "tcp"
	}
	if out.Agent == "" {
		out.Agent = "Rykvo-Voice"
	}
	if out.Country == "AUTO" {
		out.Country = map[string]string{"234": "GB", "235": "GB", "310": "US", "311": "US", "312": "US", "313": "US", "262": "DE", "208": "FR", "454": "HK", "466": "TW", "440": "JP", "441": "JP", "505": "AU"}[id.MCC]
	}
	return out
}
func wifiSPN(raw []byte) string {
	if len(raw) < 2 {
		return ""
	}
	raw = bytes.TrimRight(raw[1:], "\xff\x00")
	// Reject unsupported SIM encodings rather than guess an operator match.
	for _, b := range raw {
		if b < 32 || b > 126 {
			return ""
		}
	}
	return strings.TrimSpace(string(raw))
}
