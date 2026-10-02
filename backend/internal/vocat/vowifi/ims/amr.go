package ims

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
)

var errAMR = errors.New("ims: AMR codec unavailable")

type amrAPI struct {
	encoderCreate, decoderCreate   func() uintptr
	encoderDestroy, decoderDestroy func(uintptr)
	encode                         func(uintptr, int32, *int16, *byte, int32) int32
	decode                         func(uintptr, *byte, *int16, int32)
}

type amrCodec struct {
	encodeMu, decodeMu sync.Mutex
	encoder, decoder   uintptr
	api                *amrAPI
	options            amrOptions
	mode               int
	frame              uint64
	requested          atomic.Int32
	up, down           amrResampler
}

func amrAvailable(wide bool) bool { return loadAMR(wide) != nil }

func newAMRCodec(options amrOptions) (*amrCodec, error) {
	a := loadAMR(options.wide)
	allowed := uint16(255)
	if options.wide {
		allowed = 511
	}
	if a == nil || options.modes == 0 || options.modes&^allowed != 0 || options.period < 1 || options.period > 2 {
		return nil, errAMR
	}
	c := &amrCodec{api: a, options: options}
	for i := 0; i < 9; i++ {
		if options.modes&(1<<i) != 0 {
			c.mode = i
		}
	}
	c.requested.Store(int32(c.mode))
	c.encoder, c.decoder = a.encoderCreate(), a.decoderCreate()
	if c.encoder == 0 || c.decoder == 0 {
		c.close()
		return nil, errAMR
	}
	return c, nil
}

func (c *amrCodec) requestMode(mode int) {
	if mode >= 0 && mode < 9 && c.options.modes&(1<<mode) != 0 {
		c.requested.Store(int32(mode))
	}
}

func (c *amrCodec) encode(pcm []int16) ([]byte, error) {
	c.encodeMu.Lock()
	defer c.encodeMu.Unlock()
	if c.encoder == 0 || len(pcm) != rtpPacketSamples {
		return nil, errAMR
	}
	if c.frame%uint64(c.options.period) == 0 {
		target := int(c.requested.Load())
		if c.options.neighbor && target != c.mode {
			step := 1
			if target < c.mode {
				step = -1
			}
			for m := c.mode + step; m >= 0 && m < 9; m += step {
				if c.options.modes&(1<<m) != 0 {
					target = m
					break
				}
			}
		}
		c.mode = target
	}
	input := pcm
	if c.options.wide {
		input = c.up.upsample(pcm)
	}
	f := amrFrame{}
	n := int(c.api.encode(c.encoder, int32(c.mode), &input[0], &f.data[0], 0))
	if n < 1 || n > len(f.data) || int((f.data[0]>>3)&15) != c.mode {
		return nil, errAMR
	}
	f.size = n
	c.frame++
	return amrPack([]amrFrame{f}, 15, c.options.wide, c.options.octet)
}

func (c *amrCodec) decode(data []byte, samples int) ([]int16, error) {
	c.decodeMu.Lock()
	defer c.decodeMu.Unlock()
	if c.decoder == 0 || samples < 160 || samples > 160*amrMaxFrames || samples%160 != 0 {
		return nil, errAMR
	}
	var frames []amrFrame
	if len(data) > 0 {
		p, e := amrParse(data, c.options.wide, c.options.octet)
		if e != nil || len(p.frames)*160 != samples {
			return nil, errAMRPacket
		}
		frames = p.frames
	}
	out := make([]int16, 0, samples)
	for i := 0; i < samples/160; i++ {
		f := amrFrame{size: 1}
		f.data[0] = 15 << 3 // NO_DATA for decoder concealment.
		bad := int32(1)
		if len(frames) > 0 {
			f = frames[i]
			if f.data[0]&4 != 0 {
				bad = 0
			}
		}
		var pcm [320]int16
		c.api.decode(c.decoder, &f.data[0], &pcm[0], bad)
		if c.options.wide {
			out = append(out, c.down.downsample(pcm[:])...)
		} else {
			out = append(out, pcm[:160]...)
		}
	}
	return out, nil
}

func (c *amrCodec) close() {
	if c == nil {
		return
	}
	c.encodeMu.Lock()
	defer c.encodeMu.Unlock()
	c.decodeMu.Lock()
	defer c.decodeMu.Unlock()
	if c.encoder != 0 {
		c.api.encoderDestroy(c.encoder)
		c.encoder = 0
	}
	if c.decoder != 0 {
		c.api.decoderDestroy(c.decoder)
		c.decoder = 0
	}
}

// Stateful low-pass conversion for the existing 8 kHz PCM bridge; no wideband claim.
var amrLowpass = func() [31]float64 {
	var h [31]float64
	sum := 0.0
	for i := range h {
		x := float64(i - 15)
		v := 0.45
		if x != 0 {
			v = math.Sin(math.Pi*0.45*x) / (math.Pi * x)
		}
		h[i] = v * (0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/30))
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}()

type amrResampler struct {
	history [31]float64
	pos     int
}

func (r *amrResampler) filter(sample float64) int16 {
	r.history[r.pos] = sample
	v := 0.0
	index := r.pos
	for _, h := range amrLowpass {
		v += h * r.history[index]
		index--
		if index < 0 {
			index = 30
		}
	}
	r.pos = (r.pos + 1) % 31
	return int16(math.Round(max(-32768, min(32767, v))))
}
func (r *amrResampler) upsample(pcm []int16) []int16 {
	out := make([]int16, len(pcm)*2)
	for i, v := range pcm {
		out[2*i] = r.filter(float64(v) * 2)
		out[2*i+1] = r.filter(0)
	}
	return out
}
func (r *amrResampler) downsample(pcm []int16) []int16 {
	out := make([]int16, len(pcm)/2)
	for i, v := range pcm {
		s := r.filter(float64(v))
		if i%2 == 0 {
			out[i/2] = s
		}
	}
	return out
}
