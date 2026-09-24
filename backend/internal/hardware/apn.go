package hardware

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type APNConfig struct {
	APN      string `json:"apn"`
	Protocol string `json:"protocol"`
	Auth     string `json:"auth"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}
type APNContext struct {
	CID      int    `json:"cid"`
	APN      string `json:"apn"`
	Protocol string `json:"protocol"`
}

var apnNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,98}[A-Za-z0-9])?$`)

func ValidAPNConfig(v APNConfig) bool {
	if v.APN != "" && !apnNamePattern.MatchString(v.APN) {
		return false
	}
	if strings.EqualFold(v.APN, "ims") || strings.EqualFold(v.APN, "sos") {
		return false
	}
	if v.Protocol != "IP" && v.Protocol != "IPV6" && v.Protocol != "IPV4V6" {
		return false
	}
	if v.Auth != "NONE" && v.Auth != "PAP" && v.Auth != "CHAP" && v.Auth != "PAP_OR_CHAP" {
		return false
	}
	for _, text := range []string{v.Username, v.Password} {
		if len(text) > 128 || strings.ContainsAny(text, "\"\\") {
			return false
		}
		for _, c := range text {
			if c < 32 || c > 126 {
				return false
			}
		}
	}
	return v.Auth != "NONE" || v.Username == "" && v.Password == ""
}
func parseAPNContexts(lines []string) ([]APNContext, error) {
	result := []APNContext{}
	seen := map[int]bool{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CGDCONT:") {
			continue
		}
		f := fields(line)
		if len(f) < 3 {
			return nil, errors.New("APN_READ_FAILED")
		}
		cid := integer(f[0])
		if cid == nil || *cid < 1 || *cid > 16 || seen[*cid] || len(f[1]) > 32 || len(f[2]) > 253 {
			return nil, errors.New("APN_READ_FAILED")
		}
		seen[*cid] = true
		result = append(result, APNContext{*cid, f[2], f[1]})
	}
	return result, nil
}

// APN reads or prepares CID 1, like the reference PrepareRegistration path.
// It never attaches data, activates PDP, changes IMS/SOS contexts or reboots.
func (s *System) APN(ctx context.Context, c Candidate, identity, iccid string, config *APNConfig) ([]APNContext, error) {
	if !decimal(iccid, 18, 20) || config != nil && !ValidAPNConfig(*config) {
		return nil, errors.New("APN_INVALID")
	}
	found, err := s.Discover(ctx)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, current := range found {
		if current.Key == c.Key && current.Generation == c.Generation {
			c, valid = current, true
			break
		}
	}
	if !valid {
		return nil, errors.New("DEVICE_CHANGED")
	}
	session, err := openWiFiSession(ctx, c, identity)
	if err != nil {
		return nil, err
	}
	defer session.port.Close()
	at := &vocatAT{session: session, device: c.Key, iccid: iccid}
	if err = at.verify(ctx); err != nil {
		return nil, err
	}
	read := func() ([]APNContext, error) {
		lines, e := session.exchange(ctx, "AT+CGDCONT?", 3*time.Second)
		if e != nil {
			return nil, errors.New("APN_READ_FAILED")
		}
		return parseAPNContexts(lines)
	}
	current, err := read()
	if err != nil {
		return nil, err
	}
	if config == nil {
		return current, at.verify(ctx)
	}
	if err = wifiHostData(c.Network); err != nil {
		return nil, err
	}
	for _, row := range current {
		if row.CID == 1 && (strings.EqualFold(row.APN, "ims") || strings.EqualFold(row.APN, "sos")) {
			return nil, errors.New("APN_SYSTEM_CONTEXT")
		}
	}
	lines, err := session.exchange(ctx, "AT+CGACT?", 3*time.Second)
	if err != nil {
		return nil, errors.New("WIFI_DATA_STATE_UNKNOWN")
	}
	active, err := wifiActiveData(lines)
	if err != nil {
		return nil, err
	}
	if len(active) != 0 {
		return nil, errors.New("WIFI_DATA_ACTIVE")
	}
	if err = at.verify(ctx); err != nil {
		return nil, err
	}
	return writeAPNConfig(ctx, session, iccid, *config)
}
func writeAPNConfig(ctx context.Context, s *atSession, iccid string, config APNConfig) ([]APNContext, error) {
	if !ValidAPNConfig(config) {
		return nil, errors.New("APN_INVALID")
	}
	// Never report success after a partial write. Do not replay or roll back onto
	// a potentially changed card; the operator must reread the actual settings.
	commands := []string{
		fmt.Sprintf(`AT+CGDCONT=1,"%s","%s"`, config.Protocol, config.APN),
		fmt.Sprintf(`AT+CGAUTH=1,%d,"%s","%s"`, map[string]int{"NONE": 0, "PAP": 1, "CHAP": 2, "PAP_OR_CHAP": 3}[config.Auth], config.Username, config.Password),
	}
	for _, command := range commands {
		at := &vocatAT{session: s, device: "apn", iccid: iccid}
		if at.verify(ctx) != nil {
			return nil, errors.New("APN_APPLY_UNCONFIRMED")
		}
		if _, err := s.exchange(ctx, command, 3*time.Second); err != nil {
			return nil, errors.New("APN_APPLY_UNCONFIRMED")
		}
	}
	lines, err := s.exchange(ctx, "AT+CGDCONT?", 3*time.Second)
	if err != nil {
		return nil, errors.New("APN_APPLY_UNCONFIRMED")
	}
	result, err := parseAPNContexts(lines)
	if err != nil {
		return nil, errors.New("APN_APPLY_UNCONFIRMED")
	}
	verified := false
	for _, row := range result {
		if row.CID == 1 && row.APN == config.APN && row.Protocol == config.Protocol {
			verified = true
		}
	}
	at := &vocatAT{session: s, device: "apn", iccid: iccid}
	if !verified || at.verify(ctx) != nil {
		return nil, errors.New("APN_APPLY_UNCONFIRMED")
	}
	return result, nil
}
