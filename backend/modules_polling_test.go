package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

func TestModuleSkippedPollDoesNotWaitAFullInterval(t *testing.T) {
	start := time.Unix(1000, 0)
	next := start
	last := start
	accepted := 0
	for tick := 0; tick < 180; tick++ {
		now := start.Add(time.Duration(tick) * time.Second)
		if !now.Before(next) {
			// A two-second receive loop briefly holds the same module gate.
			busy := tick%2 == 0
			if !busy {
				last = now
				accepted++
			}
			// Results are handled just after the scheduler tick.
			next = now.Add(time.Millisecond + modulePollDelay(!busy))
		}
		if now.Sub(last) > 90*time.Second {
			t.Fatal("healthy receiver starved state refresh", tick, accepted)
		}
	}
	if accepted < 5 || modulePollDelay(true) != 25*time.Second {
		t.Fatal("poll interval changed or refresh did not resume", accepted)
	}
}

func TestModulePollAcceptanceContentionPreservesLastReading(t *testing.T) {
	m := newModuleManager(nil, nil)
	sample := wifiModuleFixture()
	m.seen[sample.Candidate.Key] = sample.Candidate
	m.values[1] = sample
	gate := m.gate(sample.Candidate.Key)
	gate <- struct{}{}
	next := sample
	next.Reading.UpdatedAt = next.Reading.UpdatedAt.Add(time.Second)
	if m.accept(context.Background(), next) {
		t.Fatal("busy acceptance reported a saved reading")
	}
	<-gate
	next.Reading.Issue = "OPERATION_ACTIVE"
	if m.accept(context.Background(), next) {
		t.Fatal("skipped read reported a saved reading")
	}
	if !m.values[1].Reading.UpdatedAt.Equal(sample.Reading.UpdatedAt) {
		t.Fatal("skipped poll refreshed hardware freshness")
	}
}

func TestPollingFairness(t *testing.T) {
	now := time.Now()
	seen := map[string]hardware.Candidate{}
	due := map[string]time.Time{}
	pending := map[string]bool{}
	for i := 0; i < 170; i++ {
		key := fmt.Sprintf("fixture-%03d", i)
		seen[key] = hardware.Candidate{Key: key}
	}
	pending["fixture-000"] = true // One stuck device never becomes another queued worker.
	visited := map[string]bool{}
	for pass := 0; pass < 30; pass++ {
		ready := pollingCandidates(seen, pending, due, now)
		for i, c := range ready {
			if i == moduleReadWorkers {
				break
			}
			if visited[c.Key] {
				t.Fatal("starved unsampled module", c.Key)
			}
			visited[c.Key] = true
			due[c.Key] = now.Add(time.Minute)
		}
	}
	if len(visited) != 169 || visited["fixture-000"] {
		t.Fatal("incomplete fair scheduling", len(visited))
	}
	due["fixture-090"] = now.Add(-2 * time.Minute)
	due["fixture-001"] = now.Add(-time.Minute)
	ready := pollingCandidates(seen, pending, due, now)
	if len(ready) != 2 || ready[0].Key != "fixture-090" {
		t.Fatal("discovery ordering defeated overdue sampling", ready)
	}
	delete(pending, "fixture-000")
	if pollingCandidates(seen, pending, due, now)[0].Key != "fixture-000" {
		t.Fatal("first-time device not sampled")
	}
}
