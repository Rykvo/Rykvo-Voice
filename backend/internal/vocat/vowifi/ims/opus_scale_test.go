package ims

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Only gateway DSP runs under load. Synthetic APP encode/decode happens outside
// the timed phase; counting it here would measure twice the server's codec work.
func TestOpusConcurrentCallMedia(t *testing.T) {
	count, _ := strconv.Atoi(os.Getenv("RYKVO_RTP_SCALE"))
	if count == 0 {
		t.Skip("bounded media load is opt-in")
	}
	requireOpus(t)
	seconds, _ := strconv.Atoi(os.Getenv("RYKVO_RTP_SCALE_SECONDS"))
	if seconds == 0 {
		seconds = 30
	}
	if count < 2 || count > 256 || seconds < 6 || seconds > 120 {
		t.Fatal("invalid load dimensions")
	}
	type lane struct {
		app                   *net.UDPConn
		gateway, ims, carrier *rtpMedia
		encoded               [][]byte
		received              [][]byte
		pcm                   [][]int16
		cancel                context.CancelFunc
	}
	ip := net.IPv4(127, 0, 0, 1)
	lanes := make([]*lane, 0, count)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() {
		cancel()
		for _, l := range lanes {
			l.app.Close()
			l.gateway.Close()
			l.ims.Close()
			l.carrier.Close()
		}
		workers.Wait()
	}()
	start := make(chan struct{})
	spawn := func(fn func()) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case <-ctx.Done():
				return
			case <-start:
				fn()
			}
		}()
	}
	for i := 0; i < count; i++ {
		gateway, e := newRTPMedia(ip)
		if e != nil {
			t.Fatal(e)
		}
		gateway.allowOpus = true
		carrier, e := newRTPMedia(ip)
		if e != nil {
			t.Fatal(e)
		}
		bridge, e := newRTPMedia(ip)
		if e != nil {
			t.Fatal(e)
		}
		app, e := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
		if e != nil {
			t.Fatal(e)
		}
		laneCtx, stop := context.WithCancel(ctx)
		l := &lane{app: app, gateway: gateway, ims: bridge, carrier: carrier, cancel: stop}
		l.encoded = make([][]byte, 0, (seconds+1)*50)
		l.received = make([][]byte, 0, (seconds+1)*50)
		l.pcm = make([][]int16, 0, (seconds+1)*50)
		lanes = append(lanes, l)
		sdp := []byte(fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n", app.LocalAddr().(*net.UDPAddr).Port))
		if e = gateway.configureRemote(sdp); e != nil {
			t.Fatal(e)
		}
		if e = bridge.configureRemote(carrier.offerSDP(ip)); e != nil {
			t.Fatal(e)
		}
		if e = carrier.configureRemote(bridge.answerSDP(ip)); e != nil {
			t.Fatal(e)
		}
		freq := float64(200 + i*20)
		tone := func(frame int) []int16 {
			pcm := make([]int16, 160)
			for k := range pcm {
				pcm[k] = int16(7000 * math.Sin(float64(frame*160+k)*2*math.Pi*freq/8000))
			}
			return pcm
		}
		encoder, e := newOpusCodec(12000)
		if e != nil {
			t.Fatal(e)
		}
		for frame := 0; frame < (seconds+1)*50; frame++ {
			p, e := encoder.encode(tone(frame))
			if e != nil {
				t.Fatal(e)
			}
			l.encoded = append(l.encoded, p)
		}
		encoder.close()
		for _, pair := range [][2]*rtpMedia{{gateway, bridge}, {bridge, gateway}} {
			spawn(func() {
				for {
					p, e := pair[0].ReadPCM(laneCtx)
					if e != nil {
						return
					}
					if e = pair[1].WritePCM(p); e != nil {
						if laneCtx.Err() == nil {
							t.Error(e)
						}
						return
					}
				}
			})
		}
		spawn(func() {
			tick := time.NewTicker(rtpFrameTime)
			defer tick.Stop()
			frame := 0
			clockStart := time.Now()
			for {
				select {
				case <-laneCtx.Done():
					return
				case <-tick.C:
					// A PCM clock does not slow down when the test goroutine misses a tick.
					// Deliver queued samples once, as a real audio capture buffer would.
					target := int(time.Since(clockStart) / rtpFrameTime)
					if target-frame > 10 {
						t.Error("fixture capture stalled over 200 ms")
						return
					}
					for frame < target {
						if frame >= len(l.encoded) {
							t.Error("fixture exhausted")
							return
						}
						p := make([]byte, 12+len(l.encoded[frame]))
						p[0], p[1] = 0x80, 111
						binary.BigEndian.PutUint16(p[2:], uint16(frame))
						binary.BigEndian.PutUint32(p[4:], uint32(frame*960))
						binary.BigEndian.PutUint32(p[8:], uint32(i+1))
						copy(p[12:], l.encoded[frame])
						if _, e := app.WriteToUDP(p, gateway.conn.LocalAddr().(*net.UDPAddr)); e != nil {
							t.Error(e)
							return
						}
						if e := carrier.WritePCM(tone(frame)); e != nil {
							t.Error(e)
							return
						}
						frame++
					}
				}
			}
		})
		spawn(func() {
			p := make([]byte, 1500)
			for laneCtx.Err() == nil {
				_ = app.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, _, e := app.ReadFromUDP(p)
				if e != nil {
					if x, ok := e.(net.Error); ok && x.Timeout() {
						continue
					}
					return
				}
				if n < 13 || p[1] != 111 {
					t.Error("invalid Opus wire frame")
					return
				}
				l.received = append(l.received, append([]byte(nil), p[:n]...))
			}
		})
		spawn(func() {
			for {
				p, e := carrier.ReadPCM(laneCtx)
				if e != nil {
					return
				}
				l.pcm = append(l.pcm, p)
			}
		})
	}
	started := time.Now()
	close(start)
	time.Sleep(time.Duration(seconds) * time.Second / 2)
	lanes[0].cancel()
	time.Sleep(time.Duration(seconds) * time.Second / 2)
	cancel()
	workers.Wait()
	var frames, missing, late uint64
	for i, l := range lanes {
		minimum := (seconds - 1) * 50 * 95 / 100
		if i == 0 {
			minimum = (seconds/2 - 1) * 50 * 95 / 100
		}
		if len(l.received) < minimum || len(l.pcm) < minimum {
			t.Errorf("lane %d stalled: app=%d carrier=%d min=%d", i, len(l.received), len(l.pcm), minimum)
		}
		decoder, e := newOpusCodec(12000)
		if e != nil {
			t.Fatal(e)
		}
		appPCM := make([][]int16, 0, len(l.received))
		for n, p := range l.received {
			if n > 0 && uint16(binary.BigEndian.Uint16(p[2:])-binary.BigEndian.Uint16(l.received[n-1][2:])) != 1 {
				t.Errorf("lane %d missing APP RTP", i)
				break
			}
			pcm, e := decoder.decode(p[12:], 160)
			if e != nil {
				t.Error(e)
				break
			}
			appPCM = append(appPCM, pcm)
		}
		decoder.close()
		for d, pcm := range [][][]int16{appPCM, l.pcm} {
			if len(pcm) <= 50 {
				continue
			}
			var crossings, samples, quiet int
			var last int16
			for _, p := range pcm[50:] {
				var energy float64
				for _, v := range p {
					energy += float64(v) * float64(v)
					if last <= 0 && v > 0 {
						crossings++
					}
					last = v
					samples++
				}
				if energy/160 < 100000 {
					quiet++
				}
			}
			want := float64(200 + i*20)
			hz := float64(crossings) * 8000 / float64(samples)
			if math.Abs(hz-want) > want*0.02 || quiet*100 > len(pcm)-50 {
				t.Errorf("lane %d/%d tone=%.1f want=%.1f silent=%d/%d", i, d, hz, want, quiet, len(pcm)-50)
			}
		}
		frames += uint64(len(appPCM) + len(l.pcm))
		for _, m := range []*rtpMedia{l.gateway, l.ims, l.carrier} {
			s := m.jitter.snapshot()
			missing += s.MissingSamples
			late += s.Late
		}
		if i < 3 {
			t.Logf("lane=%d gateway=%+v ims=%+v carrier=%+v", i, l.gateway.jitter.snapshot(), l.ims.jitter.snapshot(), l.carrier.jitter.snapshot())
		}
	}
	t.Logf("gateway_calls=%d seconds=%d frames=%d missing_samples=%d late_packets=%d elapsed_with_offline_verification=%s", count, seconds, frames, missing, late, time.Since(started))
}
