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
	"sync"
	"time"
)

// Network contract only. A future voice gateway consumes these addresses, not keys.
type sipNetworkBinding struct {
	Interface     string `json:"interface"`
	BindAddress   string `json:"bindAddress"`
	PublicAddress string `json:"publicAddress"`
	Server        string `json:"server"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
}
type sipNetworkStatus struct {
	State         string             `json:"state"`
	Configured    bool               `json:"configured"`
	Enabled       bool               `json:"enabled"`
	Address       string             `json:"address"`
	Issue         string             `json:"issue"`
	LastHandshake int64              `json:"lastHandshake,omitempty"`
	Network       *sipNetworkBinding `json:"network,omitempty"`
	Capabilities  map[string]bool    `json:"capabilities"`
}
type sipNetworkManager struct {
	mu    sync.Mutex
	busy  bool
	issue string
	call  func(context.Context, map[string]string) (sipNetworkStatus, error)
}

func sipNetworkCall(ctx context.Context, input map[string]string) (sipNetworkStatus, error) {
	var status sipNetworkStatus
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", "/run/rykvo-sip-network.sock")
	if err != nil {
		return status, errors.New("SIP_HELPER_UNAVAILABLE")
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(input); err != nil {
		return status, errors.New("SIP_NETWORK_FAILED")
	}
	var result struct {
		Data  sipNetworkStatus `json:"data"`
		Error string           `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(conn, 16385)).Decode(&result); err != nil {
		return status, errors.New("SIP_NETWORK_FAILED")
	}
	if result.Error != "" {
		if !regexp.MustCompile(`^SIP_[A-Z_]{1,40}$`).MatchString(result.Error) {
			result.Error = "SIP_NETWORK_FAILED"
		}
		return status, errors.New(result.Error)
	}
	return result.Data, nil
}

func (m *sipNetworkManager) status(ctx context.Context) (sipNetworkStatus, error) {
	m.mu.Lock()
	busy, issue := m.busy, m.issue
	m.mu.Unlock()
	if busy {
		return sipNetworkStatus{State: "configuring", Capabilities: map[string]bool{"network": true, "calls": false}}, nil
	}
	status, err := m.call(ctx, map[string]string{"action": "status"})
	if err == nil && issue != "" {
		status.Issue = issue
	}
	return status, err
}

func (m *sipNetworkManager) start(input map[string]string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy {
		return false
	}
	m.busy, m.issue = true, ""
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
		defer cancel()
		_, err := m.call(ctx, input)
		delete(input, "accessCode")
		m.mu.Lock()
		defer m.mu.Unlock()
		m.busy = false
		if err != nil {
			m.issue = err.Error()
		}
	}()
	return true
}

func (s *server) sipNetworkAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if s.sipNetwork == nil {
		fail(w, 503, "SIP_HELPER_UNAVAILABLE")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/settings/sip-server")
	if path == "" && r.Method == http.MethodGet {
		status, err := s.sipNetwork.status(ctx)
		if err != nil {
			fail(w, 503, err.Error())
			return
		}
		reply(w, 200, map[string]any{"data": status})
		return
	}
	if path != "/connect" && path != "/logout" {
		fail(w, 404, "NOT_FOUND")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var input struct {
		Address    string `json:"address"`
		AccessCode string `json:"accessCode"`
	}
	if !decodeBody(w, r, &input) {
		return
	}
	request := map[string]string{"action": strings.TrimPrefix(path, "/")}
	if path == "/connect" {
		if len(input.Address) > 512 || !strings.HasPrefix(input.Address, "https://") || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(input.AccessCode) {
			fail(w, 400, "SIP_INVALID_INPUT")
			return
		}
		request["address"], request["accessCode"] = input.Address, input.AccessCode
	} else if input.Address != "" || input.AccessCode != "" {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	if !s.sipNetwork.start(request) {
		fail(w, 409, "SIP_BUSY")
		return
	}
	reply(w, 202, map[string]any{"data": map[string]string{"state": "configuring"}})
}
