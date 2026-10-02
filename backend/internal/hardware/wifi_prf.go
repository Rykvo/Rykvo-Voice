package hardware

import (
	"encoding/binary"
	"math/bits"
)

// RFC 4187 Appendix A: SHA-1 compression, not padded SHA-1 hashing.
func akaPRF(seed []byte) (keys [160]byte) {
	var x [20]byte
	copy(x[:], seed)
	defer clear(x[:])
	for offset := 0; offset < len(keys); offset += 20 {
		var block [64]byte
		copy(block[:], x[:])
		w := akaSHA1Block(block)
		copy(keys[offset:], w[:])
		carry := uint16(1)
		for i := len(x) - 1; i >= 0; i-- {
			carry += uint16(x[i]) + uint16(w[i])
			x[i], carry = byte(carry), carry>>8
		}
		clear(block[:])
		clear(w[:])
	}
	return keys
}

func akaSHA1Block(block [64]byte) (out [20]byte) {
	var w [80]uint32
	for i := 0; i < 16; i++ {
		w[i] = binary.BigEndian.Uint32(block[i*4:])
	}
	for i := 16; i < 80; i++ {
		w[i] = bits.RotateLeft32(w[i-3]^w[i-8]^w[i-14]^w[i-16], 1)
	}
	h := [5]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476, 0xc3d2e1f0}
	a, b, c, d, e := h[0], h[1], h[2], h[3], h[4]
	for i := 0; i < 80; i++ {
		var f, k uint32
		switch {
		case i < 20:
			f, k = (b&c)|(^b&d), 0x5a827999
		case i < 40:
			f, k = b^c^d, 0x6ed9eba1
		case i < 60:
			f, k = (b&c)|(b&d)|(c&d), 0x8f1bbcdc
		default:
			f, k = b^c^d, 0xca62c1d6
		}
		t := bits.RotateLeft32(a, 5) + f + e + k + w[i]
		a, b, c, d, e = t, a, bits.RotateLeft32(b, 30), c, d
	}
	h[0] += a
	h[1] += b
	h[2] += c
	h[3] += d
	h[4] += e
	for i, v := range h {
		binary.BigEndian.PutUint32(out[i*4:], v)
	}
	clear(w[:])
	clear(h[:])
	clear(block[:])
	return out
}
