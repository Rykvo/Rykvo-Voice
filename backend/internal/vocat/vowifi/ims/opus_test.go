package ims

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireOpus(t *testing.T) {
	t.Helper()
	if !opusAvailable() {
		if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
			t.Fatal("install libopus0: release codec tests must run")
		}
		t.Skip("native Opus is a Linux runtime dependency")
	}
}

func opusTone(frame int) []int16 {
	x := make([]int16, 160)
	for i := range x {
		x[i] = int16(7000 * math.Sin(float64(frame*160+i)*2*math.Pi*440/8000))
	}
	return x
}

func TestOpusNativeRoundTripAndClose(t *testing.T) {
	requireOpus(t)
	c, err := newOpusCodec(12000)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	var energy float64
	for i := 0; i < 30; i++ {
		p, err := c.encode(opusTone(i))
		if err != nil || len(p) != 30 || opusSamples(p) != 160 {
			t.Fatal(len(p), err)
		}
		pcm, err := c.decode(p, 160)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range pcm {
			energy += float64(s) * float64(s)
		}
	}
	if math.Sqrt(energy/(30*160)) < 1000 {
		t.Fatal("decoded silence")
	}
	for _, n := range []int{20, 40, 60, 80, 100, 120, 140, 160} {
		if pcm, err := c.decode(nil, n); err != nil || len(pcm) != n {
			t.Fatal(n, err)
		}
	}
	c.close()
	c.close()
	if _, err := c.encode(opusTone(0)); err == nil {
		t.Fatal("encode after close")
	}
	if _, err := c.decode(nil, 160); err == nil {
		t.Fatal("decode after close")
	}
}

func TestOpusJitterDecodeOrderLossAndWrap(t *testing.T) {
	requireOpus(t)
	enc, _ := newOpusCodec(12000)
	defer enc.close()
	dec, _ := newOpusCodec(12000)
	defer dec.close()
	ref, _ := newOpusCodec(12000)
	defer ref.close()
	packets := make([][]byte, 4)
	for i := range packets {
		packets[i], _ = enc.encode(opusTone(i))
	}
	j := rtpJitter{compressed: &compressedPlayout{decode: dec.decode, clockScale: 6, quantum: 20}}
	now := time.Now()
	base := uint32(0xfffffe12)
	for _, i := range []int{1, 0, 3} {
		if !j.pushOpus(uint16(65535+i), base+uint32(i*960), 7, packets[i], now) {
			t.Fatal("reorder rejected", i)
		}
	}
	if j.pushOpus(0, base+960, 7, packets[1], now) {
		t.Fatal("duplicate")
	}
	if j.pushOpus(3, base+3840, 8, packets[0], now) {
		t.Fatal("foreign stream")
	}
	for i := 0; i < 4; i++ {
		p := packets[i]
		if i == 2 {
			p = nil
		}
		want, err := ref.decode(p, 160)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(i)*rtpFrameTime))
		if !slices.Equal(got, want) {
			t.Fatal("decoded out of order or wrong concealment", i)
		}
	}
	s := j.snapshot()
	if s.Duplicates != 1 || s.ForeignStream != 1 || s.Reordered != 1 || s.MissingSamples != 160 {
		t.Fatal(s)
	}
	if j.pushOpus(1, base+1920, 7, packets[2], now.Add(150*time.Millisecond)) {
		t.Fatal("late packet replay")
	}
}

func TestCompressedJitterVariablePackets(t *testing.T) {
	var order []int
	j := rtpJitter{compressed: &compressedPlayout{clockScale: 6, quantum: 20, decode: func(data []byte, n int) ([]int16, error) {
		v := 0
		if len(data) > 0 {
			v = int(data[0])
			order = append(order, v)
		}
		out := make([]int16, n)
		for i := range out {
			out[i] = int16(v)
		}
		return out, nil
	}}}
	now := time.Now()
	sizes := []int{20, 40, 80, 20, 960}
	starts := []int{0, 20, 60, 140, 160}
	for _, i := range []int{1, 0, 3, 2, 4} {
		p := &compressedPacket{size: sizes[i], data: []byte{byte(i + 1)}}
		if !j.pushAudio(uint16(i), 123456+uint32(starts[i]*6), 1, nil, p, now) {
			t.Fatal(i)
		}
	}
	for frame := 0; frame < 7; frame++ {
		out, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(frame)*rtpFrameTime))
		for k, v := range out {
			stamp := frame*160 + k
			want := 5
			for i, start := range starts {
				if stamp >= start && stamp < start+sizes[i] {
					want = i + 1
					break
				}
			}
			if int(v) != want {
				t.Fatal(stamp, v, want)
			}
		}
	}
	if !slices.Equal(order, []int{1, 2, 3, 4, 5}) {
		t.Fatal(order)
	}
}

func TestOpusClientNegotiationAndRTP(t *testing.T) {
	requireOpus(t)
	ip := net.IPv4(127, 0, 0, 1)
	m, err := newRTPMedia(ip)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if strings.Contains(strings.ToLower(string(m.offerSDP(ip))), "opus") {
		t.Fatal("carrier IMS changed")
	}
	m.allowOpus = true
	r := &ClientRTP{m}
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	sdp := []byte(fmt.Sprintf("v=0\r\nc=IN IP4 192.0.2.8\r\nm=audio %d RTP/AVP 8 111\r\na=rtpmap:111 opus/48000/2\r\n", peer.LocalAddr().(*net.UDPAddr).Port))
	if err := r.AcceptAnswer(ip, sdp); err != nil {
		t.Fatal(err)
	}
	if m.Codec() != "OPUS" || !strings.Contains(string(r.Answer(ip)), "opus/48000/2") {
		t.Fatal(m.Codec())
	}
	if err := r.RefreshOffer(sdp); err != nil {
		t.Fatal(err)
	}
	if err := r.WritePCM(append(opusTone(0), opusTone(1)...)); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var stamp uint32
	for i := 0; i < 2; i++ {
		b := make([]byte, 2048)
		n, _, err := peer.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		if n != 42 || b[1] != 111 {
			t.Fatal(n, b[1])
		}
		ts := binary.BigEndian.Uint32(b[4:8])
		if i > 0 && ts-stamp != 960 {
			t.Fatal("wrong RTP clock")
		}
		stamp = ts
		_, err = peer.WriteToUDP(b[:n], m.conn.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		pcm, err := r.ReadPCM(ctx)
		if err != nil || len(pcm) != 160 {
			t.Fatal(err)
		}
	}
	if err := r.RefreshOffer([]byte(strings.ReplaceAll(string(sdp), "48000/2", "8000/1"))); err == nil {
		t.Fatal("changed mapping accepted")
	}
}

func TestOpusNegotiationBounds(t *testing.T) {
	requireOpus(t)
	for _, tc := range []struct {
		mapping, extra string
		want           string
	}{
		{"opus/48000/2", "", "OPUS"}, {"OPUS/48000/2", "a=fmtp:112 maxaveragebitrate=6000\r\n", "OPUS"},
		{"opus/48000/1", "", "PCMA"}, {"opus/8000/2", "", "PCMA"},
		{"opus/48000/2", "a=maxptime:10\r\n", "PCMA"}, {"opus/48000/2", "a=fmtp:112 maxaveragebitrate=garbage\r\n", "PCMA"},
	} {
		m, _ := newRTPMedia(net.IPv4(127, 0, 0, 1))
		m.allowOpus = true
		body := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 42000 RTP/AVP 8 112\r\na=rtpmap:112 " + tc.mapping + "\r\n" + tc.extra)
		if err := m.configureRemote(body); err != nil || m.Codec() != tc.want {
			t.Fatal(tc, err, m.Codec())
		}
		if err := (&ClientRTP{m}).AcceptAnswer(net.IPv4(127, 0, 0, 1), body); err == nil {
			t.Fatal("unoffered dynamic PT")
		}
		m.Close()
	}
}

func TestOpusCloseWhileActive(t *testing.T) {
	requireOpus(t)
	c, _ := newOpusCodec(12000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				_, _ = c.encode(opusTone(k))
				_, _ = c.decode(nil, 160)
			}
		}()
	}
	c.close()
	wg.Wait()
}

func BenchmarkOpusRoundTrip(b *testing.B) {
	if !opusAvailable() {
		b.Skip("libopus0")
	}
	for _, complexity := range []int32{0, 1, 3, 5} {
		b.Run(fmt.Sprint(complexity), func(b *testing.B) {
			c, err := newOpusCodec(12000)
			if err != nil {
				b.Fatal(err)
			}
			defer c.close()
			if c.api.encoderCTL(c.encoder, 4010, complexity) != 0 {
				b.Fatal("complexity")
			}
			pcm := opusTone(0)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p, e := c.encode(pcm)
				if e != nil {
					b.Fatal(e)
				}
				if _, e = c.decode(p, 160); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}

func FuzzOpusPacket(f *testing.F) {
	if !opusAvailable() {
		f.Skip("libopus0")
	}
	f.Add([]byte{0xf8, 0xff, 0xfe})
	f.Add([]byte{0x08})
	f.Fuzz(func(t *testing.T, p []byte) {
		n := opusSamples(p)
		if n == 0 {
			return
		}
		c, e := newOpusCodec(12000)
		if e != nil {
			t.Fatal(e)
		}
		defer c.close()
		out, e := c.decode(p, n)
		if e == nil && len(out) != n {
			t.Fatal("decode size")
		}
	})
}
