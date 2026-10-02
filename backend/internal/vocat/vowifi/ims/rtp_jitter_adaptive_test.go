package ims

import (
	"sort"
	"testing"
	"time"
)

func TestRTPJitterAdaptsToRepeatedLatePackets(t *testing.T) {
	for _, profile := range []string{"steady", "isolated", "step", "variable", "batch"} {
		t.Run(profile, func(t *testing.T) {
			type arrival struct{ at, index int }
			events := make([]arrival, 500)
			for i := range events {
				delay := 0
				switch profile {
				case "isolated":
					if i == 50 {
						delay = 100
					}
				case "step":
					if i >= 50 {
						delay = 100
					}
				case "variable":
					delay = i % 6 * 20
				case "batch":
					delay = (4 - i%5) * 20
				}
				events[i] = arrival{i*20 + delay, i}
			}
			sort.SliceStable(events, func(i, k int) bool { return events[i].at < events[k].at })
			var j rtpJitter
			start := time.Unix(1, 0)
			event, previous, lastPlayout, silence, longest := 0, -1, -1, 0, 0
			for ms := 0; ms <= 9900; ms++ {
				now := start.Add(time.Duration(ms) * time.Millisecond)
				for event < len(events) && events[event].at <= ms {
					i := events[event].index
					pcm := make([]int16, 160)
					for k := range pcm {
						pcm[k] = int16(1000 + i)
					}
					j.push(uint16(65500+i), uint32(0xffffff00+uint64(i*160)), 7, pcm, now)
					event++
				}
				cursor, held := j.cursor, j.holdFrames > 0
				index := cursor & (rtpSampleWindow - 1)
				received := j.present[index] && j.stamps[index] == cursor
				blending := j.pcm.lost > 0
				out, _ := j.pop(now)
				if out == nil {
					continue
				}
				if lastPlayout >= 0 && ms-lastPlayout != 20 {
					t.Fatalf("playout stalled: %d ms", ms-lastPlayout)
				}
				lastPlayout = ms
				if held || !received {
					silence++
					longest = max(longest, silence)
					if ms >= 2000 {
						t.Fatalf("continued loss after adaptation at %d ms: %+v", ms, j.snapshot())
					}
					continue
				}
				silence = 0
				i := int(out[len(out)-1]) - 1000
				if i <= previous {
					t.Fatal("replayed old speech", previous, i)
				}
				previous = i
				unchanged := out
				if blending {
					unchanged = out[pcmBlend:]
				}
				for _, sample := range unchanged {
					if sample != int16(1000+i) {
						t.Fatal("changed speech samples")
					}
				}
			}
			if longest > 10 || j.snapshot().Late > 8 {
				t.Fatalf("excessive recovery gap: %d ms, %+v", longest*20, j.snapshot())
			}
			stats := j.snapshot()
			if profile == "step" || profile == "variable" {
				if stats.BufferAdjustments != 2 || stats.RebufferSamples != 320 || stats.PlayoutDelayMillis != 100 {
					t.Fatal("unexpected adaptive reserve", stats)
				}
			} else if stats.BufferAdjustments != 0 || stats.PlayoutDelayMillis != 60 {
				t.Fatal("normal traffic unnecessarily delayed", stats)
			}
		})
	}
}

func TestRTPJitterAdaptiveReserveBoundsAndDuplicates(t *testing.T) {
	var j rtpJitter
	now := time.Unix(1, 0)
	pcm := jitterSamples(160, 1000)
	j.push(1, 0, 7, pcm, now)
	now = now.Add(rtpPlayoutDelay)
	j.pop(now)
	for sequence := uint16(2); sequence <= 31; sequence++ {
		now = now.Add(rtpFrameTime)
		j.push(sequence, j.cursor-160, 7, pcm, now)
		before := j.snapshot()
		for duplicate := 0; duplicate < 5; duplicate++ {
			if j.push(sequence, j.cursor-160, 7, pcm, now) {
				t.Fatal("late duplicate accepted")
			}
		}
		if got := j.snapshot(); got.BufferAdjustments != before.BufferAdjustments || got.Late != before.Late || got.Duplicates != before.Duplicates+5 {
			t.Fatal("duplicates enlarged the buffer", before, got)
		}
		if out, _ := j.pop(now); len(out) != 160 {
			t.Fatal("adaptive buffer interrupted the playout clock")
		}
	}
	if stats := j.snapshot(); stats.PlayoutDelayMillis != 180 || stats.BufferAdjustments != 6 || stats.RebufferSamples != 960 {
		t.Fatal("unbounded adaptive delay", stats)
	}
	// A new timeline after a genuine pause starts at the normal reserve again.
	now = now.Add(2 * time.Second)
	if !j.push(32, 100000, 7, pcm, now) {
		t.Fatal("stream did not resume")
	}
	if stats := j.snapshot(); stats.Resets != 1 || stats.PlayoutDelayMillis != 60 || j.holdFrames != 0 {
		t.Fatal("old adaptive state survived timeline reset", stats)
	}
}

func TestRTPJitterSparseLatePacketsDoNotEnlargeReserve(t *testing.T) {
	var j rtpJitter
	now := time.Unix(1, 0)
	pcm := jitterSamples(160, 1000)
	j.push(1, 0, 7, pcm, now)
	j.pop(now.Add(rtpPlayoutDelay))
	now = now.Add(rtpPlayoutDelay)
	for i := 1; i <= 90; i++ {
		now = now.Add(rtpFrameTime)
		if i%30 == 0 {
			j.push(uint16(i*2), j.cursor-160, 7, pcm, now)
		}
		j.push(uint16(i*2+1), j.cursor, 7, pcm, now)
		j.pop(now)
	}
	if stats := j.snapshot(); stats.BufferAdjustments != 0 {
		t.Fatal("isolated late packets enlarged the buffer", stats)
	}
}
