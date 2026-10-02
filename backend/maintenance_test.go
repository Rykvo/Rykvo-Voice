package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMaintenanceWaitsWithoutCancellingActiveWork(t *testing.T) {
	m := newModuleManager(nil, &restartFake{})
	m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
	sample := wifiModuleFixture()
	m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
	s := &server{modules: m}
	work, request := m.reserveModuleWork(sample), s.maintenance.enter()
	if work == nil || request == nil {
		t.Fatal("admission failed")
	}
	s.setDraining(true)
	v := s.maintenanceState()
	if !v.Draining || v.Idle || v.Requests != 1 || v.Work != 1 {
		t.Fatal(v)
	}
	if m.reserveModuleWork(sample) != nil || s.maintenance.enter() != nil {
		t.Fatal("new work admitted while draining")
	}
	if m.restartableLocked(moduleRecord{ID: 1, Endpoint: sample.Candidate.Key, Identity: sample.Candidate.Identity(sample.Reading)}) {
		t.Fatal("restart admitted")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); work(); request() }()
	}
	wg.Wait()
	if !s.maintenanceState().Idle {
		t.Fatal("completed work remained busy")
	}
	m.jobs[1] = moduleJob{State: "running"}
	if s.maintenanceState().Idle {
		t.Fatal("eSIM task ignored")
	}
	delete(m.jobs, 1)
	s.sipAccountsMu.Lock()
	if s.maintenanceState().Idle {
		t.Fatal("SIP admission ignored")
	}
	s.sipAccountsMu.Unlock()
	s.setDraining(false)
	if v := s.maintenanceState(); v.Draining || v.Idle {
		t.Fatal(v)
	}
	request = s.maintenance.enter()
	if request == nil {
		t.Fatal("resume failed")
	}
	request()
	work = m.reserveModuleWork(sample)
	if work == nil {
		t.Fatal("module resume failed")
	}
	work()
}

func TestMaintenanceRejectsPublicWritesBeforeSideEffects(t *testing.T) {
	s := &server{}
	s.setDraining(true)
	for _, path := range []string{"/api/messages", "/api/v1/messages", "/api/modules/module-1/restart", "/api/session"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, localRequest("POST", path, strings.NewReader("{}")))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "SERVICE_DRAINING") || w.Header().Get("Retry-After") == "" {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestPrivateMaintenanceSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux service socket permissions")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "control.sock")
	s := &server{}
	stop, err := s.startControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if _, err = s.startControl(ctx, path); err == nil {
		t.Fatal("live socket replaced")
	}
	var out bytes.Buffer
	if err = maintenanceCommand(path, "drain", &out); err != nil || !strings.Contains(out.String(), `"idle":true`) {
		t.Fatal(err, out.String())
	}
	out.Reset()
	if err = maintenanceCommand(path, "resume", &out); err != nil || !strings.Contains(out.String(), `"draining":false`) {
		t.Fatal(err, out.String())
	}
	out.Reset()
	if err = maintenanceCommand(path, "status", &out); err != nil {
		t.Fatal(err)
	}
	if err = maintenanceCommand(path, "unknown", &out); err == nil {
		t.Fatal("unknown action accepted")
	}
}
