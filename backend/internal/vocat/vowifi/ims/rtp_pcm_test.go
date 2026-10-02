package ims

import (
	"math"
	"reflect"
	"sort"
	"testing"
	"time"
)

func pcmTone(n, offset int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(12000 * math.Sin(2*math.Pi*float64(i+offset)/40))
	}
	return out
}

func TestPCMConcealmentBoundedFadeAndRecovery(t *testing.T) {
	var p pcmConcealer
	for _, v := range pcmTone(400, 0) {
		if p.sample(v, true) != v {
			t.Fatal("healthy audio changed")
		}
	}
	var energy [5]int64
	for i := 0; i < 800; i++ {
		v := int64(p.sample(0, false))
		energy[i/160] += v * v
		if v < -12000 || v > 12000 {
			t.Fatal("concealment amplified audio")
		}
	}
	if energy[0] == 0 || energy[0] <= energy[1] || energy[1] <= energy[2] || energy[3] != 0 || energy[4] != 0 {
		t.Fatal("unbounded or abrupt concealment", energy)
	}
	for i := 0; i < 160; i++ {
		v := p.sample(-30000, true)
		if i < pcmBlend && v != int16(-30000*(i+1)/pcmBlend) || i >= pcmBlend && v != -30000 {
			t.Fatal("recovery did not crossfade once", i, v)
		}
	}
}

func TestPCMConcealmentSilenceAndUnvoicedBounds(t *testing.T) {
	for _, signal := range []string{"empty", "silence", "noise", "full-scale"} {
		t.Run(signal, func(t *testing.T) {
			var p pcmConcealer
			seed := uint32(17)
			if signal != "empty" {
				for i := 0; i < 320; i++ {
					var v int16
					if signal == "noise" {
						seed = seed*1664525 + 1013904223
						v = int16(seed >> 16)
					} else if signal == "full-scale" {
						v = -32768
					}
					p.sample(v, true)
				}
			}
			for i := 0; i < 2000; i++ {
				v := p.sample(0, false)
				if (signal == "empty" || signal == "silence" || i >= pcmFade) && v != 0 {
					t.Fatal("invented or prolonged speech", i, v)
				}
				if signal == "noise" && i >= pcmBlend && v != 0 {
					t.Fatal("unvoiced noise repeated as a tone")
				}
			}
		})
	}
}

func TestPCMConcealmentShortToneError(t *testing.T) {
	var p pcmConcealer
	for _, v := range pcmTone(400, 0) {
		p.sample(v, true)
	}
	var concealed, silence int64
	for _, want := range pcmTone(160, 400) {
		got := p.sample(0, false)
		diff := int64(got) - int64(want)
		concealed += diff * diff
		silence += int64(want) * int64(want)
	}
	if concealed*2 >= silence {
		t.Fatal("synthetic tone did not improve over silence", concealed, silence)
	}
}

func TestRTPG711ConcealsMissingAndReserveWithoutMovingSpeech(t *testing.T) {
	for _, hold := range []bool{false, true} {
		var j rtpJitter
		now := time.Unix(1, 0)
		j.push(1, 0, 7, pcmTone(320, 0), now)
		j.pop(now.Add(60 * time.Millisecond))
		j.pop(now.Add(80 * time.Millisecond))
		if hold {
			j.holdFrames = 1
		}
		out, _ := j.pop(now.Add(100 * time.Millisecond))
		if reflect.DeepEqual(out, make([]int16, 160)) {
			t.Fatal("short gap still replaced with full silence")
		}
		wantCursor := uint32(480)
		if hold {
			wantCursor = 320
		}
		if j.cursor != wantCursor || j.snapshot().MissingSamples+j.snapshot().RebufferSamples != 160 {
			t.Fatal("concealment changed the media clock or hid loss", j.snapshot())
		}
		fresh := pcmTone(160, 23)
		j.push(3, wantCursor, 7, fresh, now.Add(101*time.Millisecond))
		out, _ = j.pop(now.Add(120 * time.Millisecond))
		if !reflect.DeepEqual(out[pcmBlend:], fresh[pcmBlend:]) {
			t.Fatal("valid speech modified after crossfade")
		}
		j.reset(10, 9000, 7, now)
		out, _ = j.pop(now.Add(60 * time.Millisecond))
		if !reflect.DeepEqual(out, make([]int16, 160)) {
			t.Fatal("old call history survived reset")
		}
	}
}

func TestRTPG711QuietReserveReduction(t *testing.T) {
	for _, profile := range []string{"quiet", "speech", "missing", "unstable"} {
		t.Run(profile, func(t *testing.T) {
			var j rtpJitter
			now := time.Unix(1, 0)
			pcm := make([]int16, 160)
			if profile == "speech" {
				pcm = pcmTone(160, 0)
			}
			j.push(0, 0, 7, pcm, now)
			j.delay, j.nextAt = rtpMaxDelay, now.Add(rtpMaxDelay)
			j.stats.PlayoutDelayMillis = 180
			previous := time.Time{}
			for i := 1; i < 650; i++ {
				at := now.Add(time.Duration(i) * rtpFrameTime)
				if profile != "missing" {
					j.push(uint16(i), uint32(i*160), 7, pcm, at)
				}
				if profile == "unstable" && i%20 == 0 {
					j.noteLate(at)
				}
				if out, _ := j.pop(at); out != nil {
					if !previous.IsZero() && at.Sub(previous) != rtpFrameTime {
						t.Fatal("reserve reduction stalled playback")
					}
					previous = at
					if profile == "speech" && !reflect.DeepEqual(out, pcm) {
						t.Fatal("reserve reduction cut speech")
					}
				}
			}
			s := j.snapshot()
			if profile == "quiet" {
				if s.BufferReductions != 6 || s.PlayoutDelayMillis != 60 || s.MissingSamples != 0 {
					t.Fatal("quiet reserve did not return to baseline", s)
				}
			} else if s.BufferReductions != 0 || s.PlayoutDelayMillis != 180 {
				t.Fatal("unsafe reserve reduction", s)
			}
		})
	}
}

func TestRTPG711BurstRecovery(t *testing.T) {
	type arrival struct{ at, index int }
	events := make([]arrival, 400)
	for i := range events {
		delay := 0
		if i >= 50 && i < 65 {
			delay = 250
		}
		events[i] = arrival{i*20 + delay, i}
	}
	sort.SliceStable(events, func(i, k int) bool { return events[i].at < events[k].at })
	var j rtpJitter
	start := time.Unix(1, 0)
	packet, frames := 0, 0
	for ms := 0; ms < 7900; ms++ {
		now := start.Add(time.Duration(ms) * time.Millisecond)
		for packet < len(events) && events[packet].at <= ms {
			i := events[packet].index
			j.push(uint16(65500+i), uint32(0xffffff00+uint64(i*160)), 7, pcmTone(160, i*160), now)
			packet++
		}
		before := j.snapshot()
		if out, _ := j.pop(now); out != nil {
			frames++
			if ms > 3000 && (j.snapshot().MissingSamples != before.MissingSamples || reflect.DeepEqual(out, make([]int16, 160))) {
				t.Fatal("short burst left persistent audio failure", j.snapshot())
			}
		}
	}
	if frames != 392 || j.snapshot().OutsideWindow != 0 {
		t.Fatal("unbounded burst recovery", frames, j.snapshot())
	}
}

func TestRTPG711QuietReductionPreservesNextSpeech(t *testing.T) {
	var j rtpJitter
	now := time.Unix(10, 0)
	speech := pcmTone(160, 0)
	j.push(1, 0, 7, speech, now)
	j.delay, j.quietFrames = rtpMaxDelay, 20
	j.stableAt, j.reducedAt = now.Add(-10*time.Second), now.Add(-10*time.Second)
	j.reduceQuietReserve(now)
	if j.cursor != 0 || j.snapshot().BufferReductions != 0 {
		t.Fatal("quiet reduction discarded speech at the next boundary")
	}
	out, _ := j.pop(now.Add(rtpPlayoutDelay))
	if !reflect.DeepEqual(out, speech) {
		t.Fatal("next speech was changed")
	}
}

func TestPCMConcealmentCallsAreIndependent(t *testing.T) {
	var a, b pcmConcealer
	for _, v := range pcmTone(320, 0) {
		a.sample(v, true)
		b.sample(0, true)
	}
	for i := 0; i < 160; i++ {
		a.sample(0, false)
		if b.sample(0, false) != 0 {
			t.Fatal("audio history crossed call boundaries")
		}
	}
}

func BenchmarkPCMConcealment(b *testing.B) {
	var p pcmConcealer
	pcm := pcmTone(320, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, v := range pcm {
			p.sample(v, true)
		}
		for k := 0; k < 160; k++ {
			p.sample(0, false)
		}
	}
}
