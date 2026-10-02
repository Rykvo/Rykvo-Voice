package dispatch

import (
	"context"
	"sync"
	"testing"
)

func TestKeyLockCancellationIsolationAndCleanup(t *testing.T) {
	var locks Locks[int]
	unlock, err := locks.Lock(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	other, err := locks.Lock(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = locks.Lock(ctx, 1); err == nil {
		t.Fatal("cancelled waiter acquired busy key")
	}
	unlock()
	if len(locks.keys) != 0 {
		t.Fatal("unused key retained")
	}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			unlock, err := locks.Lock(context.Background(), i%4)
			if err == nil {
				unlock()
			}
		})
	}
	wg.Wait()
	if len(locks.keys) != 0 {
		t.Fatal("concurrent key retained")
	}
}
