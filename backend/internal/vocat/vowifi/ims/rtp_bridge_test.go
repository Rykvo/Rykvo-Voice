package ims

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestClientRTPRefreshKeepsCodecAndLearnedEndpoint(t *testing.T) {
	m, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	r := &ClientRTP{media: m}
	offer := "v=0\r\nc=IN IP4 192.0.2.88\r\nm=audio 42000 RTP/AVP 8 0 99\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:99 AMR/8000\r\na=sendrecv\r\n"
	if err := r.SetAnswer(net.IPv4(127, 0, 0, 1), []byte(offer)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.remote.Port = 42002
	m.mu.Unlock()
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{offer, true},
		{strings.Replace(offer, "RTP/AVP 8 0 99", "RTP/AVP 0 99", 1), false},
		{strings.Replace(offer, "PCMA/8000", "PCMU/8000", 1), false},
		{strings.Replace(offer, "42000", "42001", 1), false},
		{strings.Replace(offer, "sendrecv", "inactive", 1), false},
		{strings.Replace(offer, "192.0.2.88", "0.0.0.0", 1), false},
	} {
		if err := r.RefreshOffer([]byte(tc.body)); (err == nil) != tc.ok {
			t.Fatal(tc.ok, err)
		}
		m.mu.RLock()
		endpoint, codec, pt := m.remote.String(), m.codec, m.payloadType
		m.mu.RUnlock()
		if endpoint != "127.0.0.1:42002" || codec != "PCMA" || pt != 8 {
			t.Fatal("refresh changed media", endpoint, codec, pt)
		}
	}
}

func TestClientRTPPinsPeerAndAdvertisesAssignedPort(t *testing.T) {
	peer, err := newRTPMedia(net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	reserve, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.LocalAddr().(*net.UDPAddr).Port
	reserve.Close()
	bridge, err := OpenClientRTP(net.ParseIP("127.0.0.1"), port, net.ParseIP("127.0.0.1"), peer.offerSDP(net.ParseIP("192.0.2.66")), false)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	bridge.media.mu.RLock()
	ip := bridge.media.remote.IP.String()
	bridge.media.mu.RUnlock()
	if ip != "127.0.0.1" || !strings.Contains(string(bridge.Answer(net.ParseIP("203.0.113.1"))), "c=IN IP4 203.0.113.1") {
		t.Fatal("endpoint pinning")
	}
	if err := peer.configureRemote(bridge.Answer(net.ParseIP("127.0.0.1"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = 4096
	}
	if err := peer.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	got, err := bridge.ReadPCM(ctx)
	if err != nil || len(got) != 160 || got[0] < 3900 {
		t.Fatal(err, got)
	}
	if err := bridge.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	got, err = peer.ReadPCM(ctx)
	if err != nil || len(got) != 160 || got[0] < 3900 {
		t.Fatal(err, got)
	}
}

func TestClientRTPAudioPreference(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1)
	mixed := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 42000 RTP/AVP 111 8 0\r\na=rtpmap:111 opus/48000/2\r\n")
	g711 := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 42000 RTP/AVP 8 0\r\n")
	opusOnly := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 42000 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n")
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			if enabled {
				requireOpus(t)
			}
			reserve, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
			if err != nil {
				t.Fatal(err)
			}
			port := reserve.LocalAddr().(*net.UDPAddr).Port
			reserve.Close()
			r, err := NewClientRTPOffer(ip, port, enabled)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if got := strings.Contains(string(r.Offer(ip)), "opus/48000/2"); got != enabled {
				t.Fatal("wrong offer", got, enabled)
			}
			if err = r.SetAnswer(ip, mixed); err != nil {
				t.Fatal(err)
			}
			want := "PCMA"
			if enabled {
				want = "OPUS"
			}
			if r.media.Codec() != want {
				t.Fatal("wrong negotiated codec", r.media.Codec())
			}
			if err = r.AcceptAnswer(ip, opusOnly); (err == nil) != enabled {
				t.Fatal("accepted disabled codec", err)
			}
			r.Close()
			fallback, err := OpenClientRTP(ip, port, ip, g711, enabled)
			if err != nil {
				t.Fatal("G.711 fallback", err)
			}
			if fallback.media.Codec() != "PCMA" {
				t.Fatal("G.711 fallback codec")
			}
			fallback.Close()
			out, err := OpenClientRTP(ip, port, ip, mixed, enabled)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			if out.media.Codec() != want {
				t.Fatal("outgoing ignored audio preference", out.media.Codec())
			}
		})
	}
}
