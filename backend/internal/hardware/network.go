package hardware

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Operator struct {
	Name       string `json:"name"`
	PLMN       string `json:"plmn"`
	Technology *int   `json:"technology"`
	Status     int    `json:"status"`
}

type NetworkRequest struct {
	IMEI       string `json:"imei"`
	ICCID      string `json:"iccid"`
	Automatic  bool   `json:"automatic"`
	PLMN       string `json:"plmn"`
	Technology *int   `json:"technology"`
}

func (r NetworkRequest) Valid(scan bool) bool {
	if !decimal(r.IMEI, 14, 17) || !decimal(r.ICCID, 18, 22) {
		return false
	}
	if scan || r.Automatic {
		return r.PLMN == "" && r.Technology == nil
	}
	return decimal(r.PLMN, 5, 6) && (r.Technology == nil || (*r.Technology >= 0 && *r.Technology <= 13))
}

func (r NetworkRequest) Confirmed(v Reading) bool {
	if !v.Responsive || v.Issue != "" || v.IMEI != r.IMEI || v.ICCID != r.ICCID || v.NetworkMode == nil {
		return false
	}
	if r.Automatic {
		return *v.NetworkMode == 0
	}
	return *v.NetworkMode == 1 && v.PLMN == r.PLMN && (r.Technology == nil || (v.AccessTechnology != nil && *v.AccessTechnology == *r.Technology))
}

func readOperator(lines []string, r *Reading) {
	for _, line := range lines {
		if !strings.HasPrefix(line, "+COPS:") {
			continue
		}
		v := fields(line)
		if len(v) > 0 {
			r.NetworkMode = integer(v[0])
		}
		if len(v) > 2 {
			if v[1] == "2" {
				r.PLMN = v[2]
			} else {
				r.Operator = v[2]
			}
		}
		if len(v) > 3 {
			r.AccessTechnology = integer(v[3])
			r.Technology = map[string]string{"0": "GSM", "2": "UMTS", "7": "LTE", "9": "NB-IoT", "11": "5G", "12": "5G"}[v[3]]
		}
	}
}

func parseOperators(lines []string) ([]Operator, error) {
	result := []Operator{}
	seen := map[string]bool{}
	found := false
	for _, line := range lines {
		if !strings.HasPrefix(line, "+COPS:") {
			continue
		}
		found = true
		start, quoted := -1, false
		for i, ch := range line {
			if ch == '"' {
				quoted = !quoted
			}
			if quoted {
				continue
			}
			if ch == '(' {
				start = i + 1
			}
			if ch != ')' || start < 0 {
				continue
			}
			v := fields("+COPS:" + line[start:i])
			start = -1
			if len(v) < 4 || len(v) > 5 || !decimal(v[3], 5, 6) {
				continue
			}
			status, err := strconv.Atoi(v[0])
			if err != nil || status < 0 || status > 3 {
				continue
			}
			op := Operator{Name: v[1], PLMN: v[3], Status: status}
			if op.Name == "" {
				op.Name = v[2]
			}
			if op.Name == "" {
				op.Name = op.PLMN
			}
			key := op.PLMN
			if len(v) == 5 {
				op.Technology = integer(v[4])
				if op.Technology == nil || *op.Technology < 0 || *op.Technology > 13 {
					continue
				}
				key += ":" + v[4]
			}
			if !seen[key] && len(result) < 256 {
				result = append(result, op)
				seen[key] = true
			}
		}
	}
	if !found {
		return nil, errors.New("INVALID_RESPONSE")
	}
	return result, nil
}

// Only the selected modem is touched; no detach, reset or default-route changes.
func NetworkCall(ctx context.Context, c Candidate, r NetworkRequest, scan bool) ([]Operator, bool, error) {
	if !r.Valid(scan) {
		return nil, false, errors.New("INVALID_NETWORK_REQUEST")
	}
	s, err := openATSession(ctx, c, r.IMEI)
	if err != nil {
		return nil, false, err
	}
	defer s.port.Close()
	return networkSession(ctx, s, r, scan)
}

func networkSession(ctx context.Context, s *atSession, r NetworkRequest, scan bool) ([]Operator, bool, error) {
	check := func() error {
		lines, err := s.query(ctx, "AT+CGSN")
		if err != nil {
			return err
		}
		if digits(lines, 14, 17) != r.IMEI {
			return errors.New("DEVICE_CHANGED")
		}
		lines, err = s.query(ctx, "AT+CCID")
		if err != nil && err.Error() != "COMMAND_UNSUPPORTED" {
			return err
		}
		if digits(lines, 18, 22) == "" {
			lines, err = s.query(ctx, "AT+QCCID")
		}
		if err != nil {
			return err
		}
		if digits(lines, 18, 22) != r.ICCID {
			return errors.New("DEVICE_CHANGED")
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, false, err
	}
	command := "AT+COPS=?"
	if !scan {
		command = "AT+COPS=0"
		if !r.Automatic {
			command = fmt.Sprintf("AT+COPS=1,2,\"%s\"", r.PLMN)
			if r.Technology != nil {
				command += fmt.Sprintf(",%d", *r.Technology)
			}
		}
	}
	lines, err := s.exchange(ctx, command, 180*time.Second)
	if err != nil {
		return nil, !scan, err
	}
	if err = check(); err != nil {
		return nil, !scan, err
	}
	if scan {
		ops, err := parseOperators(lines)
		return ops, false, err
	}
	return nil, true, nil
}
