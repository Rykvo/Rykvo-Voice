package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type maintenanceGate struct {
	mu       sync.Mutex
	draining bool
	requests int
}

func (g *maintenanceGate) enter() func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return nil
	}
	g.requests++
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); g.requests--; g.mu.Unlock() }) }
}

type maintenanceView struct {
	Draining bool `json:"draining"`
	Idle     bool `json:"idle"`
	Requests int  `json:"requests"`
	Work     int  `json:"work"`
	Jobs     int  `json:"jobs"`
	Calls    int  `json:"calls"`
}

func (s *server) setDraining(value bool) {
	s.maintenance.mu.Lock()
	defer s.maintenance.mu.Unlock()
	if s.modules != nil {
		s.modules.mu.Lock()
		s.modules.draining = value
		s.modules.mu.Unlock()
	}
	s.maintenance.draining = value
}

func (m *moduleManager) isDraining() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.draining
}

func (s *server) maintenanceState() maintenanceView {
	s.maintenance.mu.Lock()
	v := maintenanceView{Draining: s.maintenance.draining, Requests: s.maintenance.requests}
	s.maintenance.mu.Unlock()
	if m := s.modules; m != nil {
		m.mu.RLock()
		for _, n := range m.work {
			v.Work += n
		}
		v.Work += len(m.controlPending)
		for _, job := range m.jobs {
			if job.active() {
				v.Jobs++
			}
		}
		for _, until := range m.recoveryUntil {
			if time.Now().Before(until) {
				v.Jobs++
			}
		}
		m.mu.RUnlock()
	}
	// Never wait behind slow account persistence just to report progress.
	if !s.sipAccountsMu.TryLock() {
		v.Calls = 1
	} else {
		if s.sipGateway != nil && s.sipGateway.calls != nil {
			c := s.sipGateway.calls
			v.Calls = len(c.active) + len(c.incoming) + len(c.admitting)
		}
		s.sipAccountsMu.Unlock()
	}
	v.Idle = v.Draining && v.Requests == 0 && v.Work == 0 && v.Jobs == 0 && v.Calls == 0
	return v
}

// A private, mode-0600 Unix socket; never exposed through Nginx or a tunnel.
func (s *server) startControl(ctx context.Context, path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("invalid control socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("control socket in use")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health" && r.Method == "GET":
			s.healthAPI(r.Context(), w, r)
			return
		case r.URL.Path == "/drain" && r.Method == "POST":
			s.setDraining(true)
		case r.URL.Path == "/resume" && r.Method == "POST":
			s.setDraining(false)
		case r.URL.Path == "/status" && r.Method == "GET":
		default:
			http.NotFound(w, r)
			return
		}
		reply(w, 200, s.maintenanceState())
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 3 * time.Second, MaxHeaderBytes: 4096}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	stop := func() { _ = server.Close(); <-done }
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	return stop, nil
}

func maintenanceCommand(path, action string, output io.Writer) error {
	if action != "drain" && action != "resume" && action != "status" && action != "health" {
		return errors.New("maintenance action must be drain, resume, status or health")
	}
	if path == "" {
		path = "/run/rykvo-voice/control.sock"
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request := func(method, action string) (maintenanceView, error) {
		var v maintenanceView
		req, _ := http.NewRequest(method, "http://localhost/"+action, nil)
		response, err := client.Do(req)
		if err != nil {
			return v, err
		}
		defer response.Body.Close()
		if action == "health" {
			_, err = io.Copy(output, io.LimitReader(response.Body, 16384))
		} else {
			err = json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&v)
		}
		if response.StatusCode != 200 {
			return v, fmt.Errorf("maintenance status %d", response.StatusCode)
		}
		return v, err
	}
	method := "GET"
	if action == "drain" || action == "resume" {
		method = "POST"
	}
	v, err := request(method, action)
	if err != nil || action == "health" {
		return err
	}
	deadline := time.Now().Add(180 * time.Second)
	for action == "drain" && !v.Idle {
		if time.Now().After(deadline) {
			return errors.New("active work remains; resume or wait before upgrading")
		}
		time.Sleep(500 * time.Millisecond)
		v, err = request("GET", "status")
		if err != nil {
			return err
		}
		if !v.Draining {
			return errors.New("maintenance cancelled")
		}
	}
	return json.NewEncoder(output).Encode(v)
}
