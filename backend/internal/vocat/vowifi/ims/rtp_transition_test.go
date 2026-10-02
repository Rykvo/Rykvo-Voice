package ims

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestCallMediaEarlyNegotiationGate(t *testing.T) {
	m, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.configureRemote([]byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 25000 RTP/AVP 0\r\n")); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"dialing", "ringing", "early_media", "active", "ending", "ended"} {
		for _, ready := range []bool{false, true} {
			for _, terminated := range []bool{false, true} {
				s := &Session{calls: map[string]*imsCall{"fixture": {public: vowifi.Call{State: state, MediaReady: ready}, media: m, terminated: terminated}}}
				got, err := s.CallMedia(context.Background(), "fixture")
				allowed := ready && !terminated && (state == "early_media" || state == "active")
				if (err == nil && got != nil) != allowed {
					t.Fatalf("state=%s ready=%t ended=%t: %v", state, ready, terminated, err)
				}
			}
		}
	}
}

func TestRTPForwardClockJumpNeedsConsecutiveEvidence(t *testing.T) {
	for _, base := range []uint32{0, ^uint32(0) - 500} {
		var j rtpJitter
		now := time.Unix(1, 0)
		p := jitterSamples(160, 100)
		j.push(100, base, 7, p, now)
		j.pop(now.Add(rtpPlayoutDelay))
		for i := 0; i < 3; i++ {
			accepted := j.push(uint16(101+i), base+8000+uint32(i*160), 7, p, now.Add(time.Duration(80+i*20)*time.Millisecond))
			if accepted != (i == 2) {
				t.Fatalf("forward clock jump: packet %d accepted=%t", i, accepted)
			}
		}
		if j.snapshot().Resets != 1 || j.snapshot().OutsideWindow != 2 {
			t.Fatal(j.snapshot())
		}
		got, _ := j.pop(now.Add(120*time.Millisecond + rtpPlayoutDelay))
		if len(got) != 160 || got[0] != p[0] {
			t.Fatal("new clock did not resume promptly")
		}
	}
}

func TestRTPForwardClockJumpRejectsStrays(t *testing.T) {
	for _, mode := range []string{"duplicate", "reorder", "timestamp", "foreign", "old"} {
		t.Run(mode, func(t *testing.T) {
			var j rtpJitter
			now := time.Unix(1, 0)
			p := jitterSamples(160, 100)
			j.push(100, 8000, 7, p, now)
			j.pop(now.Add(rtpPlayoutDelay))
			for i := 0; i < 5; i++ {
				seq, stamp, ssrc := uint16(101+i), uint32(16000+i*160), uint32(7)
				switch mode {
				case "duplicate":
					seq, stamp = 101, 16000
				case "reorder":
					seq = uint16(105 - i)
				case "timestamp":
					stamp += uint32(i * 200)
				case "foreign":
					ssrc = 8
				case "old":
					stamp = uint32(i * 160)
				}
				if j.push(seq, stamp, ssrc, p, now.Add(time.Duration(80+i*20)*time.Millisecond)) {
					t.Fatal("stray or stale audio reset timeline")
				}
			}
			if j.snapshot().Resets != 0 {
				t.Fatal(j.snapshot())
			}
		})
	}
}

func TestRTPNegotiatedMediaTransition(t *testing.T) {
	m, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	sdp := func(port, payload int) []byte {
		return []byte(fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP %d\r\n", port, payload))
	}
	if err = m.configureRemote(sdp(25000, 0)); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1, 0)
	m.jitter.push(1, 100, 7, jitterSamples(160, 10), now)
	m.mu.Lock()
	m.remote.Port = 25001 // Learned NAT port survives the same SDP.
	m.mu.Unlock()
	if err = m.configureRemote(sdp(25000, 0)); err != nil {
		t.Fatal(err)
	}
	if m.remote.Port != 25001 || !m.jitter.initialized || m.jitter.stats.Resets != 0 {
		t.Fatal("unchanged SDP reset NAT or audio")
	}
	for _, change := range [][2]int{{25002, 0}, {25002, 8}} {
		if err = m.configureRemote(sdp(change[0], change[1])); err != nil {
			t.Fatal(err)
		}
		if m.jitter.initialized {
			t.Fatal("old source pinned after negotiated transition")
		}
		if !m.jitter.push(1, 5000, 9, jitterSamples(160, 20), now) {
			t.Fatal("new source rejected")
		}
	}
	if m.jitter.stats.Resets != 2 {
		t.Fatal(m.jitter.stats)
	}
}
