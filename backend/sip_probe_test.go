package main

import (
	"context"
	"fmt"
	"rykvo.local/auth/internal/hardware"
	"sync/atomic"
	"testing"
	"time"
)

type probeFixture struct {
	testWiFiVoiceAdapter
	started         chan string
	active, maximum atomic.Int32
}

func (f *probeFixture) WiFiCalls(ctx context.Context, c hardware.Candidate, card, request string) ([]hardware.VoiceCall, error) {
	active := f.active.Add(1)
	defer f.active.Add(-1)
	for old := f.maximum.Load(); active > old; old = f.maximum.Load() {
		if f.maximum.CompareAndSwap(old, active) {
			break
		}
	}
	f.started <- c.Key
	if c.Key == "module-01" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	return nil, nil
}
func (*probeFixture) OpenWiFiIncoming(context.Context, hardware.Candidate, string, string) (*hardware.WiFiCall, error) {
	return nil, fmt.Errorf("fixture has no calls")
}

func TestSIPIncomingProbesDoNotWaitForSlowModuleOrPeriodicRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &probeFixture{testWiFiVoiceAdapter: testWiFiVoiceAdapter{ready: true}, started: make(chan string, 100)}
	m := newModuleManagerWithWiFi(nil, nil, f)
	m.ready, m.ctx, m.lastScan = true, ctx, time.Now()
	for i := int64(1); i <= 70; i++ {
		sample := wifiModuleFixture()
		sample.Candidate.Key = moduleID(i)
		m.values[i], m.seen[sample.Candidate.Key] = sample, sample.Candidate
		m.wifi[i] = &moduleWiFi{Enabled: true, running: true, Registered: true, State: "connected", RadioOff: true, ICCID: sample.Reading.ICCID, candidate: sample.Candidate}
	}
	g := newSIPGateway(&server{modules: m})
	g.calls.ctx = ctx
	defer func() { cancel(); g.calls.wait.Wait() }()
	scan := func() {
		g.server.sipAccountsMu.Lock()
		defer g.server.sipAccountsMu.Unlock()
		g.calls.scanIncomingLocked(sipAccountNetwork{})
	}
	scan()
	seen := map[string]bool{}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for len(seen) < 70 {
		select {
		case id := <-f.started:
			if seen[id] {
				t.Fatal("one module monopolized incoming polling", id)
			}
			seen[id] = true
		case <-g.calls.probeWake:
			scan()
		case <-deadline.C:
			t.Fatalf("only %d modules checked before periodic refresh", len(seen))
		}
	}
	if f.maximum.Load() > 4 || !seen["module-01"] {
		t.Fatal("probe limit ignored", f.maximum.Load())
	}
	cancel()
	g.calls.wait.Wait()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.work) != 0 || len(g.calls.probeSlots) != 0 {
		t.Fatal("probe resources leaked")
	}
}
