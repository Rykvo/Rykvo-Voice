package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

type cleanupError struct {
	stage string
	cause error
}

func (e *cleanupError) Error() string { return "CLEANUP_FAILED" }
func (e *cleanupError) Unwrap() error { return e.cause }

func cleanupDetails(err error) (string, string) {
	stage, code := "cleanup", "CLEANUP_FAILED"
	var step *cleanupError
	if errors.As(err, &step) {
		stage = step.stage
	}
	var known tunnelError
	switch {
	case errors.As(err, &known):
		code = string(known)
	case errors.Is(err, context.DeadlineExceeded):
		code = "CLEANUP_TIMEOUT"
	case errors.Is(err, context.Canceled):
		code = "CLEANUP_INTERRUPTED"
	case errors.Is(err, os.ErrPermission):
		code = "FILE_PERMISSION"
	}
	return stage, code
}

func logCleanup(stage string, attempt int, err error) {
	_, code := cleanupDetails(err)
	status, providerCode := 0, 0
	var api *cfRequestError
	if errors.As(err, &api) {
		status, providerCode = api.status, api.providerCode
	}
	// 只记录步骤和错误码，不记录凭据、请求头或云端原始响应。
	log.Printf("tunnel_cleanup stage=%s attempt=%d code=%s http=%d provider=%d", stage, attempt, code, status, providerCode)
}

func waitCleanup(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func cleanupStep(ctx context.Context, stage string, wait func(context.Context, time.Duration) error, run func() error) error {
	if wait == nil {
		wait = waitCleanup
	}
	for attempt := 1; attempt <= 5; attempt++ {
		if err := ctx.Err(); err != nil {
			return &cleanupError{stage, err}
		}
		err := run()
		if err == nil {
			return nil
		}
		logCleanup(stage, attempt, err)
		retry := errors.Is(err, tunnelError("DATABASE_UNAVAILABLE"))
		delay := 2 * time.Second << (attempt - 1)
		var api *cfRequestError
		if errors.As(err, &api) {
			retry = api.code == "CLOUDFLARE_UNAVAILABLE" || api.status == 429 || api.status == 409 ||
				(stage == "delete_tunnel" && api.status == 400 && api.providerCode == 1022)
			delay = max(delay, api.retryAfter)
		}
		if !retry || attempt == 5 {
			return &cleanupError{stage, err}
		}
		if err = wait(ctx, delay); err != nil {
			return &cleanupError{stage, err}
		}
	}
	return nil
}

func (t *tunnelManager) disconnect(owner string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.binding.Owner != "" && t.binding.Owner != owner {
		return tunnelError("TUNNEL_OWNER_REQUIRED")
	}
	if t.ctx.Err() != nil {
		return t.ctx.Err()
	}
	if t.binding.Status == "disconnected" || t.binding.Status == "disconnecting" {
		return nil
	}
	previous := t.binding
	t.binding.Status, t.binding.ErrorCode = "disconnecting", ""
	t.binding.CleanupStage, t.binding.CleanupCode = "", ""
	t.binding.Resume, t.binding.Removing = false, true
	if err := t.saveLocked(); err != nil {
		t.binding = previous
		logCleanup("save_intent", 1, tunnelError("DATABASE_UNAVAILABLE"))
		return err
	}
	t.authURL = ""
	t.startCleanupLocked()
	return nil
}

func (t *tunnelManager) startCleanupLocked() {
	if t.cancel != nil {
		t.cancel()
	}
	done := t.done
	t.workers.Add(1)
	go func() {
		defer t.workers.Done()
		ctx, cancel := context.WithTimeout(t.ctx, 2*time.Minute)
		defer cancel()
		log.Print("tunnel_cleanup stage=start")
		var err error
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				err = &cleanupError{"stop_connector", ctx.Err()}
			}
		}
		if err == nil {
			err = t.cleanupFlow(ctx)
		}
		if err == nil {
			log.Print("tunnel_cleanup stage=complete")
			return
		}
		if t.ctx.Err() != nil {
			return
		} // 下次启动接着清理，不恢复连接。
		stage, code := cleanupDetails(err)
		logCleanup(stage, 0, err)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.binding.Status, t.binding.ErrorCode = "failed", "DISCONNECT_FAILED"
		t.binding.CleanupStage, t.binding.CleanupCode = stage, code
		if t.saveLocked() != nil {
			logCleanup("save_failure", 1, tunnelError("DATABASE_UNAVAILABLE"))
		}
	}()
}

func (t *tunnelManager) cleanupFlow(ctx context.Context) error {
	b := t.read()
	if !b.RemoteCleared {
		if b.Provisioned {
			cert, err := readCertificate(t.certificatePath())
			if err != nil {
				return &cleanupError{"read_credentials", err}
			}
			if err = t.client(cert).removeOwned(ctx, b); err != nil {
				return err
			}
		}
		if err := t.cleanupSave(ctx, "save_remote_cleanup", func(b *tunnelBinding) { b.RemoteCleared = true }); err != nil {
			return err
		}
	}
	if err := cleanupStep(ctx, "remove_credentials", nil, func() error {
		for _, path := range []string{t.certificatePath(), filepath.Join(t.dir, "secret"), filepath.Join(t.dir, "credentials.json"), filepath.Join(t.dir, "config.json")} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return t.cleanupSave(ctx, "save_complete", func(b *tunnelBinding) { *b = tunnelBinding{Status: "disconnected"} })
}

func (t *tunnelManager) cleanupSave(ctx context.Context, stage string, change func(*tunnelBinding)) error {
	return cleanupStep(ctx, stage, nil, func() error {
		if t.update(change) != nil {
			return tunnelError("DATABASE_UNAVAILABLE")
		}
		return nil
	})
}
