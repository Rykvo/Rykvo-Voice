package ims

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

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
	bridge, err := OpenClientRTP(net.ParseIP("127.0.0.1"), port, net.ParseIP("127.0.0.1"), peer.offerSDP(net.ParseIP("192.0.2.66")))
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
