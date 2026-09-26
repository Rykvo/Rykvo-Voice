package ims

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func jitterSamples(n int, first int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = first + int16(i)
	}
	return out
}

func TestRTPJitterReordersWithoutChangingSpeech(t *testing.T) {
	for _, start := range []uint32{1000, ^uint32(0) - 79} {
		var j rtpJitter
		now := time.Unix(1, 0)
		frames := [][]int16{jitterSamples(160, 10), jitterSamples(160, -500), jitterSamples(160, 1000)}
		j.push(65534, start, 7, frames[0], now)
		j.push(0, start+320, 7, frames[2], now.Add(5*time.Millisecond))
		j.push(65535, start+160, 7, frames[1], now.Add(25*time.Millisecond))
		if j.push(0, start+320, 7, jitterSamples(160, 9000), now.Add(30*time.Millisecond)) {
			t.Fatal("duplicate accepted")
		}
		if p, wait := j.pop(now.Add(59 * time.Millisecond)); p != nil || wait != time.Millisecond {
			t.Fatal("no jitter reserve", p, wait)
		}
		for i, want := range frames {
			got, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(i)*rtpFrameTime))
			if !reflect.DeepEqual(got, want) {
				t.Fatal("speech order or amplitude changed", start, i)
			}
		}
		stats := j.snapshot()
		if stats.Reordered != 1 || stats.Duplicates != 1 || stats.MissingSamples != 0 {
			t.Fatal(stats)
		}
	}
}

func TestRTPJitterReordersInitialPackets(t *testing.T) {
	var j rtpJitter
	now := time.Unix(1, 0)
	for _, i := range []int{2, 0, 1} {
		j.push(uint16(10+i), uint32(i*160), 7, jitterSamples(160, int16(i*1000)), now)
	}
	for i := 0; i < 3; i++ {
		p, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(i)*rtpFrameTime))
		if !reflect.DeepEqual(p, jitterSamples(160, int16(i*1000))) {
			t.Fatal("initial reorder", i)
		}
	}
}

func TestRTPJitterLossAndDTXKeepTheTimeline(t *testing.T) {
	for _, lastSequence := range []uint16{11, 13} {
		var j rtpJitter
		now := time.Unix(1, 0)
		j.push(10, 0, 7, jitterSamples(160, 1000), now)
		j.push(lastSequence, 480, 7, jitterSamples(160, 2000), now.Add(40*time.Millisecond))
		for i := 0; i < 4; i++ {
			want := make([]int16, 160)
			if i == 0 {
				want = jitterSamples(160, 1000)
			}
			if i == 3 {
				want = jitterSamples(160, 2000)
			}
			p, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(i)*rtpFrameTime))
			if !reflect.DeepEqual(p, want) {
				t.Fatal("gap compressed or repeated speech", i)
			}
		}
		if lastSequence == 13 && j.push(12, 320, 7, jitterSamples(160, 5000), now.Add(121*time.Millisecond)) {
			t.Fatal("late speech replayed")
		}
		if j.snapshot().MissingSamples != 320 {
			t.Fatal(j.snapshot())
		}
	}
}

func TestRTPJitterVariablePacketDuration(t *testing.T) {
	var j rtpJitter
	now := time.Unix(1, 0)
	want := jitterSamples(640, -1000)
	j.push(1, 0, 7, want[:80], now)
	j.push(3, 320, 7, want[320:], now)
	j.push(2, 80, 7, want[80:320], now)
	for i := 0; i < 4; i++ {
		got, _ := j.pop(now.Add(rtpPlayoutDelay + time.Duration(i)*rtpFrameTime))
		if !reflect.DeepEqual(got, want[i*160:(i+1)*160]) {
			t.Fatal("10/30/40 ms packets changed sample boundaries", i)
		}
	}
}

func TestRTPJitterBoundsStreamPinAndResume(t *testing.T) {
	var j rtpJitter
	now := time.Unix(1, 0)
	p := jitterSamples(160, 1000)
	if j.push(1, 0, 7, nil, now) || j.push(1, 0, 7, make([]int16, 1601), now) {
		t.Fatal("invalid packet length")
	}
	j.push(1, 0, 7, p, now)
	if j.push(2, 160, 8, p, now) || j.push(2, 100000, 7, p, now) {
		t.Fatal("foreign stream or unbounded timestamp")
	}
	// No reader for two seconds; discard old media, never grow a FIFO.
	j.pop(now.Add(2 * time.Second))
	if j.snapshot().PlayoutSkippedSamples == 0 {
		t.Fatal("reader stall not accounted for")
	}
	if !j.push(2, 100000, 7, p, now.Add(2*time.Second)) {
		t.Fatal("continuing source did not recover")
	}
	got, _ := j.pop(now.Add(2*time.Second + rtpPlayoutDelay))
	if !reflect.DeepEqual(got, p) || j.snapshot().Resets != 1 {
		t.Fatal("resume replayed stale audio", j.snapshot())
	}
}

func TestRTPJitterClockAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &rtpMedia{wake: make(chan struct{}, 1), closed: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		m.jitter.push(1, 0, 7, jitterSamples(320, 100), start)
		for i := 0; i < 2; i++ {
			got, err := m.ReadPCM(ctx)
			if err != nil || len(got) != 160 || time.Since(start) != rtpPlayoutDelay+time.Duration(i)*rtpFrameTime {
				t.Fatal("unclocked playout", err, time.Since(start))
			}
		}
		cancel()
		if _, err := m.ReadPCM(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(m.closed)
		if _, err := m.ReadPCM(context.Background()); !errors.Is(err, io.EOF) {
			t.Fatal("audio survived close", err)
		}
	})
}

func TestRTPJitterUDPReorderAndDuplicate(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1)
	receiver, err := newRTPMedia(ip)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if err = receiver.configureRemote(receiver.offerSDP(ip)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 2, 1, 1} {
		packet := make([]byte, 172)
		packet[0], packet[1] = 0x80, 8
		binary.BigEndian.PutUint16(packet[2:4], uint16(n+1))
		binary.BigEndian.PutUint32(packet[4:8], uint32(n*160))
		binary.BigEndian.PutUint32(packet[8:12], 7)
		for i := 12; i < len(packet); i++ {
			packet[i] = linearToALaw(int16((n + 1) * 1000))
		}
		if _, err = sender.WriteToUDP(packet, receiver.conn.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for n := 0; n < 3; n++ {
		got, err := receiver.ReadPCM(ctx)
		want := aLawToLinear(linearToALaw(int16((n + 1) * 1000)))
		if err != nil || len(got) != 160 {
			t.Fatal(err, len(got))
		}
		for _, v := range got {
			if v != want {
				t.Fatal("UDP arrival order corrupted speech", n, v, want)
			}
		}
	}
	stats := receiver.jitter.snapshot()
	if stats.Reordered != 1 || stats.Duplicates != 1 || stats.MissingSamples != 0 {
		t.Fatal(stats)
	}
}

func FuzzRTPJitterWindow(f *testing.F) {
	f.Add([]byte{0, 20, 40, 1, 3, 2, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		var j rtpJitter
		now := time.Unix(1, 0)
		for i, b := range data[:min(len(data), 512)] {
			stamp := uint32(int32(int8(b)) * 160)
			j.push(uint16(i+int(b)), stamp, uint32(b&1), jitterSamples(int(b)+1, int16(b)), now)
			now = now.Add(time.Duration(b) * time.Millisecond)
			if p, wait := j.pop(now); len(p) != 0 && len(p) != 160 || wait < 0 || wait > rtpPlayoutDelay {
				t.Fatal("unbounded jitter state", len(p), wait)
			}
		}
	})
}
