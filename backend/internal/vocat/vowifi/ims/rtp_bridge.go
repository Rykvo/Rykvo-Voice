package ims

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
)

// ClientRTP negotiates APP audio independently from the carrier IMS codec.
type ClientRTP struct{ media *rtpMedia }

// Accept a session refresh only when the existing codec and endpoint remain offered.
// Do not reset learned NAT ports or the jitter buffer during an unchanged re-INVITE.
func (r *ClientRTP) RefreshOffer(offer []byte) error {
	ip, port, formats, mappings, err := parseAudioSDP(offer)
	if err != nil || ip == nil || ip.IsUnspecified() || port < 1024 {
		return errors.New("invalid refresh audio")
	}
	for _, raw := range strings.Split(strings.ReplaceAll(string(offer), "\r\n", "\n"), "\n") {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "a=sendonly", "a=recvonly", "a=inactive":
			return errors.New("unsupported media direction")
		}
	}
	m := r.media
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.remote == nil || port != m.signaledPort {
		return errors.New("changed refresh endpoint")
	}
	for _, format := range formats {
		if format != strconv.Itoa(int(m.payloadType)) {
			continue
		}
		mapping := strings.ToUpper(mappings[int(m.payloadType)])
		if m.codec == "OPUS" && mapping == "OPUS/48000/2" {
			if rate, ok := opusRemoteOptions(offer, int(m.payloadType)); ok && rate == m.opusBitrate {
				return nil
			}
			return errors.New("changed refresh Opus parameters")
		}
		if mapping == "" && (m.payloadType == 0 || m.payloadType == 8) {
			return nil
		}
		if m.codec != "OPUS" && (mapping == m.codec+"/8000" || mapping == m.codec+"/8000/1") {
			return nil
		}
	}
	return errors.New("changed refresh codec")
}

func OpenClientRTP(bind net.IP, port int, peer net.IP, offer []byte, allowOpus bool) (*ClientRTP, error) {
	r, err := NewClientRTPOffer(bind, port, allowOpus)
	if err != nil {
		return nil, err
	}
	if err = r.SetAnswer(peer, offer); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func NewClientRTPOffer(bind net.IP, port int, allowOpus bool) (*ClientRTP, error) {
	if bind == nil || bind.IsUnspecified() || port < 1024 || port > 65535 {
		return nil, errors.New("invalid RTP binding")
	}
	m, err := newRTPMediaAt(bind, port)
	if err != nil {
		return nil, err
	}
	m.allowOpus = allowOpus
	return &ClientRTP{m}, nil
}

func (r *ClientRTP) SetAnswer(peer net.IP, answer []byte) error {
	if peer == nil || peer.IsUnspecified() || peer.IsMulticast() {
		return errors.New("invalid RTP peer")
	}
	m := r.media
	if err := m.configureRemote(answer); err != nil {
		return err
	}
	m.mu.Lock()
	if m.remote.Port < 1024 {
		m.mu.Unlock()
		return errors.New("invalid RTP peer port")
	}
	// Only the authenticated signaling peer may receive audio; ignore SDP IPs.
	m.remote.IP = append(net.IP(nil), peer...)
	m.mu.Unlock()
	return nil
}

// Answers to our offer must use one of its advertised payload mappings.
func (r *ClientRTP) AcceptAnswer(peer net.IP, answer []byte) error {
	_, _, formats, mappings, err := parseAudioSDP(answer)
	if err != nil {
		return err
	}
	for _, format := range formats {
		if format == strconv.Itoa(opusPayload) && r.media.allowOpus && opusAvailable() && strings.EqualFold(mappings[opusPayload], "opus/48000/2") {
			continue
		}
		if format != "0" && format != "8" {
			return errors.New("unoffered RTP payload")
		}
	}
	return r.SetAnswer(peer, answer)
}
func (r *ClientRTP) Offer(public net.IP) []byte                   { return r.media.offerSDP(public) }
func (r *ClientRTP) Answer(public net.IP) []byte                  { return r.media.answerSDP(public) }
func (r *ClientRTP) ReadPCM(ctx context.Context) ([]int16, error) { return r.media.ReadPCM(ctx) }
func (r *ClientRTP) WritePCM(pcm []int16) error                   { return r.media.WritePCM(pcm) }
func (r *ClientRTP) Close() error                                 { return r.media.Close() }
func (r *ClientRTP) ReceiveStats() RTPReceiveStats {
	return r.media.ReceiveStats()
}

func (media *rtpMedia) ReceiveStats() RTPReceiveStats {
	s := media.jitter.snapshot()
	s.Codec = media.Codec()
	return s
}
