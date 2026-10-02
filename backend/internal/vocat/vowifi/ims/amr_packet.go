package ims

import "errors"

var errAMRPacket = errors.New("ims: invalid AMR payload")

// RFC 4867 speech bits, excluding the storage-format ToC byte.
var amrFrameBits = [2][16]int{
	{95, 103, 118, 134, 148, 159, 204, 244, 39, -1, -1, -1, -1, -1, -1, 0},
	{132, 177, 253, 285, 317, 365, 397, 461, 477, 40, -1, -1, -1, -1, 0, 0},
}

const amrMaxFrames = 10

type amrFrame struct {
	data [61]byte // Storage ToC + up to 477 speech bits.
	size int
}

type amrPacket struct {
	cmr    int
	frames []amrFrame
}

type amrBitReader struct {
	data []byte
	pos  int
}

func (b *amrBitReader) read(n int) (byte, bool) {
	if n < 0 || n > 8 || b.pos+n > len(b.data)*8 {
		return 0, false
	}
	var v byte
	for i := 0; i < n; i++ {
		v = v<<1 | ((b.data[b.pos/8] >> (7 - b.pos%8)) & 1)
		b.pos++
	}
	return v, true
}

func amrParse(data []byte, wide, octet bool) (amrPacket, error) {
	var p amrPacket
	if len(data) < 2 || len(data) > 1+amrMaxFrames*61 {
		return p, errAMRPacket
	}
	b := amrBitReader{data: data}
	cmr, _ := b.read(4)
	p.cmr = int(cmr)
	if octet {
		b.read(4)
	}
	kind := 0
	if wide {
		kind = 1
	}
	for {
		if len(p.frames) == amrMaxFrames {
			return amrPacket{}, errAMRPacket
		}
		toc, ok := b.read(6)
		if !ok {
			return amrPacket{}, errAMRPacket
		}
		if octet {
			if _, ok := b.read(2); !ok {
				return amrPacket{}, errAMRPacket
			}
		}
		bits := amrFrameBits[kind][(toc>>1)&15]
		if bits < 0 {
			return amrPacket{}, errAMRPacket
		}
		f := amrFrame{size: 1 + (bits+7)/8}
		f.data[0] = (toc & 31) << 2
		p.frames = append(p.frames, f)
		if toc&32 == 0 {
			break
		}
	}
	for i := range p.frames {
		f := &p.frames[i]
		bits := amrFrameBits[kind][(f.data[0]>>3)&15]
		for pos := 0; pos < bits; pos += 8 {
			n := min(8, bits-pos)
			v, ok := b.read(n)
			if !ok {
				return amrPacket{}, errAMRPacket
			}
			f.data[1+pos/8] = v << (8 - n)
		}
		if octet && bits%8 != 0 {
			if _, ok := b.read(8 - bits%8); !ok {
				return amrPacket{}, errAMRPacket
			}
		}
	}
	if len(data)*8-b.pos >= 8 {
		return amrPacket{}, errAMRPacket
	}
	return p, nil
}

func amrPack(frames []amrFrame, cmr int, wide, octet bool) ([]byte, error) {
	if len(frames) == 0 || len(frames) > amrMaxFrames || cmr < 0 || cmr > 15 {
		return nil, errAMRPacket
	}
	kind := 0
	if wide {
		kind = 1
	}
	bits := 4 + len(frames)*6
	if octet {
		bits = 8 + len(frames)*8
	}
	for _, f := range frames {
		n := amrFrameBits[kind][(f.data[0]>>3)&15]
		if n < 0 || f.size != 1+(n+7)/8 {
			return nil, errAMRPacket
		}
		if octet {
			n = (n + 7) / 8 * 8
		}
		bits += n
	}
	out := make([]byte, (bits+7)/8)
	pos := 0
	put := func(v byte, n int) {
		for i := n - 1; i >= 0; i-- {
			out[pos/8] |= ((v >> i) & 1) << (7 - pos%8)
			pos++
		}
	}
	put(byte(cmr), 4)
	if octet {
		put(0, 4)
	}
	for i, f := range frames {
		toc := f.data[0] >> 2 & 31
		if i < len(frames)-1 {
			toc |= 32
		}
		put(toc, 6)
		if octet {
			put(0, 2)
		}
	}
	for _, f := range frames {
		n := amrFrameBits[kind][(f.data[0]>>3)&15]
		for i := 0; i < n; i += 8 {
			count := min(8, n-i)
			put(f.data[1+i/8]>>(8-count), count)
		}
		if octet && n%8 != 0 {
			put(0, 8-n%8)
		}
	}
	return out, nil
}
