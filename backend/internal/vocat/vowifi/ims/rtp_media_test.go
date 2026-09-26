package ims

import (
	"context"
	"math"
	"net"
	"strings"
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

func TestRTPDoesNotAdvertiseUnimplementedCodecs(t *testing.T) {
	media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	offer := string(media.offerSDP(net.IPv4(127, 0, 0, 1)))
	if strings.Contains(offer, "AMR") || strings.Contains(offer, "telephone-event") || !strings.Contains(offer, "RTP/AVP 8 0") {
		t.Fatal(offer)
	}
}

func TestRTPNegotiatesOnlyRealMonoG711(t *testing.T) {
	for _, tc := range []struct{ formats, mapping, codec string }{
		{"8", "", "PCMA"}, {"0", "", "PCMU"}, {"96", "a=rtpmap:96 PCMA/8000/1\r\n", "PCMA"},
		{"104 8", "a=rtpmap:104 AMR-WB/16000\r\n", "PCMA"},
		{"104", "a=rtpmap:104 AMR-WB/16000\r\n", ""},
		{"102", "a=rtpmap:102 AMR/8000\r\n", ""},
		{"100", "a=rtpmap:100 telephone-event/8000\r\n", ""},
		{"111", "a=rtpmap:111 opus/48000/2\r\n", ""},
		{"8", "a=rtpmap:8 PCMA/16000\r\n", ""},
		{"8", "a=rtpmap:8 PCMA/8000/2\r\n", ""},
		{"8", "a=rtpmap:8 PCMU/8000\r\n", ""},
		{"97", "", ""},
	} {
		t.Run(tc.formats+tc.mapping, func(t *testing.T) {
			media, err := newRTPMedia(net.IPv4(127, 0, 0, 1))
			if err != nil {
				t.Fatal(err)
			}
			defer media.Close()
			err = media.configureRemote([]byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 24000 RTP/AVP " + tc.formats + "\r\n" + tc.mapping))
			if tc.codec == "" {
				if err == nil || media.ready() {
					t.Fatal("unsupported codec accepted")
				}
			} else if err != nil || media.Codec() != tc.codec {
				t.Fatal(err, media.Codec())
			}
		})
	}
}

func TestRTPFirstAudioDoesNotUseFollowingVideoAddress(t *testing.T) {
	ip, port, _, _, err := parseAudioSDP([]byte("v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 24000 RTP/AVP 8\r\nm=video 25000 RTP/AVP 96\r\nc=IN IP4 192.0.2.2\r\n"))
	if err != nil || ip.String() != "192.0.2.1" || port != 24000 {
		t.Fatal(ip, port, err)
	}
}
