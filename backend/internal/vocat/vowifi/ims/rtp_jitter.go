package ims

import (
	"sync"
	"time"
)

const (
	rtpPlayoutDelay = 60 * time.Millisecond
	rtpFrameTime    = 20 * time.Millisecond
	rtpMaxLate      = 120 * time.Millisecond
	rtpSampleWindow = 4096 // Bounded 512 ms window at 8 kHz.
)

// Counters only. MissingSamples includes silence suppression, not just loss.
type RTPReceiveStats struct {
	Packets, Reordered, Duplicates, Late, OutsideWindow, ForeignStream uint64
	Resets, MissingSamples, PlayoutSkippedSamples                      uint64
}

type rtpJitter struct {
	mu          sync.Mutex
	samples     [rtpSampleWindow]int16
	stamps      [rtpSampleWindow]uint32
	present     [rtpSampleWindow]bool
	initialized bool
	playing     bool
	ssrc        uint32
	cursor      uint32
	sequence    uint16
	seen        uint64
	nextAt      time.Time
	lastAt      time.Time
	stats       RTPReceiveStats
}

func (j *rtpJitter) reset(sequence uint16, timestamp, ssrc uint32, now time.Time) {
	clear(j.present[:])
	j.initialized, j.playing = true, false
	j.sequence, j.seen, j.ssrc = sequence, 0, ssrc
	j.cursor, j.nextAt, j.lastAt = timestamp, now.Add(rtpPlayoutDelay), now
}

// A stalled reader must not replay seconds of old speech in a burst.
func (j *rtpJitter) skipStale(now time.Time) {
	if lag := now.Sub(j.nextAt); lag > rtpMaxLate {
		frames := (lag - rtpMaxLate) / rtpFrameTime
		j.cursor += uint32(frames * rtpPacketSamples)
		j.nextAt = j.nextAt.Add(frames * rtpFrameTime)
		j.stats.PlayoutSkippedSamples += uint64(frames * rtpPacketSamples)
	}
}

func (j *rtpJitter) push(sequence uint16, timestamp, ssrc uint32, samples []int16, now time.Time) bool {
	if len(samples) == 0 || len(samples) > 1600 {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stats.Packets++
	if !j.initialized {
		j.reset(sequence, timestamp, ssrc, now)
	} else if ssrc != j.ssrc {
		j.stats.ForeignStream++
		return false
	}
	j.skipStale(now)
	delta := int(int16(sequence - j.sequence))
	if delta <= 0 {
		if delta <= -64 {
			j.stats.Late++
			return false
		}
		if j.seen&(uint64(1)<<(-delta)) != 0 {
			j.stats.Duplicates++
			return false
		}
	}
	offset := int(int32(timestamp - j.cursor))
	// Also reorder the first packets, before any samples have been played.
	if offset < 0 && offset >= -480 && delta < 0 && !j.playing && now.Before(j.nextAt) {
		j.cursor = timestamp
		offset = 0
	}
	if offset+len(samples) <= 0 || offset+len(samples) > rtpSampleWindow {
		// Re-anchor a continuing source after a long pause or timestamp jump.
		if delta > 0 && now.Sub(j.lastAt) >= time.Second {
			j.reset(sequence, timestamp, ssrc, now)
			j.stats.Resets++
			delta, offset = 0, 0
		} else {
			if offset < 0 {
				j.stats.Late++
			} else {
				j.stats.OutsideWindow++
			}
			return false
		}
	}
	if delta > 0 {
		j.seen = j.seen<<delta | 1
		j.sequence = sequence
	} else {
		j.seen |= uint64(1) << (-delta)
		if delta < 0 {
			j.stats.Reordered++
		}
	}
	if offset < 0 {
		j.stats.Late++
	}
	for i := max(0, -offset); i < len(samples); i++ {
		stamp := timestamp + uint32(i)
		index := stamp & (rtpSampleWindow - 1)
		if !j.present[index] || j.stamps[index] != stamp {
			j.samples[index], j.stamps[index], j.present[index] = samples[i], stamp, true
		}
	}
	j.lastAt = now
	return true
}

func (j *rtpJitter) pop(now time.Time) ([]int16, time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.initialized {
		return nil, 0
	}
	j.skipStale(now)
	if wait := j.nextAt.Sub(now); wait > 0 {
		return nil, wait
	}
	out := make([]int16, rtpPacketSamples)
	for i := range out {
		stamp := j.cursor + uint32(i)
		index := stamp & (rtpSampleWindow - 1)
		if j.present[index] && j.stamps[index] == stamp {
			out[i] = j.samples[index]
			j.present[index] = false
		} else {
			j.stats.MissingSamples++
		}
	}
	j.playing = true
	j.cursor += rtpPacketSamples
	j.nextAt = j.nextAt.Add(rtpFrameTime)
	return out, 0
}

func (j *rtpJitter) snapshot() RTPReceiveStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stats
}
