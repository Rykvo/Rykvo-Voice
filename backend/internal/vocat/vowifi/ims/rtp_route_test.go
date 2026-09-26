package ims

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
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
