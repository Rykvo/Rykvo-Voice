// Package carrierconfig resolves offline carrier defaults without activating data.
package carrierconfig

import (
	_ "embed"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

//go:embed catalog.json
var catalogJSON []byte

const Version = "aosp-73f904fbeb87-cid-bca387f553a4"

type Identity struct{ MCC, MNC, SPN, GID1, GID2, IMSI, ICCID string }
type Profile struct {
	CarrierID       string `json:"carrier_id,omitempty"`
	Carrier         string `json:"carrier"`
	MCC             string `json:"mcc"`
	MNC             string `json:"mnc"`
	APN             string `json:"apn"`
	User            string `json:"user,omitempty"`
	Password        string `json:"password,omitempty"`
	Auth            string `json:"authtype,omitempty"`
	Types           string `json:"type"`
	Protocol        string `json:"protocol,omitempty"`
	RoamingProtocol string `json:"roaming_protocol,omitempty"`
	MMSC            string `json:"mmsc,omitempty"`
	MMSProxy        string `json:"mmsproxy,omitempty"`
	MMSPort         string `json:"mmsport,omitempty"`
	MVNOType        string `json:"mvno_type,omitempty"`
	MVNOMatch       string `json:"mvno_match_data,omitempty"`
}
type Choice struct {
	Status  string   `json:"status"`
	Profile *Profile `json:"profile,omitempty"`
}
type Selection struct {
	Version string `json:"version"`
	MCC     string `json:"mcc"`
	MNC     string `json:"mnc"`
	Data    Choice `json:"data"`
	MMS     Choice `json:"mms"`
}

var catalog = func() []Profile {
	var p []Profile
	if json.Unmarshal(catalogJSON, &p) != nil {
		panic("invalid bundled APN catalog")
	}
	return p
}()

func digits(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func Match(id Identity) Selection {
	s := Selection{Version: Version, MCC: id.MCC, MNC: id.MNC, Data: Choice{Status: "identity_required"}, MMS: Choice{Status: "identity_required"}}
	if !digits(id.MCC, 3, 3) || !digits(id.MNC, 2, 3) || !strings.HasPrefix(id.IMSI, id.MCC+id.MNC) {
		return s
	}
	s.Data = choose(catalog, id, "default")
	s.MMS = choose(catalog, id, "mms")
	return s
}
func mvno(p Profile, id Identity) (bool, int) {
	v := strings.TrimSpace(p.MVNOMatch)
	switch strings.ToLower(p.MVNOType) {
	case "":
		return true, 0
	case "spn":
		return v != "" && strings.EqualFold(v, strings.TrimSpace(id.SPN)), 100 + len(v)
	case "gid":
		return v != "" && strings.HasPrefix(strings.ToUpper(id.GID1), strings.ToUpper(v)), 100 + len(v)
	case "imsi":
		if v == "" || len(id.IMSI) < len(v) {
			return false, 0
		}
		for i, c := range v {
			if c != 'x' && c != 'X' && byte(c) != id.IMSI[i] {
				return false, 0
			}
		}
		return true, 100 + len(v)
	case "iccid":
		for _, prefix := range strings.Split(v, ",") {
			prefix = strings.TrimSpace(prefix)
			if prefix != "" && strings.HasPrefix(id.ICCID, prefix) {
				return true, 100 + len(prefix)
			}
		}
	}
	return false, 0
}
func choose(all []Profile, id Identity, kind string) Choice {
	best := -1
	selected := map[string]Profile{}
	for _, p := range all {
		if !hasType(p.Types, kind) {
			continue
		}
		ok, rank := mvno(p, id)
		if p.CarrierID != "" {
			n := carrierRank(p.CarrierID, id)
			if n < 0 {
				continue
			}
			rank += n
		} else if p.MCC != id.MCC || p.MNC != id.MNC {
			continue
		}
		p.MCC, p.MNC = id.MCC, id.MNC
		if !ok || rank < best {
			continue
		}
		if !validProfile(p, kind) {
			continue
		}
		if rank > best {
			best = rank
			selected = map[string]Profile{}
		}
		// Metadata differences do not make identical connection parameters ambiguous.
		comparable := p
		comparable.Carrier = ""
		comparable.CarrierID = ""
		comparable.MVNOType = ""
		comparable.MVNOMatch = ""
		comparable.Types = kind
		if comparable.Protocol == "" {
			comparable.Protocol = "IP"
		}
		if comparable.RoamingProtocol == "" {
			comparable.RoamingProtocol = "IP"
		}
		b, _ := json.Marshal(comparable)
		selected[string(b)] = p
	}
	if len(selected) == 0 {
		return Choice{Status: "not_found"}
	}
	if len(selected) > 1 {
		return Choice{Status: "ambiguous"}
	}
	for _, p := range selected {
		if p.Protocol == "" {
			p.Protocol = "IP"
		}
		if p.RoamingProtocol == "" {
			p.RoamingProtocol = "IP"
		}
		return Choice{Status: "matched", Profile: &p}
	}
	return Choice{Status: "not_found"}
}
func hasType(types, kind string) bool {
	for _, t := range strings.Split(types, ",") {
		if strings.TrimSpace(t) == kind || strings.TrimSpace(t) == "*" {
			return true
		}
	}
	return false
}
func validProfile(p Profile, kind string) bool {
	if len(p.APN) > 253 || p.APN == "" || strings.ContainsAny(p.APN, "\r\n\x00\"\\") {
		return false
	}
	if kind == "mms" {
		u, e := url.Parse(p.MMSC)
		if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
		if p.MMSPort != "" {
			n, e := strconv.Atoi(p.MMSPort)
			if e != nil || n < 1 || n > 65535 {
				return false
			}
		}
	}
	return true
}

// Valid rejects malformed worker events and stale catalog versions.
func (s Selection) Valid() bool {
	if s.Version != Version || !digits(s.MCC, 3, 3) || !digits(s.MNC, 2, 3) {
		return false
	}
	for kind, c := range map[string]Choice{"default": s.Data, "mms": s.MMS} {
		switch c.Status {
		case "matched":
			if c.Profile == nil || c.Profile.MCC != s.MCC || c.Profile.MNC != s.MNC || !validProfile(*c.Profile, kind) {
				return false
			}
		case "not_found", "ambiguous":
			if c.Profile != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func (s Selection) Public() Selection {
	for _, c := range []*Choice{&s.Data, &s.MMS} {
		if c.Profile != nil {
			p := *c.Profile
			p.Password = ""
			p.MVNOMatch = ""
			c.Profile = &p
		}
	}
	return s
}
