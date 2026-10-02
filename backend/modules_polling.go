package main

import (
	"sort"
	"time"

	"rykvo.local/auth/internal/hardware"
)

const moduleReadWorkers = 8

type modulePollResult struct {
	moduleSample
	accepted bool
}

func modulePollDelay(accepted bool) time.Duration {
	if !accepted {
		// Retry on the next tick; a skipped read did not refresh hardware state.
		return 250 * time.Millisecond
	}
	return 25 * time.Second
}

// Least-recently sampled devices go first, independently of discovery order.
func pollingCandidates(seen map[string]hardware.Candidate, pending map[string]bool, due map[string]time.Time, now time.Time) []hardware.Candidate {
	ready := make([]hardware.Candidate, 0, len(seen))
	for key, c := range seen {
		if !pending[key] && !now.Before(due[key]) {
			ready = append(ready, c)
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		a, b := due[ready[i].Key], due[ready[j].Key]
		if a.Equal(b) {
			return ready[i].Key < ready[j].Key
		}
		return a.Before(b)
	})
	return ready
}
