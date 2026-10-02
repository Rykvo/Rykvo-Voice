package ims

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func TestAMRClockPhaseNativeReceive(t *testing.T) {
	for pt := byte(96); pt <= 99; pt++ {
		t.Run(fmt.Sprint(pt), func(t *testing.T) {
			o, _ := amrOfferOptions(pt)
			requireAMR(t, o.wide)
			encoder, err := newAMRCodec(o)
			if err != nil {
				t.Fatal(err)
			}
			defer encoder.close()
			local := net.IPv4(127, 0, 0, 1)
			peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: local})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			media, err := newRTPMedia(local)
			if err != nil {
				t.Fatal(err)
			}
			defer media.Close()
			media.allowAMR = true
			sdp := fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP %d\r\n%s\r\n", peer.LocalAddr().(*net.UDPAddr).Port, pt, strings.Join(o.attributes(pt), "\r\n"))
			if err = media.configureRemote([]byte(sdp)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sent := make(chan error, 1)
			go func() {
				for frame := 0; frame < 30; frame++ {
					payload, err := encoder.encode(opusTone(frame))
					if err != nil {
						sent <- err
						return
					}
					packet := make([]byte, 12+len(payload))
					packet[0], packet[1] = 0x80, pt
					stamp := uint32(160*frame) * o.clockScale()
					if frame >= 3 {
						stamp += 16 * o.clockScale()
					}
					binary.BigEndian.PutUint16(packet[2:], uint16(frame))
					binary.BigEndian.PutUint32(packet[4:], stamp)
					binary.BigEndian.PutUint32(packet[8:], 7)
					copy(packet[12:], payload)
					if _, err = peer.WriteToUDP(packet, media.conn.LocalAddr().(*net.UDPAddr)); err != nil {
						sent <- err
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				sent <- nil
			}()
			energy, samples := 0.0, 0
			for frame := 0; frame < 25; frame++ {
				pcm, err := media.ReadPCM(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if frame >= 10 {
					for _, v := range pcm {
						energy += float64(v) * float64(v)
						samples++
					}
				}
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			stats := media.jitter.snapshot()
			if samples == 0 || math.Sqrt(energy/float64(samples)) < 1000 || stats.TimestampAdjustments != 1 || stats.DecodeErrors != 0 {
				t.Fatalf("AMR receive silent after clock change: samples=%d stats=%+v", samples, stats)
			}
		})
	}
}

func TestAMRTalkspurtClockPhaseDoesNotSilenceCall(t *testing.T) {
	for _, scale := range []uint32{1, 2} {
		for _, shift := range []int32{-16, 16} {
			t.Run(fmt.Sprintf("scale%d/shift%d", scale, shift), func(t *testing.T) {
				j := rtpJitter{compressed: &compressedPlayout{
					clockScale: scale, quantum: 160,
					decode: func(data []byte, n int) ([]int16, error) {
						out := make([]int16, n)
						if len(data) > 0 {
							for i := range out {
								out[i] = int16(data[0]) * 100
							}
						}
						return out, nil
					},
				}}
				now := time.Now()
				base := uint32(0xffffffab)
				stamp := func(frame int) uint32 {
					phase := int32(0)
					if frame >= 2 {
						phase = shift // observed AMR-WB step: 992 instead of 960
					}
					return base + uint32(int32(frame*160)+phase)*scale
				}
				push := func(frame int, ssrc uint32) bool {
					packet := &compressedPacket{size: 160, data: []byte{byte(frame + 1)}, amr: true}
					return j.pushAudio(uint16(65534+frame), stamp(frame), ssrc, nil, packet, now.Add(time.Duration(frame)*rtpFrameTime))
				}
				for _, frame := range []int{0, 1} {
					if !push(frame, 7) {
						t.Fatal("initial frame rejected")
					}
				}
				for frame := 0; frame < 2; frame++ {
					pcm, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(frame)*rtpFrameTime))
					if len(pcm) != 160 || pcm[0] != int16(frame+1)*100 {
						t.Fatal("initial audio missing")
					}
				}
				// Silence gap, followed by reordered speech on a shifted clock.
				if !push(4, 7) || !push(3, 7) || push(3, 7) || push(5, 8) {
					t.Fatal("phase recovery broke reorder, duplicate or SSRC guards")
				}
				for frame := 2; frame < 5; frame++ {
					pcm, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(frame)*rtpFrameTime))
					want := int16(0)
					if frame > 2 {
						want = int16(frame+1) * 100
					}
					if len(pcm) != 160 || pcm[0] != want || pcm[159] != want {
						t.Fatalf("frame %d = %v, want %d", frame, pcm, want)
					}
				}
				for frame := 5; frame < 100; frame++ {
					if !push(frame, 7) {
						t.Fatalf("speech after phase change rejected: %d", frame)
					}
					pcm, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(frame)*rtpFrameTime))
					if len(pcm) != 160 || pcm[0] != int16(frame+1)*100 {
						t.Fatalf("speech after phase change silent: %d", frame)
					}
				}
				stats := j.snapshot()
				if stats.TimestampAdjustments != 1 || stats.MissingSamples != 160 || stats.DecodeErrors != 0 || stats.ForeignStream != 1 {
					t.Fatalf("stats = %+v", stats)
				}
			})
		}
	}
}
