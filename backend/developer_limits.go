package main

import (
	"math"
	"net/http"
	"sync"
	"time"
)

type apiBudget struct {
	key              string
	perMinute, burst int
}

func developerTotalBudget() apiBudget { return apiBudget{"total", 6000, 100} }
func developerBulkBudget() apiBudget  { return apiBudget{"bulk-send", 1200, 25} }

type apiBucket struct {
	tokens float64
	at     time.Time
}

// Authentication, traffic and work limits are local to this host, not each key.
type developerAdmission struct {
	mu                       sync.Mutex
	buckets                  map[string]apiBucket
	sweep                    time.Time
	active, uploads, batches int
}

func (a *developerAdmission) bulk() func() {
	a.mu.Lock()
	if a.batches >= 4 {
		a.mu.Unlock()
		return nil
	}
	a.batches++
	a.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.batches--; a.mu.Unlock() }) }
}

func (a *developerAdmission) take(now time.Time, budgets ...apiBudget) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.buckets == nil {
		a.buckets = make(map[string]apiBucket)
	}
	if !now.Before(a.sweep) {
		for key, b := range a.buckets {
			if now.Sub(b.at) >= 2*time.Minute {
				delete(a.buckets, key)
			}
		}
		a.sweep = now.Add(time.Minute)
	}
	next := make([]apiBucket, len(budgets))
	var retry time.Duration
	missing := 0
	for i, budget := range budgets {
		b, ok := a.buckets[budget.key]
		if !ok {
			missing++
			b = apiBucket{tokens: float64(budget.burst), at: now}
		}
		elapsed := math.Max(0, now.Sub(b.at).Seconds())
		b.tokens = math.Min(float64(budget.burst), b.tokens+elapsed*float64(budget.perMinute)/60)
		b.at = now
		next[i] = b
		if b.tokens < 1 {
			delay := time.Duration(math.Ceil((1 - b.tokens) * 60 / float64(budget.perMinute) * float64(time.Second)))
			retry = max(retry, delay)
		}
	}
	if len(a.buckets)+missing > 4096 {
		return time.Minute
	}
	if retry > 0 {
		return retry
	}
	// Charge all budgets together; a rejected category cannot drain another one.
	for i, budget := range budgets {
		b := next[i]
		b.tokens--
		a.buckets[budget.key] = b
	}
	return 0
}

func developerCategory(method, tail string) apiBudget {
	if method == http.MethodPost && tail == "/messages" {
		return apiBudget{"send", 1500, 32}
	}
	if method == http.MethodPost && tail == "/attachments" {
		return apiBudget{"upload", 300, 4}
	}
	// Status confirmations must not compete with the slower carrier send rate.
	return apiBudget{"read", 3600, 64}
}

func (a *developerAdmission) enter(upload bool) func() {
	a.mu.Lock()
	if a.active >= 16 || upload && a.uploads >= 4 {
		a.mu.Unlock()
		return nil
	}
	a.active++
	if upload {
		a.uploads++
	}
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.active--
			if upload {
				a.uploads--
			}
		})
	}
}

// Inline MMS images use the same image-work slots as /attachments.
func (a *developerAdmission) image() func() {
	a.mu.Lock()
	if a.uploads >= 4 {
		a.mu.Unlock()
		return nil
	}
	a.uploads++
	a.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { a.mu.Lock(); a.uploads--; a.mu.Unlock() }) }
}
