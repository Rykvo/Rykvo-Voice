package ims

import (
	"context"
	"math"
	"net"
	"testing"
	"time"
)

func TestRTPMediaCarriesPCMOverPCMA(t *testing.T) {
	left, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	if err := left.configureRemote(right.offerSDP(net.IPv4(127, 0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	if err := right.configureRemote(left.answerSDP(net.IPv4(127, 0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	want := make([]int16, rtpPacketSamples)
	for index := range want {
		want[index] = int16(9000 * math.Sin(float64(index)*2*math.Pi/40))
	}
	if err := left.WritePCM(want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := right.ReadPCM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("received %d samples, want %d", len(got), len(want))
	}
	for index := range got {
		if difference := math.Abs(float64(got[index]) - float64(want[index])); difference > 700 {
			t.Fatalf("sample %d difference %.0f exceeds G.711 tolerance", index, difference)
		}
	}
}

func TestParseAudioSDPRejectsMissingEndpoint(t *testing.T) {
	if _, _, _, _, err := parseAudioSDP([]byte("v=0\r\nm=audio 0 RTP/AVP 8\r\n")); err == nil {
		t.Fatal("expected unusable SDP error")
	}
}
