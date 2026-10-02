package dispatch

import (
	"context"
	"sync"
)

type keyLock struct {
	gate  chan struct{}
	users int
}

// Locks serializes one resource and releases unused entries immediately.
type Locks[K comparable] struct {
	mu   sync.Mutex
	keys map[K]*keyLock
}

func (l *Locks[K]) Lock(ctx context.Context, key K) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.keys == nil {
		l.keys = make(map[K]*keyLock)
	}
	entry := l.keys[key]
	if entry == nil {
		entry = &keyLock{gate: make(chan struct{}, 1)}
		l.keys[key] = entry
	}
	entry.users++
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.keys, key)
		}
		l.mu.Unlock()
	}
	select {
	case entry.gate <- struct{}{}:
		return func() { <-entry.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}
