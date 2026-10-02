package ims

import (
	"bytes"
	"context"
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

func amrFixture(wide bool, ft byte) amrFrame {
	kind := 0
	if wide {
		kind = 1
	}
	n := amrFrameBits[kind][ft]
	f := amrFrame{size: 1 + (n+7)/8}
	f.data[0] = ft<<3 | 4
	for i := 1; i < f.size; i++ {
		f.data[i] = byte(i * 17)
	}
	if n%8 != 0 {
		f.data[f.size-1] &= 0xff << (8 - n%8)
	}
	return f
}

func TestAMRPacketFormatsAndBounds(t *testing.T) {
	for _, wide := range []bool{false, true} {
		for _, octet := range []bool{false, true} {
			kind := 0
			if wide {
				kind = 1
			}
			for ft, n := range amrFrameBits[kind] {
				if n < 0 {
					continue
				}
				f := amrFixture(wide, byte(ft))
				for _, count := range []int{1, 2, 10} {
					frames := make([]amrFrame, count)
					for i := range frames {
						frames[i] = f
					}
					data, err := amrPack(frames, 15, wide, octet)
					if err != nil {
						t.Fatal(err)
					}
					p, err := amrParse(data, wide, octet)
					if err != nil || p.cmr != 15 || !slices.Equal(p.frames, frames) {
						t.Fatalf("roundtrip wide=%v octet=%v ft=%d count=%d: %v", wide, octet, ft, count, err)
					}
					if _, err := amrParse(data[:len(data)-1], wide, octet); err == nil {
						t.Fatal("truncated payload")
					}
					if _, err := amrParse(append(data, 0), wide, octet); err == nil {
						t.Fatal("trailing bytes")
					}
				}
			}
		}
	}
	// Independent wire vector: CMR=15, F=0, FT=0, Q=1, 95 zero speech bits.
	f := amrFrame{size: 13}
	f.data[0] = 4
	be, _ := amrPack([]amrFrame{f}, 15, false, false)
	want := make([]byte, 14)
	want[0], want[1] = 0xf0, 0x40
	if !bytes.Equal(be, want) {
		t.Fatalf("BE header %x", be)
	}
	oa, _ := amrPack([]amrFrame{f}, 15, false, true)
	want[1] = 4
	if !bytes.Equal(oa, want) {
		t.Fatalf("OA header %x", oa)
	}
	for _, bad := range [][]byte{nil, {0xf0}, {0xf0, 0x4c}, bytes.Repeat([]byte{0xff}, 900)} {
		if _, err := amrParse(bad, false, true); err == nil {
			t.Fatalf("accepted %x", bad)
		}
	}
}

func amrTestSDP(port, pt int, mapping, fmtp string) []byte {
	return []byte(fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP %d\r\na=rtpmap:%d %s\r\na=fmtp:%d %s\r\n", port, pt, pt, mapping, pt, fmtp))
}

func TestAMRSDPNegotiation(t *testing.T) {
	for _, tc := range []struct {
		mapping, params string
		valid           bool
	}{
		{"AMR/8000", "octet-align=0", true},
		{"AMR/8000", "octet-align=1;", true},
		{"AMR-WB/16000/1", "octet-align=1;mode-set=0,2,4;mode-change-period=2;mode-change-neighbor=1", true},
		{"AMR/8000/2", "octet-align=1", false},
		{"AMR/16000", "octet-align=1", false},
		{"AMR/8000", "crc=1", false},
		{"AMR/8000", "robust-sorting=1", false},
		{"AMR/8000", "interleaving=0", false},
		{"AMR/8000", "mode-set=8", false},
		{"AMR/8000", "mode-set=", false},
		{"AMR/8000", "mode-set=-1", false},
		{"AMR/8000", "octet-align=2", false},
		{"AMR/8000", "octet-align=1;octet-align=0", false},
		{"AMR/8000", "mode-change-period=3", false},
		{"AMR/8000", "octet-align=1;vendor-extension=1", true},
	} {
		o, ok := amrRemoteOptions(amrTestSDP(31000, 102, tc.mapping, tc.params), 102, tc.mapping)
		if ok != tc.valid {
			t.Fatalf("%+v parsed %+v", tc, o)
		}
		if ok && o.modeSet && !strings.Contains(strings.Join(o.attributes(102), "\n"), "mode-set=0,2,4") {
			t.Fatal("mode set changed")
		}
	}
	if _, ok := amrRemoteOptions(append(amrTestSDP(31000, 102, "AMR/8000", "octet-align=0"), []byte("a=maxptime:10\r\n")...), 102, "AMR/8000"); ok {
		t.Fatal("unsupported maxptime")
	}
}

func requireAMR(t *testing.T, wide bool) {
	t.Helper()
	if amrAvailable(wide) {
		return
	}
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		t.Fatal("install libopencore-amrnb0, libopencore-amrwb0 and libvo-amrwbenc0: release codec tests must run")
	}
	t.Skip("AMR native runtime is Linux")
}

func TestAMRNativeRoundTripModesAndClose(t *testing.T) {
	for _, wide := range []bool{false, true} {
		t.Run(fmt.Sprint(wide), func(t *testing.T) {
			requireAMR(t, wide)
			maxMode := 7
			if wide {
				maxMode = 8
			}
			for mode := 0; mode <= maxMode; mode++ {
				for _, octet := range []bool{false, true} {
					o := amrDefaults(wide, octet)
					o.modes = 1 << mode
					c, err := newAMRCodec(o)
					if err != nil {
						t.Fatal(err)
					}
					energy := 0.0
					for i := 0; i < 20; i++ {
						p, err := c.encode(opusTone(i))
						if err != nil {
							t.Fatal(mode, err)
						}
						parsed, err := amrParse(p, wide, octet)
						if err != nil || int(parsed.frames[0].data[0]>>3&15) != mode {
							t.Fatal("mode", mode, err)
						}
						pcm, err := c.decode(p, 160)
						if err != nil || len(pcm) != 160 {
							t.Fatal(err)
						}
						for _, v := range pcm {
							energy += float64(v) * float64(v)
						}
					}
					if math.Sqrt(energy/3200) < 1000 {
						t.Fatal("decoded silence", mode, wide)
					}
					for _, n := range []int{160, 320, 1600} {
						if pcm, err := c.decode(nil, n); err != nil || len(pcm) != n {
							t.Fatal("concealment", err)
						}
					}
					if _, err := c.decode(nil, 20); err == nil {
						t.Fatal("invalid frame size")
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
			}
		})
	}
}

func TestAMRModeChangesAndConcurrentClose(t *testing.T) {
	requireAMR(t, false)
	o := amrDefaults(false, true)
	o.modeSet = true
	o.modes = 1 | 4 | 32 | 128
	o.neighbor = true
	o.period = 2
	c, err := newAMRCodec(o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	c.requestMode(0)
	for i, want := range []int{5, 5, 2, 2, 0, 0} {
		p, err := c.encode(opusTone(i))
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := amrParse(p, false, true)
		if int(parsed.frames[0].data[0]>>3&15) != want {
			t.Fatal("mode step", i, want)
		}
	}
	c.requestMode(1)
	c.requestMode(15)
	if c.requested.Load() != 0 {
		t.Fatal("invalid CMR changed encoder")
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				c.encode(opusTone(i))
				c.decode(nil, 160)
			}
		}()
	}
	c.close()
	wg.Wait()
}

func TestAMRResamplingFiltersAndContinuity(t *testing.T) {
	for _, frequency := range []float64{1000, 6000} {
		input := make([]int16, 3200)
		for i := range input {
			input[i] = int16(10000 * math.Sin(2*math.Pi*frequency*float64(i)/16000))
		}
		var a, b amrResampler
		whole := a.downsample(input)
		var chunks []int16
		for i := 0; i < len(input); i += 320 {
			chunks = append(chunks, b.downsample(input[i:i+320])...)
		}
		if !slices.Equal(whole, chunks) {
			t.Fatal("frame boundary discontinuity")
		}
		energy := 0.0
		for _, v := range whole[160:] {
			energy += float64(v) * float64(v)
		}
		rms := math.Sqrt(energy / float64(len(whole)-160))
		if (frequency == 1000 && (rms < 6500 || rms > 7500)) || (frequency == 6000 && rms > 100) {
			t.Fatal("resampling", frequency, rms)
		}
	}
}

func TestAMRJitterOrderLossWrapAndCMR(t *testing.T) {
	for _, wide := range []bool{false, true} {
		t.Run(fmt.Sprint(wide), func(t *testing.T) {
			requireAMR(t, wide)
			o := amrDefaults(wide, true)
			enc, _ := newAMRCodec(o)
			defer enc.close()
			dec, _ := newAMRCodec(o)
			defer dec.close()
			ref, _ := newAMRCodec(o)
			defer ref.close()
			packets := make([][]byte, 4)
			for i := range packets {
				packets[i], _ = enc.encode(opusTone(i))
				packets[i][0] = byte(i) << 4
			}
			j := rtpJitter{compressed: &compressedPlayout{decode: dec.decode, clockScale: o.clockScale(), quantum: 160}}
			now := time.Now()
			base := uint32(0xfffffff3)
			for _, i := range []int{1, 0, 3} {
				if !j.pushAMR(uint16(65535+i), base+uint32(i*160)*o.clockScale(), 7, packets[i], dec, now) {
					t.Fatal("reorder", i)
				}
			}
			if j.pushAMR(0, base+160*o.clockScale(), 7, packets[1], dec, now) {
				t.Fatal("duplicate")
			}
			if dec.requested.Load() != 3 {
				t.Fatal("reordered CMR replaced newest request")
			}
			for i := 0; i < 4; i++ {
				p := packets[i]
				if i == 2 {
					p = nil
				}
				want, _ := ref.decode(p, 160)
				got, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(i)*rtpFrameTime))
				if !slices.Equal(got, want) {
					t.Fatal("decode order", i)
				}
			}
			if s := j.snapshot(); s.DecodeErrors != 0 || s.MissingSamples != 160 {
				t.Fatal(s)
			}
		})
	}
}

func TestAMRCarrierNegotiationAndLoopback(t *testing.T) {
	for pt := byte(96); pt <= 99; pt++ {
		t.Run(fmt.Sprint(pt), func(t *testing.T) {
			o, _ := amrOfferOptions(pt)
			requireAMR(t, o.wide)
			local := net.IPv4(127, 0, 0, 1)
			a, e := newRTPMedia(local)
			if e != nil {
				t.Fatal(e)
			}
			defer a.Close()
			a.allowAMR = true
			b, e := newRTPMedia(local)
			if e != nil {
				t.Fatal(e)
			}
			defer b.Close()
			b.allowAMR = true
			offer := a.offerSDP(local)
			if !strings.Contains(string(offer), o.name()) {
				t.Fatal("codec not offered")
			}
			makeSDP := func(m *rtpMedia) []byte { return m.buildSDP(local, fmt.Sprint(pt), o.attributes(pt)) }
			if e = a.configureRemote(makeSDP(b)); e != nil {
				t.Fatal(e)
			}
			if e = b.configureRemote(makeSDP(a)); e != nil {
				t.Fatal(e)
			}
			if a.Codec() != o.name() {
				t.Fatal(a.Codec())
			}
			codec := a.amr
			if e = a.configureRemote(makeSDP(b)); e != nil || codec != a.amr {
				t.Fatal("refresh reset", e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					if a.WritePCM(opusTone(i)) != nil || b.WritePCM(opusTone(i)) != nil {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
			results := make(chan error, 2)
			for _, m := range []*rtpMedia{a, b} {
				go func(m *rtpMedia) {
					energy := 0.0
					for i := 0; i < 15; i++ {
						pcm, e := m.ReadPCM(ctx)
						if e != nil || len(pcm) != 160 {
							results <- fmt.Errorf("PCM length=%d error=%v", len(pcm), e)
							return
						}
						for _, v := range pcm {
							energy += float64(v) * float64(v)
						}
					}
					if math.Sqrt(energy/2400) < 1000 || m.jitter.snapshot().DecodeErrors != 0 {
						results <- fmt.Errorf("silent or failed RTP decode")
						return
					}
					results <- nil
				}(m)
			}
			for i := 0; i < 2; i++ {
				if err := <-results; err != nil {
					t.Error(err)
				}
			}
			wg.Wait()
			if a.jitter.snapshot().Packets == 0 || b.jitter.snapshot().Packets == 0 {
				t.Fatal("one-way RTP")
			}
			bad := o
			bad.octet = !bad.octet
			if e = a.configureRemote(b.buildSDP(local, fmt.Sprint(pt), bad.attributes(pt))); e == nil {
				t.Fatal("changed transport accepted")
			}
		})
	}
}

func TestAMRIncomingCarrierACKAndHangup(t *testing.T) {
	for _, wide := range []bool{false, true} {
		t.Run(fmt.Sprint(wide), func(t *testing.T) {
			requireAMR(t, wide)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ip := net.IPv4(127, 0, 0, 1)
			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			peer, err := newRTPMedia(ip)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			peer.allowAMR = true
			s := &Session{fromTag: "local", calls: map[string]*imsCall{}, refreshContext: ctx, conn: conn}
			options := amrDefaults(wide, true)
			options.modeSet = true
			options.modes = 7
			req := &sipRequest{Method: "INVITE", Headers: map[string][]string{
				"via": {"SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKamr"}, "from": {"<sip:remote@example.test>;tag=remote"},
				"to": {"<sip:local@example.test>"}, "call-id": {"amr-incoming"}, "cseq": {"18 INVITE"},
			}, Body: peer.buildSDP(ip, "102", options.attributes(102))}
			responses := make(chan []byte, 8)
			s.handleCallRequest(req, func(p []byte) error { responses <- p; return nil })
			t.Cleanup(func() { s.finishCall("amr-incoming", "ended", 0, "") })
			if p := <-responses; !bytes.HasPrefix(p, []byte("SIP/2.0 180")) {
				t.Fatal("AMR incoming rejected", string(p))
			}
			if call, err := s.AnswerCall(ctx, "amr-incoming"); err != nil || call.MediaReady {
				t.Fatal("media before ACK", err)
			}
			answer, err := parseSIPPacket(<-responses)
			if err != nil || answer.Response == nil || answer.Response.StatusCode != 200 {
				t.Fatal("AMR answer", err)
			}
			if err = peer.configureRemote(answer.Response.Body); err != nil {
				t.Fatal(err)
			}
			ack := &sipRequest{Method: "ACK", Headers: map[string][]string{"call-id": {"amr-incoming"}, "cseq": {"18 ACK"}, "from": {"<sip:remote@example.test>;tag=remote"}, "to": {"<sip:local@example.test>;tag=local"}}}
			s.handleCallRequest(ack, func([]byte) error { return nil })
			if call := s.Calls()[0]; !call.MediaReady || call.Codec != options.name() {
				t.Fatal("AMR not active", call)
			}
			if _, err = s.CallMedia(ctx, "amr-incoming"); err != nil {
				t.Fatal(err)
			}
			bye := &sipRequest{Method: "BYE", Headers: map[string][]string{"via": req.values("Via"), "call-id": {"amr-incoming"}, "cseq": {"19 BYE"}, "from": ack.values("From"), "to": ack.values("To")}}
			s.handleCallRequest(bye, func([]byte) error { return nil })
			if call := s.Calls()[0]; call.State != "ended" || call.MediaReady || call.EndedAt == nil {
				t.Fatal("AMR not released", call)
			}
		})
	}
}

func FuzzAMRPacket(f *testing.F) {
	for _, wide := range []bool{false, true} {
		for _, octet := range []bool{false, true} {
			for _, ft := range []byte{0, 7, 15} {
				data, _ := amrPack([]amrFrame{amrFixture(wide, ft)}, 15, wide, octet)
				f.Add(data, wide, octet)
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, wide, octet bool) {
		p, err := amrParse(data, wide, octet)
		if err != nil {
			return
		}
		if len(p.frames) > amrMaxFrames {
			t.Fatal("unbounded")
		}
		packed, err := amrPack(p.frames, p.cmr, wide, octet)
		if err != nil {
			t.Fatal(err)
		}
		again, err := amrParse(packed, wide, octet)
		if err != nil || !slices.Equal(p.frames, again.frames) {
			t.Fatal("unstable", err)
		}
	})
}
