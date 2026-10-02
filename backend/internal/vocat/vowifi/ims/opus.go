package ims

import (
	"errors"
	"sync"
)

const opusPayload = 111

// Each call owns its encoder/decoder. The process only shares the library handle.
type opusCodec struct {
	encodeMu, decodeMu sync.Mutex
	encoder, decoder   uintptr
	api                *opusAPI
}

type opusAPI struct {
	encoderCreate                  func(int32, int32, int32, *int32) uintptr
	decoderCreate                  func(int32, int32, *int32) uintptr
	encoderDestroy, decoderDestroy func(uintptr)
	encode                         func(uintptr, *int16, int32, *byte, int32) int32
	decode                         func(uintptr, *byte, int32, *int16, int32, int32) int32
	encoderCTL                     func(uintptr, int32, int32) int32
	packetSamples                  func(*byte, int32, int32) int32
}

var errOpus = errors.New("ims: Opus codec unavailable")

func opusAvailable() bool { return loadOpus() != nil }

func newOpusCodec(bitrate int) (*opusCodec, error) {
	if bitrate < 6000 || bitrate > 12000 {
		return nil, errOpus
	}
	a := loadOpus()
	if a == nil {
		return nil, errOpus
	}
	c := &opusCodec{api: a}
	var code int32
	c.encoder = a.encoderCreate(8000, 1, 2048, &code) // OPUS_APPLICATION_VOIP
	if code != 0 || c.encoder == 0 {
		c.close()
		return nil, errOpus
	}
	c.decoder = a.decoderCreate(8000, 1, &code)
	if code != 0 || c.decoder == 0 {
		c.close()
		return nil, errOpus
	}
	// Narrowband source; 12 kbit/s CBR, 20 ms, bounded complexity. No DTX/FEC claims.
	for _, setting := range [][2]int32{{4002, int32(bitrate)}, {4006, 0}, {4010, 0}, {4024, 3001}} {
		if a.encoderCTL(c.encoder, setting[0], setting[1]) != 0 {
			c.close()
			return nil, errOpus
		}
	}
	return c, nil
}

func (c *opusCodec) encode(pcm []int16) ([]byte, error) {
	c.encodeMu.Lock()
	defer c.encodeMu.Unlock()
	if c.encoder == 0 || len(pcm) != rtpPacketSamples {
		return nil, errOpus
	}
	out := make([]byte, 64)
	n := c.api.encode(c.encoder, &pcm[0], int32(len(pcm)), &out[0], int32(len(out)))
	if n <= 0 || int(n) > len(out) {
		return nil, errOpus
	}
	return out[:n], nil
}

func opusSamples(data []byte) int {
	a := loadOpus()
	if a == nil || len(data) == 0 || len(data) > 1275 {
		return 0
	}
	n := int(a.packetSamples(&data[0], int32(len(data)), 8000))
	if n < 20 || n > 960 || n%20 != 0 {
		return 0
	}
	return n
}

func (c *opusCodec) decode(data []byte, samples int) ([]int16, error) {
	c.decodeMu.Lock()
	defer c.decodeMu.Unlock()
	if c.decoder == 0 || samples < 20 || samples > 960 || samples%20 != 0 || len(data) > 1275 {
		return nil, errOpus
	}
	var ptr *byte
	if len(data) > 0 {
		ptr = &data[0]
	}
	pcm := make([]int16, samples)
	n := c.api.decode(c.decoder, ptr, int32(len(data)), &pcm[0], int32(samples), 0)
	if n != int32(samples) {
		return nil, errOpus
	}
	return pcm, nil
}

func (c *opusCodec) close() {
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
