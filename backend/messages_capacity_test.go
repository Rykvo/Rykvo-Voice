package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"rykvo.local/auth/internal/dispatch"
)

func TestMessageCapacityAndFairness(t *testing.T) {
	if messageSendConcurrency != 64 {
		t.Fatal(messageSendConcurrency)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := dispatch.New[int64](ctx, messageSendConcurrency)
	defer func() { cancel(); g.Close() }()
	started := make(chan int64, 120)
	var ids []int64
	for i := int64(1); i <= 120; i++ {
		ids = append(ids, i)
	}
	var cursor int64
	for _, id := range fairMessageModules(ids, cursor) {
		if g.Start(id, func(ctx context.Context) { started <- id; <-ctx.Done() }) {
			cursor = id
		}
	}
	if g.Active() != 64 || cursor != 64 {
		t.Fatal(g.Active(), cursor)
	}
	for range 64 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("workers did not start")
		}
	}
	if g.Start(1, func(context.Context) {}) || g.Start(65, func(context.Context) {}) {
		t.Fatal("capacity/serialization bypass")
	}
	order := fairMessageModules(ids, cursor)
	if order[0] != 65 || order[55] != 120 || order[56] != 1 {
		t.Fatal("early backlog starved later modules", order)
	}
	if !reflect.DeepEqual(fairMessageModules([]int64{9, 1, 3}, 3), []int64{9, 1, 3}) {
		t.Fatal("rotation")
	}
	if len(fairMessageModules(nil, 99)) != 0 {
		t.Fatal("empty queue")
	}
}
