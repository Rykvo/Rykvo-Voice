package main

import (
	"log"
	"sync"
	"time"
)

type workerState struct {
	Ready     bool      `json:"ready"`
	CheckedAt time.Time `json:"checkedAt"`
	Issue     string    `json:"issue,omitempty"`
}
type workerStatus struct {
	mu    sync.Mutex
	state workerState
}

func (s *workerStatus) update(name, issue string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue != "" && issue != s.state.Issue {
		log.Printf("%s: %s", name, issue)
	}
	s.state = workerState{Ready: issue == "", CheckedAt: time.Now(), Issue: issue}
}
func (s *workerStatus) snapshot(maxAge time.Duration) workerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.state
	if v.CheckedAt.IsZero() || time.Since(v.CheckedAt) > maxAge {
		v.Ready = false
		v.Issue = "WORKER_STALE"
	}
	return v
}
