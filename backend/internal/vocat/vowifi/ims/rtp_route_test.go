package ims

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

type routeTestLease struct{ closed int }

func (l *routeTestLease) Close() error { l.closed++; return nil }
func TestRTPNegotiatesRouteBeforePublishingMedia(t *testing.T) {
	m, e := newRTPMedia(net.ParseIP("127.0.0.1"))
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	var leases []*routeTestLease
	deny := false
	m.route = func(_ context.Context, local, remote *net.UDPAddr) (io.Closer, error) {
		if local.Port == 0 || remote.Port == 0 {
			t.Fatal("missing media port")
		}
		if deny {
			return nil, errors.New("test route unavailable")
		}
		l := &routeTestLease{}
		leases = append(leases, l)
		return l, nil
	}
	sdp := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 31000 RTP/AVP 0\r\n")
	deny = true
	if m.configureRemote(sdp) == nil || m.ready() {
		t.Fatal("missing route reported ready")
	}
	deny = false
	if e = m.configureRemote(sdp); e != nil {
		t.Fatal(e)
	}
	if e = m.configureRemote(sdp); e != nil {
		t.Fatal(e)
	}
	if len(leases) != 1 {
		t.Fatal("duplicate SDP changed route")
	}
	updated := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 32000 RTP/AVP 8\r\n")
	deny = true
	if m.configureRemote(updated) == nil || m.Codec() != "PCMU" || leases[0].closed != 0 {
		t.Fatal("failed update destroyed existing audio")
	}
	deny = false
	if e = m.configureRemote(updated); e != nil {
		t.Fatal(e)
	}
	if len(leases) != 2 || leases[0].closed != 1 || leases[1].closed != 0 {
		t.Fatal("route replacement ownership")
	}
	m.Close()
	m.Close()
	if leases[1].closed != 1 {
		t.Fatal("route survived media close")
	}
	if m.configureRemote(sdp) == nil {
		t.Fatal("closed media reacquired route")
	}
}

func TestRTPRouteNegotiationDuringReceive(t *testing.T) {
	m, err := newRTPMedia(net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	m.route = func(context.Context, *net.UDPAddr, *net.UDPAddr) (io.Closer, error) {
		return &routeTestLease{}, nil
	}
	sdp := []byte(fmt.Sprintf("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio %d RTP/AVP 0\r\n", peer.LocalAddr().(*net.UDPAddr).Port))
	if err = m.configureRemote(sdp); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		packet := make([]byte, 12+rtpPacketSamples)
		packet[0] = 0x80
		binary.BigEndian.PutUint32(packet[8:12], 123)
		for i := 0; i < 500; i++ {
			binary.BigEndian.PutUint16(packet[2:4], uint16(i))
			binary.BigEndian.PutUint32(packet[4:8], uint32(i*rtpPacketSamples))
			if _, e := peer.WriteToUDP(packet, m.conn.LocalAddr().(*net.UDPAddr)); e != nil {
				done <- e
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
		done <- nil
	}()
	for i := 0; i < 500; i++ {
		if err = m.configureRemote(sdp); err != nil {
			t.Error(err)
			break
		}
		time.Sleep(100 * time.Microsecond)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err = m.ReadPCM(ctx); err != nil {
		t.Fatal("no RTP processed during negotiation:", err)
	}
}
