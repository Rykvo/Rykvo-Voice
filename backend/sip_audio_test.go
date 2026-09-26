package main

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestSIPGatewayPCMQueueBoundsAndSilence(t *testing.T) {
	var q cellularPCMQueue
	samples := make([]int16, 4000)
	for i := range samples {
		samples[i] = int16(i + 1)
	}
	q.push(samples)
	block := make([]int16, cellularPCMBlock)
	for i := 0; i < 3; i++ {
		q.pop(block)
		if !reflect.DeepEqual(block, samples[1600+i*800:2400+i*800]) {
			t.Fatal("burst retained stale audio or reordered samples")
		}
	}
	q.push([]int16{1, -2, 3})
	q.pop(block)
	if !reflect.DeepEqual(block[:3], []int16{1, -2, 3}) {
		t.Fatal("partial block lost")
	}
	for _, v := range block[3:] {
		if v != 0 {
			t.Fatal("underflow repeated old audio instead of silence")
		}
	}
}

func TestSIPGatewayPCMStartupKeepsPartialSpeech(t *testing.T) {
	var q cellularPCMQueue
	p := make([]int16, 800)
	for i := range p {
		p[i] = int16(i - 400)
	}
	q.push(p[:480])
	out := make([]int16, 800)
	if q.pop(out) != -1 || q.n != 480 {
		t.Fatal("partial startup speech consumed before reserve")
	}
	q.push(p[480:])
	if q.pop(out) != 800 || !reflect.DeepEqual(out, p) {
		t.Fatal("startup padding cut a speech block")
	}
	if q.pop(out) != 0 {
		t.Fatal("missing samples were not counted")
	}
	if dropped := q.push(make([]int16, 3000)); dropped != 600 {
		t.Fatal("overflow count", dropped)
	}
}

func TestSIPGatewayPCMPlaybackClockAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		input := make(chan []int16)
		done := make(chan error, 1)
		type written struct {
			samples []int16
			at      time.Time
		}
		events := make(chan written, 32)
		var blocks [][]int16
		var times []time.Time
		capture := func() {
			for {
				select {
				case event := <-events:
					blocks = append(blocks, event.samples)
					times = append(times, event.at)
				default:
					return
				}
			}
		}
		go func() {
			done <- playCellularPCM(ctx, func(ctx context.Context) ([]int16, error) {
				select {
				case p := <-input:
					return p, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}, func(p []int16) error {
				events <- written{append([]int16(nil), p...), time.Now()}
				return nil
			}, nil)
		}()
		synctest.Wait()
		capture()
		if len(blocks) != 2 {
			t.Fatal("USB reserve missing", len(blocks))
		}
		// Five 20 ms RTP packets arrive together; USB still writes at 100 ms.
		for i := 0; i < 5; i++ {
			p := make([]int16, 160)
			for j := range p {
				p[j] = int16(i + 1)
			}
			input <- p
		}
		synctest.Wait()
		capture()
		if len(blocks) != 2 {
			t.Fatal("network burst bypassed playback clock")
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		capture()
		if len(blocks) != 3 {
			t.Fatal(len(blocks))
		}
		for i, v := range blocks[2] {
			if v != int16(i/160+1) {
				t.Fatal("RTP packet boundary changed samples")
			}
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		capture()
		if len(blocks) != 6 {
			t.Fatal("silence stopped USB playback", len(blocks))
		}
		for i, p := range blocks {
			if len(p) != 800 {
				t.Fatal("USB block is not 1600 bytes")
			}
			if i != 2 {
				for _, v := range p {
					if v != 0 {
						t.Fatal("nonzero silence")
					}
				}
			}
			if i >= 2 && times[i].Sub(times[i-1]) != 100*time.Millisecond {
				t.Fatal("unstable output clock")
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		capture()
		if len(blocks) != 6 {
			t.Fatal("audio continued after revocation")
		}
	})
}

func TestSIPGatewayPCMAudioErrorsStopWorkers(t *testing.T) {
	for _, failure := range []string{"read", "write"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				errAudio := errors.New("audio disconnected")
				var readerDone atomic.Bool
				err := playCellularPCM(context.Background(), func(ctx context.Context) ([]int16, error) {
					defer readerDone.Store(true)
					if failure == "read" {
						return nil, errAudio
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}, func([]int16) error {
					if failure == "write" {
						return errAudio
					}
					return nil
				}, nil)
				if !errors.Is(err, errAudio) || !readerDone.Load() {
					t.Fatal("error swallowed or reader leaked", err)
				}
			})
		})
	}
}

func TestSIPGatewayPCMCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := playCellularPCM(ctx, func(ctx context.Context) ([]int16, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, func([]int16) error {
		t.Error("cancelled call wrote USB audio")
		return nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSIPGatewayPCMPreAnswerDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var active atomic.Bool
		input := make(chan []int16)
		var output []int16
		done := make(chan error, 1)
		go func() {
			done <- forwardCallPCM(ctx, &active, func(ctx context.Context) ([]int16, error) {
				select {
				case p := <-input:
					return p, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}, func(p []int16) error { output = append(output, p...); return nil })
		}()
		for i := 0; i < 300; i++ {
			input <- []int16{1}
		}
		synctest.Wait()
		if len(output) != 0 {
			t.Fatal("ringback sent before ACK")
		}
		active.Store(true)
		input <- []int16{2, 3}
		synctest.Wait()
		if !reflect.DeepEqual(output, []int16{2, 3}) {
			t.Fatal("pre-answer audio replayed", output)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
