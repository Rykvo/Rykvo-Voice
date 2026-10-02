package ims

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in loopback media load, not physical modem or phone acceptance.
func TestRTPConcurrentCallMedia(t *testing.T) {
	count, _ := strconv.Atoi(os.Getenv("RYKVO_RTP_SCALE"))
	if count == 0 {
		t.Skip("set RYKVO_RTP_SCALE for bounded media load")
	}
	if count < 2 || count > 256 {
		t.Fatal("media load must be between 2 and 256 calls")
	}
	seconds := 30
	if raw := os.Getenv("RYKVO_RTP_SCALE_SECONDS"); raw != "" {
		seconds, _ = strconv.Atoi(raw)
	}
	if seconds < 6 || seconds > 120 {
		t.Fatal("media load duration must be between 6 and 120 seconds")
	}
	type lane struct {
		media       [4]*rtpMedia
		cancel      context.CancelFunc
		frames      [2]atomic.Uint64
		silence     [2]atomic.Uint64
		unexpected  atomic.Uint64
		maxGap      [2]atomic.Int64
		beforeFault [2]uint64
	}
	ip := net.IPv4(127, 0, 0, 1)
	lanes := make([]*lane, count)
	var workers sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		for _, l := range lanes {
			if l != nil {
				for _, media := range l.media {
					if media != nil {
						media.Close()
					}
				}
			}
		}
		workers.Wait()
	}()
	start := make(chan struct{})
	var started time.Time
	spawn := func(fn func()) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case <-ctx.Done():
				return
			case <-start:
				fn()
			}
		}()
	}
	for i := range lanes {
		l := &lane{}
		lanes[i] = l
		laneCtx, laneCancel := context.WithCancel(ctx)
		l.cancel = laneCancel
		for k := range l.media {
			media, err := newRTPMedia(ip)
			if err != nil {
				t.Fatal(err)
			}
			l.media[k] = media
		}
		for _, pair := range [][2]int{{0, 1}, {1, 0}, {2, 3}, {3, 2}} {
			if err := l.media[pair[0]].configureRemote(l.media[pair[1]].offerSDP(ip)); err != nil {
				t.Fatal(err)
			}
		}
		// APP <-> gateway <-> IMS <-> carrier, with real loopback UDP on both legs.
		for _, pair := range [][2]int{{1, 2}, {2, 1}} {
			from, to := l.media[pair[0]], l.media[pair[1]]
			spawn(func() {
				for {
					pcm, err := from.ReadPCM(laneCtx)
					if err != nil {
						return
					}
					if err = to.WritePCM(pcm); err != nil {
						if laneCtx.Err() == nil {
							t.Error(err)
						}
						return
					}
				}
			})
		}
		for direction, endpoint := range []int{0, 3} {
			media := l.media[endpoint]
			value := int16(1000 + i*64)
			spawn(func() {
				pcm := make([]int16, rtpPacketSamples)
				for k := range pcm {
					pcm[k] = value
				}
				tick := time.NewTicker(rtpFrameTime)
				defer tick.Stop()
				for {
					select {
					case <-laneCtx.Done():
						return
					case <-tick.C:
						if err := media.WritePCM(pcm); err != nil {
							if laneCtx.Err() == nil {
								t.Error(err)
							}
							return
						}
					}
				}
			})
			spawn(func() {
				want := aLawToLinear(linearToALaw(value))
				var previous time.Time
				for {
					pcm, err := media.ReadPCM(laneCtx)
					if err != nil {
						return
					}
					if time.Since(started) < time.Second {
						continue
					}
					now := time.Now()
					if !previous.IsZero() && now.Sub(previous).Nanoseconds() > l.maxGap[direction].Load() {
						l.maxGap[direction].Store(now.Sub(previous).Nanoseconds())
					}
					previous = now
					l.frames[direction].Add(1)
					for _, sample := range pcm {
						if sample == aLawToLinear(linearToALaw(0)) || sample == 0 {
							l.silence[direction].Add(1)
						} else if sample != want {
							l.unexpected.Add(1)
						}
					}
				}
			})
		}
	}
	started = time.Now()
	close(start)
	time.Sleep(time.Duration(seconds) * time.Second / 2)
	for _, l := range lanes {
		for d := range l.frames {
			l.beforeFault[d] = l.frames[d].Load()
		}
	}
	lanes[0].cancel()
	time.Sleep(time.Duration(seconds) * time.Second / 2)
	cancel()
	workers.Wait()
	var frames, silence, late, adjusted uint64
	var maxGap int64
	for i, l := range lanes {
		if l.unexpected.Load() != 0 {
			t.Errorf("lane %d: foreign or altered samples", i)
		}
		for d := range l.frames {
			maxGap = max(maxGap, l.maxGap[d].Load())
			n, empty := l.frames[d].Load(), l.silence[d].Load()
			frames += n
			silence += empty
			if i > 0 && n-l.beforeFault[d] < uint64(seconds*25*95/100) {
				t.Errorf("lane %d/%d stalled after another lane closed: %d frames", i, d, n-l.beforeFault[d])
			}
			if n == 0 || empty*100 > n*rtpPacketSamples {
				t.Errorf("lane %d/%d: more than 1%% silence (%d/%d samples)", i, d, empty, n*rtpPacketSamples)
			}
		}
		for _, media := range l.media {
			stats := media.jitter.snapshot()
			late += stats.Late
			adjusted += stats.BufferAdjustments
		}
	}
	t.Logf("calls=%d seconds=%d frames=%d silence_samples=%d late_packets=%d buffer_adjustments=%d max_read_gap_ms=%.2f", count, seconds, frames, silence, late, adjusted, float64(maxGap)/float64(time.Millisecond))
}
