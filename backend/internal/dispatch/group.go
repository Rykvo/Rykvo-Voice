package dispatch

import (
	"context"
	"sync"
)

// Group bounds active work without letting a busy key occupy another slot.
type Group[K comparable] struct {
	mu      sync.Mutex
	active  map[K]struct{}
	limit   int
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	changed chan struct{}
}

func New[K comparable](ctx context.Context, limit int) *Group[K] {
	if limit < 1 {
		panic("dispatch limit must be positive")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Group[K]{active: make(map[K]struct{}), limit: limit, ctx: ctx, cancel: cancel, changed: make(chan struct{}, 1)}
}

func (g *Group[K]) Start(key K, run func(context.Context)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.ctx.Err() != nil || len(g.active) >= g.limit {
		return false
	}
	if _, exists := g.active[key]; exists {
		return false
	}
	g.active[key] = struct{}{}
	g.wg.Add(1)
	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.active, key)
			g.mu.Unlock()
			select {
			case g.changed <- struct{}{}:
			default:
			}
			g.wg.Done()
		}()
		run(g.ctx)
	}()
	return true
}

func (g *Group[K]) Changed() <-chan struct{} { return g.changed }

func (g *Group[K]) Active() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.active)
}

func (g *Group[K]) Close() {
	g.mu.Lock()
	g.closed = true
	g.cancel()
	g.mu.Unlock()
	g.wg.Wait()
}
