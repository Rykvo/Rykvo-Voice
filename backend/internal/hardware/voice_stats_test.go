package hardware

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi/ims"
)

type measuredStreamController struct{ *streamTestController }

func (c measuredStreamController) ReceiveStats() ims.RTPReceiveStats {
	return ims.RTPReceiveStats{Codec: "AMR-WB", Packets: 100, Late: 9, MissingSamples: 160, PlayoutDelayMillis: 180}
}

func TestVoiceAudioStatsCountsActualPrivateQueueDrops(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	v := &WiFiCall{wire: &voiceStreamIO{conn: client}, pcm: make(chan []int16, 3), closed: make(chan struct{}), changed: make(chan struct{}, 1)}
	go v.receive()
	t.Cleanup(func() { client.Close(); <-v.closed })
	wire := &voiceStreamIO{conn: peer}
	pcm, _ := encodeVoicePCM(make([]int16, 160))
	for i := 0; i < 5; i++ {
		if err := wire.write(voicePCM, pcm); err != nil {
			t.Fatal(err)
		}
	}
	state, _ := json.Marshal(voiceStreamState{State: "active", Call: "call", Codec: "PCMU", Audio: &VoiceAudioStats{Packets: 5}})
	if err := wire.write(voiceStatus, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := v.wait(ctx, func(s voiceStreamState) bool { return s.Audio != nil }); err != nil {
		t.Fatal(err)
	}
	if stats := v.AudioStats(); stats.Packets != 5 || stats.PCMDropped != 320 {
		t.Fatal(stats)
	}
}

func TestVoiceAudioStatsKeepsCarrierAndPrivateDropsSeparate(t *testing.T) {
	stats := voiceAudioStats(measuredStreamController{newStreamController()})
	if stats == nil || stats.Packets != 100 || stats.Late != 9 || stats.PCMDropped != 0 {
		t.Fatal(stats)
	}
	v := &WiFiCall{state: voiceStreamState{State: "active", Call: "carrier-call", Codec: "AMR-WB", Audio: stats}}
	v.pcmDropped.Store(320)
	got := v.AudioStats()
	if got.PCMDropped != 320 || got.Late != 9 || stats.PCMDropped != 0 {
		t.Fatal("shared counters mutated", got, stats)
	}
	if voiceAudioStats(newStreamController()) != nil {
		t.Fatal("invented measurements for an uninstrumented source")
	}
}

func TestVoiceAudioStatsFitsPrivateFrameAndValidatesCodec(t *testing.T) {
	n := ^uint64(0)
	s := voiceStreamState{State: "active", Call: strings.Repeat("x", 512), Codec: "AMR-WB",
		Audio: &VoiceAudioStats{Codec: "AMR-WB", Packets: n, Late: n, Reordered: n, Missing: n, Skipped: n, DecodeErrors: n, Resets: n, ForeignStream: n, OutsideWindow: n, DelayMillis: ^uint32(0), PCMDropped: n}}
	b, err := json.Marshal(s)
	if err != nil || len(b) > voiceFrameMax || !s.valid() {
		t.Fatal("oversized stats frame", len(b), err)
	}
	s.Audio.Codec = "invalid"
	if s.valid() {
		t.Fatal("invalid stats codec accepted")
	}
}
