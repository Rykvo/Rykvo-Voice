package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestModuleWorkReservationsIsolateMaintenance(t *testing.T) {
	m := newModuleManager(nil, &restartFake{})
	m.ctx, m.ready, m.lastScan = context.Background(), true, time.Now()
	sample := wifiModuleFixture()
	m.values[1], m.seen[sample.Candidate.Key] = sample, sample.Candidate
	other := sample
	other.Candidate.Key = "independent-module"
	m.values[2], m.seen[other.Candidate.Key] = other, other.Candidate
	v := moduleRecord{ID: 1, Endpoint: sample.Candidate.Key, Identity: sample.Candidate.Identity(sample.Reading)}
	release := m.reserveModuleWork(sample)
	if release == nil {
		t.Fatal("ready work rejected")
	}
	if m.restartableLocked(v) {
		t.Fatal("restart admitted during business work")
	}
	independent := m.reserveModuleWork(other)
	if independent == nil {
		t.Fatal("one module blocked another")
	}
	independent()
	m.ready = false
	if m.reserveModuleWork(other) != nil {
		t.Fatal("maintenance admitted new work")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	if len(m.work) != 0 {
		t.Fatal("reservation leaked")
	}
	m.ready = true
	m.controlPending[sample.Candidate.Key] = true
	if m.reserveModuleWork(sample) != nil {
		t.Fatal("configuration write admitted work")
	}
	delete(m.controlPending, sample.Candidate.Key)
	m.jobs[1] = moduleJob{State: "queued"}
	if m.reserveModuleWork(sample) != nil {
		t.Fatal("eSIM job admitted work")
	}
	delete(m.jobs, 1)
	changed := sample
	changed.Reading.ICCID = "replacement"
	if m.reserveModuleWork(changed) != nil {
		t.Fatal("stale card admitted")
	}
	changed = sample
	changed.Candidate.Generation = "replacement"
	if m.reserveModuleWork(changed) != nil {
		t.Fatal("stale device admitted")
	}
	release = m.reserveModuleWork(sample)
	if release == nil {
		t.Fatal("admission did not recover")
	}
	release()
}
