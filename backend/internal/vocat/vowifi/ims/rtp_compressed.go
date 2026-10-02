package ims

import "time"

type compressedPacket struct {
	start, rtpTimestamp uint32
	size                int
	data                []byte
	pcm                 []int16
	decoded             bool
	amr                 bool
}

type compressedPlayout struct {
	packets              [rtpSampleWindow]*compressedPacket
	rtpAnchor, pcmAnchor uint32
	decode               func([]byte, int) ([]int16, error)
	clockScale           uint32
	quantum              int
}

func (j *rtpJitter) pushOpus(sequence uint16, timestamp, ssrc uint32, data []byte, now time.Time) bool {
	n := opusSamples(data)
	if n == 0 {
		return false
	}
	p := &compressedPacket{size: n, data: append([]byte(nil), data...)}
	return j.pushAudio(sequence, timestamp, ssrc, nil, p, now)
}

func (j *rtpJitter) pushAMR(sequence uint16, timestamp, ssrc uint32, data []byte, codec *amrCodec, now time.Time) bool {
	p, err := amrParse(data, codec.options.wide, codec.options.octet)
	if err != nil {
		return false
	}
	maxMode := 7
	if codec.options.wide {
		maxMode = 8
	}
	for _, f := range p.frames {
		mode := int(f.data[0] >> 3 & 15)
		if mode <= maxMode && codec.options.modes&(1<<mode) == 0 {
			return false
		}
	}
	packet := &compressedPacket{size: len(p.frames) * 160, data: append([]byte(nil), data...), amr: true}
	if !j.pushAudio(sequence, timestamp, ssrc, nil, packet, now) {
		return false
	}
	j.mu.Lock()
	if j.sequence == sequence {
		codec.requestMode(p.cmr)
	}
	j.mu.Unlock()
	return true
}

// Stateful codecs decode in playout order, never UDP arrival order.
func (j *rtpJitter) popCompressed(out []int16) {
	c := j.compressed
	quantum := c.quantum
	for pos := 0; pos < len(out); {
		stamp := j.cursor + uint32(pos)
		index := stamp & (rtpSampleWindow - 1)
		p := c.packets[index]
		if !j.present[index] || j.stamps[index] != stamp {
			p = nil
		}
		count := quantum
		if p == nil {
			// Conceal adjacent missing chunks with a single decoder call.
			for pos+count < len(out) {
				next := stamp + uint32(count)
				i := next & (rtpSampleWindow - 1)
				if j.present[i] && j.stamps[i] == next {
					break
				}
				count += quantum
			}
			pcm, err := c.decode(nil, count)
			if err != nil {
				j.stats.DecodeErrors++
			}
			copy(out[pos:pos+count], pcm)
			j.stats.MissingSamples += uint64(count)
		} else {
			if !p.decoded {
				p.decoded = true
				var err error
				p.pcm, err = c.decode(p.data, p.size)
				if err != nil {
					j.stats.DecodeErrors++
					p.pcm, _ = c.decode(nil, p.size)
				}
				p.data = nil
			}
			offset := int(uint32(stamp - p.start))
			count = min(len(out)-pos, p.size-offset)
			if count <= 0 {
				count = quantum
			}
			if offset >= 0 && offset+count <= len(p.pcm) {
				copy(out[pos:pos+count], p.pcm[offset:offset+count])
			} else {
				j.stats.MissingSamples += uint64(count)
			}
		}
		for i := pos; i < pos+count; i++ {
			idx := (j.cursor + uint32(i)) & (rtpSampleWindow - 1)
			j.present[idx], c.packets[idx] = false, nil
		}
		pos += count
	}
}
