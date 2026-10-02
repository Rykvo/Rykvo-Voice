package ims

import (
	"fmt"
	"strconv"
	"strings"
)

type amrOptions struct {
	wide, octet, modeSet, neighbor bool
	modes                          uint16
	period                         int
}

func amrDefaults(wide, octet bool) amrOptions {
	modes := uint16(255)
	if wide {
		modes = 511
	}
	return amrOptions{wide: wide, octet: octet, modes: modes, period: 1}
}

func (o amrOptions) name() string {
	if o.wide {
		return "AMR-WB"
	}
	return "AMR"
}
func (o amrOptions) clockScale() uint32 {
	if o.wide {
		return 2
	}
	return 1
}

func (o amrOptions) attributes(pt byte) []string {
	align := 0
	if o.octet {
		align = 1
	}
	params := fmt.Sprintf("octet-align=%d;mode-change-capability=2;max-red=0", align)
	if o.modeSet {
		var modes []string
		for i := 0; i < 9; i++ {
			if o.modes&(1<<i) != 0 {
				modes = append(modes, strconv.Itoa(i))
			}
		}
		params += ";mode-set=" + strings.Join(modes, ",")
	}
	return []string{fmt.Sprintf("a=rtpmap:%d %s/%d", pt, o.name(), 8000*o.clockScale()), fmt.Sprintf("a=fmtp:%d %s", pt, params)}
}

func amrRemoteOptions(body []byte, payload int, mapping string) (amrOptions, bool) {
	parts := strings.Split(strings.ToUpper(mapping), "/")
	if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "1") {
		return amrOptions{}, false
	}
	wide := parts[0] == "AMR-WB"
	if (!wide && parts[0] != "AMR") || (!wide && parts[1] != "8000") || (wide && parts[1] != "16000") {
		return amrOptions{}, false
	}
	o := amrDefaults(wide, false)
	inAudio := false
	seen := map[string]bool{}
	for _, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(line, "m=") {
			if inAudio {
				break
			}
			inAudio = strings.HasPrefix(line, "m=audio ")
		}
		if !inAudio {
			continue
		}
		if strings.HasPrefix(line, "a=maxptime:") {
			n, e := strconv.ParseFloat(strings.TrimPrefix(line, "a=maxptime:"), 64)
			if e != nil || !(n >= 20) {
				return o, false
			}
		}
		if !strings.HasPrefix(line, "a=fmtp:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "a=fmtp:"))
		if len(fields) < 2 || fields[0] != strconv.Itoa(payload) {
			continue
		}
		for _, item := range strings.Split(strings.Join(fields[1:], ""), ";") {
			if item == "" {
				continue
			}
			key, val, ok := strings.Cut(item, "=")
			if !ok {
				return o, false
			}
			if seen[key] {
				return o, false
			}
			seen[key] = true
			switch key {
			case "octet-align":
				if val != "0" && val != "1" {
					return o, false
				}
				o.octet = val == "1"
			case "crc", "robust-sorting":
				if val != "0" {
					return o, false
				}
			case "interleaving":
				return o, false // Requires a different packet layout, even when zero.
			case "channels":
				if val != "1" {
					return o, false
				}
			case "mode-set":
				var modes uint16
				for _, raw := range strings.Split(val, ",") {
					n, e := strconv.Atoi(raw)
					if e != nil || n < 0 || n > 8 || (!wide && n > 7) {
						return o, false
					}
					modes |= 1 << n
				}
				if modes == 0 {
					return o, false
				}
				o.modes, o.modeSet = modes, true
			case "mode-change-period":
				if val != "1" && val != "2" {
					return o, false
				}
				o.period, _ = strconv.Atoi(val)
			case "mode-change-neighbor":
				if val != "0" && val != "1" {
					return o, false
				}
				o.neighbor = val == "1"
			case "mode-change-capability":
				if val != "1" && val != "2" {
					return o, false
				}
			}
		}
	}
	return o, true
}

func amrOfferOptions(pt byte) (amrOptions, bool) {
	if pt < 96 || pt > 99 {
		return amrOptions{}, false
	}
	return amrDefaults(pt >= 98, pt%2 == 1), true
}
