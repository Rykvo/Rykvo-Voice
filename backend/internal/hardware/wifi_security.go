package hardware

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

func wifiHeaderValues(value string) ([]string, error) {
	if len(value) > 16384 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	var out []string
	start := 0
	quote, escape := false, false
	for i, ch := range value {
		if escape {
			escape = false
			continue
		}
		if ch == '\\' && quote {
			escape = true
			continue
		}
		if ch == '"' {
			quote = !quote
		}
		if ch == ',' && !quote {
			out = append(out, strings.TrimSpace(value[start:i]))
			start = i + 1
		}
	}
	if quote || escape {
		return nil, errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	out = append(out, strings.TrimSpace(value[start:]))
	if len(out) > 32 {
		return nil, errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	return out, nil
}
func wifiSecuritySelection(values []string) (map[string]string, string, error) {
	var chosen map[string]string
	quality := -1.0
	if len(values) > 8 {
		return nil, "", errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	for _, value := range values {
		offers, e := wifiHeaderValues(value)
		if e != nil {
			return nil, "", e
		}
		for _, offer := range offers {
			scheme, params, ok := strings.Cut(offer, ";")
			if !ok || !strings.EqualFold(strings.TrimSpace(scheme), "ipsec-3gpp") {
				continue
			}
			p, e := wifiParameters(params, ';')
			if e != nil {
				return nil, "", errors.New("WIFI_IMS_SECURITY_INVALID")
			}
			if p["alg"] != "hmac-sha-1-96" || p["ealg"] != "aes-cbc" || p["prot"] != "" && p["prot"] != "esp" || p["mod"] != "" && p["mod"] != "trans" {
				continue
			}
			q := 1.0
			if p["q"] != "" {
				q, e = strconv.ParseFloat(p["q"], 64)
				if e != nil || math.IsNaN(q) || q < 0 || q > 1 {
					return nil, "", errors.New("WIFI_IMS_SECURITY_INVALID")
				}
			}
			if q > quality {
				chosen = p
				quality = q
			}
		}
	}
	if chosen == nil {
		return nil, "", errors.New("WIFI_IMS_SECURITY_UNSUPPORTED")
	}
	return chosen, strings.Join(values, ", "), nil
}
