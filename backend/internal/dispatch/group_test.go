package dispatch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndependentKeysAndBoundedConcurrency(t *testing.T) {
	g := New[int](context.Background(), 2)
	defer g.Close()
	blocked := make(chan struct{})
	if !g.Start(1, func(ctx context.Context) { close(blocked); <-ctx.Done() }) {
		t.Fatal("first module rejected")
	}
	<-blocked
	if g.Start(1, func(context.Context) { t.Error("same module overlapped") }) {
		t.Fatal("duplicate key accepted")
	}
	other := make(chan struct{})
	if !g.Start(2, func(ctx context.Context) { close(other); <-ctx.Done() }) {
		t.Fatal("slow module blocked another module")
	}
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("independent work stalled")
	}
	if g.Start(3, func(context.Context) {}) || g.Active() != 2 {
		t.Fatal("concurrency bound ignored")
	}
}

func TestCloseCancelsAndJoinsWorkers(t *testing.T) {
	g := New[int](context.Background(), 4)
	var completed atomic.Int32
	for id := range 4 {
		g.Start(id, func(ctx context.Context) { <-ctx.Done(); completed.Add(1) })
	}
	g.Close()
	if completed.Load() != 4 || g.Active() != 0 || g.Start(5, func(context.Context) {}) {
		t.Fatal("shutdown leaked workers or accepted new work")
	}
	g.Close()
}

func TestStartAndCloseRace(t *testing.T) {
	g := New[int](context.Background(), 8)
	var callers sync.WaitGroup
	for i := range 100 {
		callers.Go(func() { g.Start(i%10, func(ctx context.Context) { <-ctx.Done() }) })
	}
	g.Close()
	callers.Wait()
	if g.Active() != 0 {
		t.Fatal("worker survived close")
	}
}

func TestCompletionWakeFreesSlot(t *testing.T) {
	g := New[int](context.Background(), 1)
	defer g.Close()
	g.Start(1, func(context.Context) {})
	select {
	case <-g.Changed():
	case <-time.After(time.Second):
		t.Fatal("missing completion wake")
	}
	if g.Active() != 0 || !g.Start(2, func(context.Context) {}) {
		t.Fatal("completion wake preceded slot release")
	}
}
