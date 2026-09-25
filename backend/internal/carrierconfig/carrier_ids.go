package carrierconfig

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed carrier_ids.json
var carrierIDsJSON []byte
var carrierIDs = func() map[string][]map[string][]string {
	var v map[string][]map[string][]string
	if json.Unmarshal(carrierIDsJSON, &v) != nil {
		panic("invalid carrier IDs")
	}
	return v
}()

// Conditions within an attribute are AND; repeated values and attribute groups
// are OR. Unknown constraints never broaden an MVNO to the host network.
func carrierRank(carrier string, id Identity) int {
	best := -1
	for _, group := range carrierIDs[carrier] {
		rank := 0
		valid := len(group["mccmnc_tuple"]) > 0
		for key, values := range group {
			match := false
			specific := 0
			for _, value := range values {
				hit, score := false, 0
				switch key {
				case "mccmnc_tuple":
					hit = value == id.MCC+id.MNC
				case "spn":
					hit = strings.EqualFold(value, id.SPN)
					score = 100 + len(value)
				case "gid1":
					hit = value != "" && id.GID1 != "" && strings.HasPrefix(strings.ToUpper(id.GID1), strings.ToUpper(value))
					score = 100 + len(value)
				case "gid2":
					hit = value != "" && id.GID2 != "" && strings.HasPrefix(strings.ToUpper(id.GID2), strings.ToUpper(value))
					score = 100 + len(value)
				case "iccid_prefix":
					hit = value != "" && id.ICCID != "" && strings.HasPrefix(id.ICCID, value)
					score = 100 + len(value)
				case "imsi_prefix_xpattern":
					hit, score = mvno(Profile{MVNOType: "imsi", MVNOMatch: value}, id)
				}
				if hit {
					match = true
					if score > specific {
						specific = score
					}
				}
			}
			valid = valid && match
			rank += specific
		}
		if valid && rank > best {
			best = rank
		}
	}
	return best
}
