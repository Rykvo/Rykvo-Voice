package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type hostnameStatus struct {
	Hostname string `json:"hostname"`
	Editable bool   `json:"editable"`
}

var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
var hostnameLetter = regexp.MustCompile(`[A-Za-z]`)

func validHostname(value string) bool {
	return hostnamePattern.MatchString(value) && hostnameLetter.MatchString(value) && !strings.EqualFold(value, "localhost")
}

func hostnameCall(ctx context.Context, request map[string]string) (hostnameStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var result struct {
		Data  hostnameStatus `json:"data"`
		Error string         `json:"error"`
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", "/run/rykvo-voice-host.sock")
	if err != nil {
		return result.Data, errors.New("HOSTNAME_UNAVAILABLE")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(request); err == nil {
		err = json.NewDecoder(io.LimitReader(conn, 4097)).Decode(&result)
	}
	if err != nil {
		return result.Data, errors.New("HOSTNAME_UNAVAILABLE")
	}
	if result.Error != "" {
		return result.Data, errors.New(result.Error)
	}
	if result.Data.Hostname == "" || len(result.Data.Hostname) > 253 {
		return result.Data, errors.New("HOSTNAME_UNAVAILABLE")
	}
	return result.Data, nil
}

func (s *server) hostnameAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	input := map[string]string{"action": "get"}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var body struct {
			Hostname string `json:"hostname"`
			Expected string `json:"expected"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		body.Hostname = strings.TrimSpace(body.Hostname)
		if !validHostname(body.Hostname) || body.Expected == "" || len(body.Expected) > 253 {
			fail(w, 400, "INVALID_HOSTNAME")
			return
		}
		input = map[string]string{"action": "set", "hostname": body.Hostname, "expected": body.Expected}
	default:
		w.Header().Set("Allow", "GET, PUT")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	call := s.hostnameCall
	if call == nil {
		call = hostnameCall
	}
	data, err := call(ctx, input)
	if err != nil {
		status, code := http.StatusServiceUnavailable, err.Error()
		switch code {
		case "HOSTNAME_BUSY", "HOSTNAME_CONFLICT":
			status = http.StatusConflict
		case "HOST_NETWORK_UNSUPPORTED", "HOSTNAME_ROLLBACK_FAILED":
		default:
			code = "HOSTNAME_UNAVAILABLE"
		}
		fail(w, status, code)
		return
	}
	reply(w, 200, map[string]any{"data": data})
}
