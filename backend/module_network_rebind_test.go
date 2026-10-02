package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/uplink"
)

type networkRebindFake struct {
	started           chan string
	cleaning, release chan struct{}
	active            atomic.Int32
	overlap           atomic.Bool
	uncertain         bool
}

func (*networkRebindFake) FixedNetworks() {}
func (f *networkRebindFake) WiFi(ctx context.Context, _ hardware.Candidate, _, _ string, emit func(string)) error {
	if f.active.Add(1) != 1 {
		f.overlap.Store(true)
	}
	defer f.active.Add(-1)
	emit("connected")
	f.started <- uplink.Network(ctx)
	<-ctx.Done()
	f.cleaning <- struct{}{}
	<-f.release
	if f.uncertain {
		return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
	}
	return ctx.Err()
}

func TestModuleNetworkRebindLatestChoice(t *testing.T) {
	for _, final := range []string{"", strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		t.Run("target-"+final, func(t *testing.T) {
			f := &networkRebindFake{started: make(chan string, 4), cleaning: make(chan struct{}, 4), release: make(chan struct{})}
			m := newModuleManagerWithWiFi(nil, nil, f)
			ctx, cancel := context.WithCancel(context.Background())
			var once sync.Once
			release := func() { once.Do(func() { close(f.release) }) }
			defer func() { cancel(); release(); m.operations.Wait() }()
			m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
			sample := wifiModuleFixture()
			m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
			a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
			m.networkLinks[a], m.networkLinks[b] = "a-ready", "b-ready"
			m.networks[1] = a
			m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
			m.mu.Lock()
			m.startWiFiLocked(1, sample)
			m.mu.Unlock()
			select {
			case got := <-f.started:
				if got != a {
					t.Fatal(got)
				}
			case <-time.After(time.Second):
				t.Fatal("first worker not started")
			}
			m.mu.Lock()
			m.applyNetworkLocked(1, b)
			m.mu.Unlock()
			select {
			case <-f.cleaning:
			case <-time.After(time.Second):
				t.Fatal("cleanup not started")
			}
			m.mu.Lock()
			busy := m.networkBusyLocked(moduleRecord{ID: 1, Endpoint: sample.Candidate.Key})
			for _, target := range []string{a, "", b, "", final} {
				m.applyNetworkLocked(1, target)
				m.resumeNetworkLocked(1)
			}
			m.mu.Unlock()
			if busy {
				t.Fatal("cleanup prevented updating the desired route")
			}
			if done := m.reserveModuleWork(sample); done != nil {
				done()
				t.Fatal("business started during cleanup")
			}
			select {
			case <-f.started:
				t.Fatal("new worker started before cleanup completed")
			default:
			}
			release()
			select {
			case got := <-f.started:
				if got != final {
					t.Fatalf("used intermediate route %q instead of %q", got, final)
				}
			case <-time.After(time.Second):
				t.Fatal("last choice did not start promptly")
			}
			if f.overlap.Load() {
				t.Fatal("old and new hardware workers overlapped")
			}
			if done := m.reserveModuleWork(sample); done == nil {
				t.Fatal("business remained blocked after reconnect")
			} else {
				done()
			}
		})
	}
}

func TestModuleNetworkRebindUncertainCleanup(t *testing.T) {
	f := &networkRebindFake{started: make(chan string, 4), cleaning: make(chan struct{}, 4), release: make(chan struct{}), uncertain: true}
	close(f.release)
	m := newModuleManagerWithWiFi(nil, nil, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx, m.ready, m.lastScan = ctx, true, time.Now()
	sample := wifiModuleFixture()
	m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
	m.wifi[1] = &moduleWiFi{ICCID: sample.Reading.ICCID, Enabled: true, State: "waiting"}
	m.mu.Lock()
	m.startWiFiLocked(1, sample)
	m.mu.Unlock()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	m.mu.Lock()
	m.applyNetworkLocked(1, "")
	m.mu.Unlock()
	m.operations.Wait()
	select {
	case <-f.started:
		t.Fatal("unconfirmed cleanup started another worker")
	default:
	}
	if m.wifiView(1, sample.Reading.ICCID)["issue"] != "WIFI_RADIO_RESTORE_UNCONFIRMED" {
		t.Fatal("cleanup error hidden")
	}
	m.mu.Lock()
	busy := m.networkBusyLocked(moduleRecord{ID: 1, Endpoint: sample.Candidate.Key})
	m.applyNetworkLocked(1, "")
	m.resumeNetworkLocked(1)
	m.mu.Unlock()
	if busy {
		t.Fatal("recovery blocked saving desired routing")
	}
	if done := m.reserveModuleWork(sample); done != nil {
		done()
		t.Fatal("uncertain cleanup admitted business")
	}
	if m.wifiView(1, sample.Reading.ICCID)["issue"] != "WIFI_RADIO_RESTORE_UNCONFIRMED" {
		t.Fatal("saving routing hid cleanup uncertainty")
	}
	select {
	case <-f.started:
		t.Fatal("saving a route bypassed hardware quarantine")
	default:
	}
}
