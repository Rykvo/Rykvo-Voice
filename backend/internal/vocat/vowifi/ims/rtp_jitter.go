package ims

import (
	"sync"
	"time"
)

const (
	rtpPlayoutDelay = 60 * time.Millisecond
	rtpMaxDelay     = 180 * time.Millisecond
	rtpFrameTime    = 20 * time.Millisecond
	rtpMaxLate      = 120 * time.Millisecond
	rtpSampleWindow = 4096 // Bounded 512 ms window at 8 kHz.
)

// Counters only. MissingSamples includes silence suppression, not just loss.
type RTPReceiveStats struct {
	Codec                                                              string
	DecodeErrors                                                       uint64
	Packets, Reordered, Duplicates, Late, OutsideWindow, ForeignStream uint64
	Resets, MissingSamples, PlayoutSkippedSamples                      uint64
	BufferAdjustments, RebufferSamples                                 uint64
	BufferReductions                                                   uint64
	TimestampAdjustments                                               uint64
	PlayoutDelayMillis                                                 uint32
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
	delay       time.Duration
	lateSince   time.Time
	lateCount   int
	holdFrames  int
	pcm         pcmConcealer
	quietFrames int
	stableAt    time.Time
	reducedAt   time.Time
	stats       RTPReceiveStats
	jump        rtpJump
	compressed  *compressedPlayout
}

// Three sequential forward packets distinguish a clock jump from one stray packet.
type rtpJump struct {
	sequence uint16
	end      uint32
	at       time.Time
	count    int
}

func (j *rtpJitter) forwardJump(sequence uint16, timestamp uint32, size int, now time.Time) bool {
	previous := j.jump
	count := 1
	if sequence == previous.sequence+1 && timestamp == previous.end && !now.Before(previous.at) && now.Sub(previous.at) <= 200*time.Millisecond {
		count = previous.count + 1
	}
	j.jump = rtpJump{sequence: sequence, end: timestamp + uint32(size), at: now, count: count}
	return count >= 3
}

func (j *rtpJitter) reset(sequence uint16, timestamp, ssrc uint32, now time.Time) {
	clear(j.present[:])
	if j.compressed != nil {
		clear(j.compressed.packets[:])
	}
	j.jump = rtpJump{}
	j.initialized, j.playing = true, false
	j.sequence, j.seen, j.ssrc = sequence, 0, ssrc
	j.cursor, j.nextAt, j.lastAt = timestamp, now.Add(rtpPlayoutDelay), now
	j.delay, j.lateCount, j.holdFrames = rtpPlayoutDelay, 0, 0
	j.pcm = pcmConcealer{}
	j.quietFrames, j.stableAt, j.reducedAt = 0, now, now
	j.stats.PlayoutDelayMillis = uint32(rtpPlayoutDelay / time.Millisecond)
}

// Grow only after repeated late packets, without replaying consumed speech.
func (j *rtpJitter) noteLate(now time.Time) {
	j.stableAt = now
	if !j.playing || j.delay >= rtpMaxDelay {
		return
	}
	if j.lateCount == 0 || now.Sub(j.lateSince) > 500*time.Millisecond {
		j.lateSince, j.lateCount = now, 1
		return
	}
	j.lateCount++
	if j.lateCount == 3 {
		j.holdFrames++
		j.delay += rtpFrameTime
		j.lateCount = 0
		j.stats.BufferAdjustments++
		j.stats.PlayoutDelayMillis = uint32(j.delay / time.Millisecond)
	}
}

func (j *rtpJitter) remember(sequence uint16, delta int) {
	if delta > 0 {
		j.seen = j.seen<<delta | 1
		j.sequence = sequence
	} else {
		j.seen |= uint64(1) << (-delta)
	}
}

// A stalled reader must not replay seconds of old speech in a burst.
func (j *rtpJitter) skipStale(now time.Time) {
	if lag := now.Sub(j.nextAt); lag > rtpMaxLate {
		frames := (lag - rtpMaxLate) / rtpFrameTime
		j.cursor += uint32(frames * rtpPacketSamples)
		j.nextAt = j.nextAt.Add(frames * rtpFrameTime)
		j.stats.PlayoutSkippedSamples += uint64(frames * rtpPacketSamples)
		j.pcm = pcmConcealer{}
		j.quietFrames, j.stableAt = 0, now
	}
}

func (j *rtpJitter) push(sequence uint16, timestamp, ssrc uint32, samples []int16, now time.Time) bool {
	return j.pushAudio(sequence, timestamp, ssrc, samples, nil, now)
}

func (j *rtpJitter) pushAudio(sequence uint16, timestamp, ssrc uint32, samples []int16, packet *compressedPacket, now time.Time) bool {
	size := len(samples)
	if packet != nil {
		size = packet.size
	}
	if size == 0 || size > 1600 {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stats.Packets++
	adjustedTimestamp := false
	if packet != nil {
		c := j.compressed
		if c == nil || c.clockScale == 0 || c.quantum <= 0 {
			return false
		}
		// Map RTP deltas to the 8 kHz bridge without breaking timestamp wrap.
		scale, quantum := c.clockScale, c.quantum
		delta := int32(timestamp - c.rtpAnchor)
		if j.initialized && delta%int32(scale*uint32(quantum)) != 0 {
			if !packet.amr {
				return false
			}
			// Some AMR gateways shift the sampling phase between talkspurts.
			// Keep whole decoder frames; do not reject the rest of the call.
			grid := int64(scale) * int64(quantum)
			frames := int64(delta) / grid
			if remainder := int64(delta) % grid; remainder >= grid/2 {
				frames++
			} else if remainder <= -grid/2 {
				frames--
			}
			delta = int32(frames * grid)
			adjustedTimestamp = true
		}
		mapped := c.pcmAnchor + uint32(delta/int32(scale))
		if !j.initialized {
			mapped = 0
		}
		packet.rtpTimestamp, timestamp = timestamp, mapped
		packet.start = timestamp
	}
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
	if offset+size <= 0 || offset+size > rtpSampleWindow || (packet != nil && offset < 0) {
		// Re-anchor a continuing source after a long pause or timestamp jump.
		if delta > 0 && (now.Sub(j.lastAt) >= time.Second || offset > rtpSampleWindow && j.forwardJump(sequence, timestamp, size, now)) {
			j.reset(sequence, timestamp, ssrc, now)
			j.stats.Resets++
			delta, offset = 0, 0
		} else {
			if offset < 0 {
				j.stats.Late++
				j.remember(sequence, delta)
				j.noteLate(now)
			} else {
				j.stats.OutsideWindow++
				j.stableAt = now
			}
			return false
		}
	}
	if packet != nil {
		// Reject overlaps before decoding: one stateful frame per sample interval.
		for i := 0; i < size; i++ {
			stamp := timestamp + uint32(i)
			index := stamp & (rtpSampleWindow - 1)
			if j.present[index] && j.stamps[index] == stamp {
				return false
			}
		}
		j.compressed.rtpAnchor, j.compressed.pcmAnchor = packet.rtpTimestamp, timestamp
		if adjustedTimestamp {
			j.stats.TimestampAdjustments++
		}
	}
	j.remember(sequence, delta)
	if delta < 0 {
		j.stats.Reordered++
	}
	if offset < 0 {
		j.stats.Late++
		j.noteLate(now)
	}
	for i := max(0, -offset); i < size; i++ {
		stamp := timestamp + uint32(i)
		index := stamp & (rtpSampleWindow - 1)
		if !j.present[index] || j.stamps[index] != stamp {
			j.stamps[index], j.present[index] = stamp, true
			if packet != nil {
				j.compressed.packets[index] = packet
			} else {
				j.samples[index] = samples[i]
			}
		}
	}
	j.lastAt = now
	j.jump = rtpJump{}
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
	if j.holdFrames > 0 {
		j.quietFrames = 0
		j.holdFrames--
		j.nextAt = j.nextAt.Add(rtpFrameTime)
		j.stats.RebufferSamples += rtpPacketSamples
		if j.compressed == nil {
			for i := range out {
				out[i] = j.pcm.sample(0, false)
			}
		}
		return out, 0
	}
	quiet := j.compressed == nil
	if j.compressed != nil {
		j.popCompressed(out)
	} else {
		for i := range out {
			stamp := j.cursor + uint32(i)
			index := stamp & (rtpSampleWindow - 1)
			if j.present[index] && j.stamps[index] == stamp {
				quiet = quiet && j.samples[index] >= -32 && j.samples[index] <= 32
				out[i] = j.pcm.sample(j.samples[index], true)
				j.present[index] = false
			} else {
				quiet = false
				j.stats.MissingSamples++
				out[i] = j.pcm.sample(0, false)
			}
		}
	}
	j.playing = true
	j.cursor += rtpPacketSamples
	j.nextAt = j.nextAt.Add(rtpFrameTime)
	if quiet {
		j.quietFrames++
	} else {
		j.quietFrames = 0
	}
	j.reduceQuietReserve(now)
	return out, 0
}

// Remove reserve only inside received silence, never by cutting active speech.
func (j *rtpJitter) reduceQuietReserve(now time.Time) {
	if j.delay <= rtpPlayoutDelay || j.quietFrames < 10 || now.Sub(j.stableAt) < 5*time.Second || now.Sub(j.reducedAt) < time.Second {
		return
	}
	for i := uint32(0); i < rtpPacketSamples; i++ {
		stamp := j.cursor + i
		index := stamp & (rtpSampleWindow - 1)
		if !j.present[index] || j.stamps[index] != stamp || j.samples[index] < -32 || j.samples[index] > 32 {
			return
		}
	}
	for i := uint32(0); i < rtpPacketSamples; i++ {
		j.present[(j.cursor+i)&(rtpSampleWindow-1)] = false
	}
	j.cursor += rtpPacketSamples
	j.delay -= rtpFrameTime
	j.reducedAt = now
	j.stats.BufferReductions++
	j.stats.PlayoutDelayMillis = uint32(j.delay / time.Millisecond)
}

func (j *rtpJitter) snapshot() RTPReceiveStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stats
}
