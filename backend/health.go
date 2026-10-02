package main

import (
	"context"
	"net/http"
	"time"
)

func (s *server) healthAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	call, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	database := s.db != nil && s.db.Ping(call) == nil
	state := messageRuntimeState{Issue: "MESSAGE_WORKER_STOPPED"}
	modules := false
	if s.modules != nil {
		state = s.modules.messageRuntime.snapshot()
		if state.CheckedAt.IsZero() || time.Since(state.CheckedAt) > 15*time.Second {
			state.Ready = false
			state.Issue = "MESSAGE_WORKER_STALE"
		}
		s.modules.mu.RLock()
		modules = s.modules.ready && time.Since(s.modules.lastScan) < 20*time.Second
		s.modules.mu.RUnlock()
	}
	webhooks := s.webhookStatus.snapshot(15 * time.Second)
	retention := s.retentionStatus.snapshot(2 * time.Minute)
	var sipState workerState
	if s.sipGateway != nil {
		sipState = s.sipGateway.status.snapshot(15 * time.Second)
	}
	maintenance := s.maintenanceState()
	ready := !maintenance.Draining && sipState.Ready && database && modules && state.Ready && webhooks.Ready && retention.Ready
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	reply(w, code, map[string]any{"data": map[string]any{"ready": ready, "database": database, "modules": modules, "messages": state, "webhooks": webhooks, "retention": retention, "sip": sipState, "version": buildVersion, "maintenance": maintenance}})
}
