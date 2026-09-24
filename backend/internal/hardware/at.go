package hardware

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

var errTimeout = errors.New("READ_TIMEOUT")

type atPort interface {
	io.ReadWriteCloser
}

type atSession struct {
	port   atPort
	buffer string
}

// Commands are generated here, never supplied by an HTTP caller.
func (s *atSession) query(ctx context.Context, command string) ([]string, error) {
	return s.exchange(ctx, command, 1500*time.Millisecond)
}

func (s *atSession) exchange(ctx context.Context, command string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data := []byte(command + "\r")
	for len(data) > 0 {
		if ctx.Err() != nil {
			return nil, errTimeout
		}
		n, err := s.port.Write(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
	}
	lines := []string{}
	total := 0
	for {
		if ctx.Err() != nil {
			return nil, errTimeout
		}
		if len(s.buffer) > 32768 {
			return nil, errors.New("INVALID_RESPONSE")
		}
		if cut := strings.IndexAny(s.buffer, "\r\n"); cut >= 0 {
			line := strings.TrimSpace(s.buffer[:cut])
			s.buffer = s.buffer[cut+1:]
			if line == "" || line == command {
				continue
			}
			if line == "OK" {
				return lines, nil
			}
			if line == "ERROR" || strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "+CMS ERROR") {
				return nil, errors.New("COMMAND_UNSUPPORTED")
			}
			if line == "RING" || strings.HasPrefix(line, "+CMT") || strings.HasPrefix(line, "+QIND") {
				continue
			}
			lines = append(lines, line)
			continue
		}
		b := make([]byte, 1024)
		n, err := s.port.Read(b)
		if err != nil {
			return nil, err
		}
		total += n
		if total > 65536 {
			return nil, errors.New("INVALID_RESPONSE")
		}
		s.buffer += string(b[:n])
	}
}

func readAT(ctx context.Context, c Candidate) Reading {
	r := Reading{Model: c.Model, SIM: "unknown", Registration: "unknown", Issue: "AT_PORT_MISSING"}
	for _, port := range c.Ports {
		if ctx.Err() != nil {
			break
		}
		fd, err := openAT(port.Path)
		if err != nil {
			r.Issue = errorCode(err)
			continue
		}
		s := &atSession{port: fd}
		_, err = s.query(ctx, "AT")
		if err != nil {
			fd.Close()
			r.Issue = errorCode(err)
			continue
		}
		r.Responsive = true
		r.Issue = ""
		readATFields(ctx, s, &r)
		fd.Close()
		return r
	}
	return r
}

func readATFields(ctx context.Context, s *atSession, r *Reading) {
	stopped := false
	query := func(cmd string) []string {
		if stopped || ctx.Err() != nil {
			return nil
		}
		v, err := s.query(ctx, cmd)
		if err != nil {
			r.Warnings = append(r.Warnings, cmd+":"+errorCode(err))
			// Close this session after a timeout; late replies must not satisfy a different query.
			if errors.Is(err, errTimeout) || errorCode(err) != "COMMAND_UNSUPPORTED" {
				stopped = true
				r.Issue = errorCode(err)
			}
		}
		return v
	}
	first := func(cmd string) string {
		v := query(cmd)
		if len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	if v := first("AT+CGMM"); v != "" {
		r.Model = v
	}
	r.Firmware = first("AT+CGMR")
	r.IMEI = digits(query("AT+CGSN"), 14, 17)
	r.SIM = strings.TrimSpace(strings.TrimPrefix(first("AT+CPIN?"), "+CPIN:"))
	if r.SIM == "" {
		r.SIM = "unknown"
	}
	if r.SIM == "READY" {
		r.ICCID = digits(query("AT+CCID"), 18, 22)
		if r.ICCID == "" {
			r.ICCID = digits(query("AT+QCCID"), 18, 22)
		}
		for _, line := range query("AT+CNUM") {
			if !strings.HasPrefix(line, "+CNUM:") {
				continue
			}
			v := fields(line)
			if len(v) > 1 && validNumber(v[1]) {
				r.Number = v[1]
				break
			}
		}
	}
	for _, line := range query("AT+CSQ") {
		if strings.HasPrefix(line, "+CSQ:") {
			v := fields(line)
			if len(v) > 0 {
				n, err := strconv.Atoi(v[0])
				if err == nil && n >= 0 && n <= 31 {
					dbm := -113 + n*2
					r.RSSI = &dbm
				}
			}
		}
	}
	readOperator(query("AT+COPS?"), r)
	for _, cmd := range []string{"AT+CEREG?", "AT+CGREG?", "AT+CREG?"} {
		v := query(cmd)
		if len(v) == 0 {
			continue
		}
		f := fields(v[0])
		if len(f) > 1 {
			r.Registration = map[string]string{"0": "not_registered", "1": "home", "2": "searching", "3": "denied", "4": "unknown", "5": "roaming"}[f[1]]
			break
		}
	}
	if strings.Contains(strings.ToUpper(r.Model), "EC") || strings.Contains(strings.ToUpper(r.Model), "EG") {
		for _, line := range query(`AT+QENG="servingcell"`) {
			v := fields(line)
			if len(v) >= 17 && v[0] == "servingcell" && v[2] == "LTE" {
				r.Technology = "LTE"
				r.RSRP = integer(v[13])
				r.RSRQ = integer(v[14])
				r.SINR = integer(v[16])
			}
		}
	}
}

func fields(line string) []string {
	_, value, ok := strings.Cut(line, ":")
	if !ok {
		value = line
	}
	r := csv.NewReader(strings.NewReader(strings.TrimSpace(value)))
	r.TrimLeadingSpace = true
	v, _ := r.Read()
	return v
}
func integer(s string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &n
}
func digits(lines []string, min, max int) string {
	for _, line := range lines {
		if _, v, ok := strings.Cut(line, ":"); ok {
			line = v
		}
		line = strings.Trim(strings.TrimSpace(line), "\"")
		line = strings.TrimRight(line, "Ff")
		if len(line) >= min && len(line) <= max && strings.Trim(line, "0123456789") == "" {
			return line
		}
	}
	return ""
}
func validNumber(s string) bool {
	v := strings.TrimPrefix(s, "+")
	return len(v) >= 5 && len(v) <= 20 && strings.Trim(v, "0123456789") == ""
}
func errorCode(err error) string {
	if errors.Is(err, errTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "READ_TIMEOUT"
	}
	for _, code := range []string{"PERMISSION_DENIED", "DEVICE_BUSY", "COMMAND_UNSUPPORTED", "INVALID_RESPONSE", "PCSC_UNAVAILABLE", "NO_SIM"} {
		if err.Error() == code {
			return code
		}
	}
	return "DEVICE_UNAVAILABLE"
}

func openATSession(ctx context.Context, c Candidate, expectedIMEI string) (*atSession, error) {
	last := errors.New("AT_PORT_MISSING")
	for _, port := range c.Ports {
		fd, err := openAT(port.Path)
		if err != nil {
			last = err
			continue
		}
		s := &atSession{port: fd}
		if _, err = s.query(ctx, "AT"); err != nil {
			fd.Close()
			last = err
			continue
		}
		if expectedIMEI != "" {
			lines, err := s.query(ctx, "AT+CGSN")
			if err != nil || digits(lines, 14, 17) != expectedIMEI {
				fd.Close()
				return nil, errors.New("DEVICE_CHANGED")
			}
		}
		return s, nil
	}
	return nil, last
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
