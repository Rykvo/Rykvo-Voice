package ims

import (
	"context"
	"errors"
	"net"
)

// ClientRTP reuses the same G.711 codec; the caller reserves its listener port.
type ClientRTP struct{ media *rtpMedia }

func OpenClientRTP(bind net.IP, port int, peer net.IP, offer []byte) (*ClientRTP, error) {
	if bind == nil || bind.IsUnspecified() || peer == nil || peer.IsUnspecified() || peer.IsMulticast() || port < 1024 || port > 65535 {
		return nil, errors.New("invalid RTP binding")
	}
	m, err := newRTPMediaAt(bind, port)
	if err != nil {
		return nil, err
	}
	if err = m.configureRemote(offer); err != nil {
		m.Close()
		return nil, err
	}
	m.mu.Lock()
	if m.remote.Port < 1024 {
		m.mu.Unlock()
		m.Close()
		return nil, errors.New("invalid RTP peer port")
	}
	// Only the authenticated signaling peer may receive audio; ignore SDP IPs.
	m.remote.IP = append(net.IP(nil), peer...)
	m.mu.Unlock()
	return &ClientRTP{m}, nil
}
func (r *ClientRTP) Answer(public net.IP) []byte                  { return r.media.answerSDP(public) }
func (r *ClientRTP) ReadPCM(ctx context.Context) ([]int16, error) { return r.media.ReadPCM(ctx) }
func (r *ClientRTP) WritePCM(pcm []int16) error                   { return r.media.WritePCM(pcm) }
func (r *ClientRTP) Close() error                                 { return r.media.Close() }
