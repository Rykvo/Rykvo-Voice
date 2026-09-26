package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const cellularPCMBlock = 800 // 100 ms, mono 8 kHz; 1600 USB bytes.

type cellularPCMQueue struct {
	mu      sync.Mutex
	samples [3 * cellularPCMBlock]int16
	head, n int
	primed  bool
}

func (q *cellularPCMQueue) push(samples []int16) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	dropped := max(0, q.n+len(samples)-len(q.samples))
	for _, sample := range samples {
		if q.n == len(q.samples) {
			q.head = (q.head + 1) % len(q.samples)
			q.n--
		}
		q.samples[(q.head+q.n)%len(q.samples)] = sample
		q.n++
	}
	return dropped
}

func (q *cellularPCMQueue) pop(block []int16) int {
	clear(block)
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.primed && q.n < len(block) {
		return -1 // Preserve the first partial speech block until it is complete.
	}
	q.primed = true
	filled := min(q.n, len(block))
	for i := 0; i < len(block) && q.n > 0; i++ {
		block[i] = q.samples[q.head]
		q.head = (q.head + 1) % len(q.samples)
		q.n--
	}
	return filled
}

type cellularPCMStats struct {
	warmup, missing, dropped, lateWrites atomic.Uint64
}

// Keep USB playback clocked even when RTP packets arrive in bursts or pause.
func playCellularPCM(ctx context.Context, read func(context.Context) ([]int16, error), write func([]int16) error, stats *cellularPCMStats) error {
	if stats == nil {
		stats = &cellularPCMStats{}
	}
	ctx, cancel := context.WithCancel(ctx)
	var queue cellularPCMQueue
	readErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			samples, err := read(ctx)
			if err != nil {
				readErr <- fmt.Errorf("RTP read: %w", err)
				return
			}
			if ctx.Err() != nil {
				return
			}
			stats.dropped.Add(uint64(queue.push(samples)))
		}
	}()
	defer func() { cancel(); <-done }()
	var previousWrite time.Time
	writeBlock := func(block []int16) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		if !previousWrite.IsZero() && now.Sub(previousWrite) > 150*time.Millisecond {
			stats.lateWrites.Add(1)
		}
		previousWrite = now
		if err := write(block); err != nil {
			return fmt.Errorf("modem write: %w", err)
		}
		return nil
	}
	block := make([]int16, cellularPCMBlock)
	// A one-block reserve prevents tiny scheduling delays from emptying USB audio.
	for i := 0; i < 2; i++ {
		if err := writeBlock(block); err != nil {
			return err
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-tick.C:
			if filled := queue.pop(block); filled < 0 {
				stats.warmup.Add(uint64(len(block)))
			} else {
				stats.missing.Add(uint64(len(block) - filled))
			}
			if err := writeBlock(block); err != nil {
				return err
			}
		}
	}
}

// Drain ringback immediately; never replay pre-answer USB audio after the ACK.
func forwardCellularPCM(ctx context.Context, active *atomic.Bool, read func(context.Context) ([]int16, error), write func([]int16) error) error {
	for {
		pcm, err := read(ctx)
		if err != nil {
			return fmt.Errorf("modem read: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if active.Load() {
			if err := write(pcm); err != nil {
				return fmt.Errorf("RTP write: %w", err)
			}
		}
	}
}
