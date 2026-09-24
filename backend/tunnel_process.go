package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func privateFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
func (t *tunnelManager) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.Env = []string{"HOME=" + t.dir, "PATH=/usr/bin:/bin", "TUNNEL_ORIGIN_CERT=" + t.certificatePath()}
	cmd.Dir = t.dir
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	return cmd
}
func (t *tunnelManager) authorize(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	cmd := t.command(ctx, "tunnel", "login")
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			for _, word := range strings.Fields(scanner.Text()) {
				value := authorizationURL(strings.Trim(word, "\"'"))
				if value == "" {
					continue
				}
				t.mu.Lock()
				if t.binding.Status != "disconnecting" {
					t.authURL = value
					t.binding.Status = "authorizing"
				}
				t.mu.Unlock()
			}
		}
		reader.Close()
	}()
	err := cmd.Run()
	writer.Close()
	<-scanDone
	if ctx.Err() != nil {
		return tunnelError("AUTH_EXPIRED")
	}
	if err != nil {
		return tunnelError("AUTH_FAILED")
	}
	if _, err = readCertificate(t.certificatePath()); err != nil {
		return err
	}
	return nil
}
func (t *tunnelManager) runConnector(ctx context.Context) error {
	client := &http.Client{Timeout: 2 * time.Second}
	delay := time.Second
	for ctx.Err() == nil {
		cmd := t.command(ctx, "tunnel", "--config", filepath.Join(t.dir, "config.json"), "--no-autoupdate", "run", t.read().TunnelID)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return tunnelError("TUNNEL_START_FAILED")
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		ticker := time.NewTicker(3 * time.Second)
		alive := true
		for alive {
			select {
			case <-ctx.Done():
				ticker.Stop()
				<-exited
				return ctx.Err()
			case <-exited:
				alive = false
			case <-ticker.C:
				ready := connectorReady(ctx, client)
				t.mu.Lock()
				if t.binding.Status != "disconnecting" {
					if ready {
						t.binding.Status = "connected"
						delay = time.Second
					} else {
						t.binding.Status = "connecting"
					}
				}
				t.mu.Unlock()
			}
		}
		ticker.Stop()
		t.mu.Lock()
		if t.binding.Status != "disconnecting" {
			t.binding.Status = "connecting"
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
	return ctx.Err()
}
func connectorReady(ctx context.Context, client *http.Client) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:20241/ready", nil)
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false
	}
	// /ready 检查 Cloudflare 连接；源站检查单独验证应用可服务。
	req, _ = http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:80/", nil)
	origin, err := client.Do(req)
	if err != nil {
		return false
	}
	defer origin.Body.Close()
	return origin.StatusCode == 200
}
