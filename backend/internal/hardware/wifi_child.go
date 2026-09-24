package hardware

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"net"
	"time"
)

var errWiFiESP = errors.New("WIFI_ESP_INVALID")

type wifiSelector struct {
	proto       byte
	first, last uint16
	start, end  net.IP
}
type wifiChild struct {
	ike                                *wifiIKE
	spiIn, spiOut, sendSeq, receiveSeq uint32
	replay                             uint64
	encIn, encOut, authIn, authOut     []byte
	mac                                func() hash.Hash
	macLen                             int
	local, pcscf                       []net.IP
	tsi, tsr                           []wifiSelector
}

func wifiSelectors(data []byte) ([]wifiSelector, error) {
	if len(data) < 4 || data[0] == 0 || data[0] > 16 {
		return nil, errWiFiESP
	}
	count := int(data[0])
	data = data[4:]
	out := make([]wifiSelector, 0, count)
	for i := 0; i < count; i++ {
		if len(data) < 8 {
			return nil, errWiFiESP
		}
		n := int(binary.BigEndian.Uint16(data[2:4]))
		size := 4
		if data[0] == 8 {
			size = 16
		} else if data[0] != 7 {
			return nil, errWiFiESP
		}
		if n != 8+2*size || n > len(data) {
			return nil, errWiFiESP
		}
		v := wifiSelector{data[1], binary.BigEndian.Uint16(data[4:6]), binary.BigEndian.Uint16(data[6:8]), bytes.Clone(data[8 : 8+size]), bytes.Clone(data[8+size : n])}
		if v.first > v.last || bytes.Compare(v.start, v.end) > 0 {
			return nil, errWiFiESP
		}
		out = append(out, v)
		data = data[n:]
	}
	if len(data) != 0 {
		return nil, errWiFiESP
	}
	return out, nil
}
func wifiTrafficAllowed(ranges []wifiSelector, ip net.IP, proto byte, port uint16) bool {
	for _, r := range ranges {
		v := ip.To16()
		if len(r.start) == 4 {
			v = ip.To4()
		}
		if v != nil && (r.proto == 0 || r.proto == proto) && port >= r.first && port <= r.last && bytes.Compare(v, r.start) >= 0 && bytes.Compare(v, r.end) <= 0 {
			return true
		}
	}
	return false
}
func wifiConfig(data []byte) (local, peers []net.IP, err error) {
	if len(data) < 4 || data[0] != 2 {
		return nil, nil, errWiFiESP
	}
	data = data[4:]
	seen := map[uint16]bool{}
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, nil, errWiFiESP
		}
		kind := binary.BigEndian.Uint16(data[:2])
		n := int(binary.BigEndian.Uint16(data[2:4]))
		data = data[4:]
		if n > len(data) {
			return nil, nil, errWiFiESP
		}
		v := data[:n]
		data = data[n:]
		switch kind {
		case 1, 8, 20, 21:
			size := 4
			if kind == 8 || kind == 21 {
				size = 16
			}
			want := size
			if kind == 8 {
				want++
			}
			if n != want || kind == 8 && v[16] > 128 {
				return nil, nil, errWiFiESP
			}
			ip := net.IP(bytes.Clone(v[:size]))
			if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				return nil, nil, errWiFiESP
			}
			if kind == 1 || kind == 8 {
				if seen[kind] {
					return nil, nil, errWiFiESP
				}
				seen[kind] = true
				local = append(local, ip)
			} else {
				if len(peers) >= 16 {
					return nil, nil, errWiFiESP
				}
				peers = append(peers, ip)
			}
		}
	}
	if len(local) == 0 || len(peers) == 0 {
		return nil, nil, errors.New("WIFI_IMS_ADDRESS_MISSING")
	}
	return
}
func newWiFiChild(s *wifiIKE, parts []ikePart, spi []byte) (*wifiChild, error) {
	if !s.authenticated || len(spi) != 4 || binary.BigEndian.Uint32(spi) == 0 {
		return nil, errWiFiESP
	}
	sa, err := ikeOne(parts, 33)
	if err != nil {
		return nil, err
	}
	suite, peerSPI, err := ikeSelection(sa, 3)
	if err != nil || len(peerSPI) != 4 {
		return nil, errWiFiESP
	}
	c := &wifiChild{ike: s, spiIn: binary.BigEndian.Uint32(spi), spiOut: binary.BigEndian.Uint32(peerSPI), mac: sha256.New, macLen: 16}
	if c.spiOut == 0 {
		return nil, errWiFiESP
	}
	if suite[3] == 2 {
		c.mac, c.macLen = sha1.New, 12
	}
	config, err := ikeOne(parts, 47)
	if err != nil {
		return nil, err
	}
	c.local, c.pcscf, err = wifiConfig(config)
	if err != nil {
		return nil, err
	}
	for kind, dest := range map[byte]*[]wifiSelector{44: &c.tsi, 45: &c.tsr} {
		v, err := ikeOne(parts, kind)
		if err != nil {
			return nil, err
		}
		*dest, err = wifiSelectors(v)
		if err != nil {
			return nil, err
		}
	}
	size := c.mac().Size()
	keys := ikeExpand(s.prf, s.skd, append(bytes.Clone(s.ni), s.nr...), (16+size)*2)
	defer clear(keys)
	c.encOut = bytes.Clone(keys[:16])
	c.authOut = bytes.Clone(keys[16 : 16+size])
	c.encIn = bytes.Clone(keys[16+size : 32+size])
	c.authIn = bytes.Clone(keys[32+size:])
	return c, nil
}
func (c *wifiChild) close() {
	for _, v := range [][]byte{c.encIn, c.encOut, c.authIn, c.authOut} {
		clear(v)
	}
	c.ike = nil
}
func (c *wifiChild) seal(plain []byte, next byte) ([]byte, error) {
	if c.sendSeq == ^uint32(0) || len(plain) > 65500 {
		return nil, errWiFiESP
	}
	c.sendSeq++
	pad := (16 - (len(plain)+2)%16) % 16
	body := bytes.Clone(plain)
	for i := 1; i <= pad; i++ {
		body = append(body, byte(i))
	}
	body = append(body, byte(pad), next)
	defer clear(body)
	packet := make([]byte, 24+len(body))
	binary.BigEndian.PutUint32(packet, c.spiOut)
	binary.BigEndian.PutUint32(packet[4:], c.sendSeq)
	if _, err := io.ReadFull(rand.Reader, packet[8:24]); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(c.encOut)
	if err != nil {
		return nil, err
	}
	cipher.NewCBCEncrypter(block, packet[8:24]).CryptBlocks(packet[24:], body)
	mac := ikeMAC(c.mac, c.authOut, packet)
	defer clear(mac)
	return append(packet, mac[:c.macLen]...), nil
}
func (c *wifiChild) open(packet []byte) ([]byte, byte, error) {
	if len(packet) < 24+16+c.macLen || binary.BigEndian.Uint32(packet) != c.spiIn {
		return nil, 0, errWiFiESP
	}
	seq := binary.BigEndian.Uint32(packet[4:])
	if seq == 0 || seq <= c.receiveSeq && (c.receiveSeq-seq >= 64 || c.replay&(uint64(1)<<(c.receiveSeq-seq)) != 0) {
		return nil, 0, errWiFiESP
	}
	end := len(packet) - c.macLen
	mac := ikeMAC(c.mac, c.authIn, packet[:end])
	defer clear(mac)
	if !hmac.Equal(mac[:c.macLen], packet[end:]) || (end-24)%16 != 0 {
		return nil, 0, errWiFiESP
	}
	block, err := aes.NewCipher(c.encIn)
	if err != nil {
		return nil, 0, err
	}
	body := make([]byte, end-24)
	cipher.NewCBCDecrypter(block, packet[8:24]).CryptBlocks(body, packet[24:end])
	pad := int(body[len(body)-2])
	if pad+2 > len(body) {
		clear(body)
		return nil, 0, errWiFiESP
	}
	for i := 0; i < pad; i++ {
		if body[len(body)-2-pad+i] != byte(i+1) {
			clear(body)
			return nil, 0, errWiFiESP
		}
	}
	if seq > c.receiveSeq {
		c.replay = c.replay<<(seq-c.receiveSeq) | 1
		c.receiveSeq = seq
	} else {
		c.replay |= uint64(1) << (c.receiveSeq - seq)
	}
	next := body[len(body)-1]
	return body[:len(body)-pad-2], next, nil
}
func wifiChecksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) > 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}
func wifiPseudo(src, dst net.IP, proto byte, n int) []byte {
	if v := src.To4(); v != nil {
		p := append(bytes.Clone(v), dst.To4()...)
		return append(p, 0, proto, byte(n>>8), byte(n))
	}
	p := append(bytes.Clone(src.To16()), dst.To16()...)
	return append(p, byte(n>>24), byte(n>>16), byte(n>>8), byte(n), 0, 0, 0, proto)
}
func wifiUDP(src, dst net.IP, source, target uint16, data []byte) ([]byte, byte, error) {
	if len(data) > 60000 || (src.To4() == nil) != (dst.To4() == nil) {
		return nil, 0, errWiFiESP
	}
	udp := make([]byte, 8, len(data)+8)
	binary.BigEndian.PutUint16(udp, source)
	binary.BigEndian.PutUint16(udp[2:], target)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(data)))
	udp = append(udp, data...)
	checksum := wifiChecksum(append(wifiPseudo(src, dst, 17, len(udp)), udp...))
	if checksum == 0 {
		checksum = 65535
	}
	binary.BigEndian.PutUint16(udp[6:], checksum)
	return wifiIP(src, dst, 17, udp)
}

func wifiIP(src, dst net.IP, proto byte, payload []byte) ([]byte, byte, error) {
	if src == nil || dst == nil || len(payload) > 60000 || (src.To4() == nil) != (dst.To4() == nil) {
		return nil, 0, errWiFiESP
	}
	if src.To4() != nil {
		ip := make([]byte, 20)
		ip[0], ip[8], ip[9] = 0x45, 64, proto
		binary.BigEndian.PutUint16(ip[2:], uint16(20+len(payload)))
		copy(ip[12:16], src.To4())
		copy(ip[16:], dst.To4())
		binary.BigEndian.PutUint16(ip[10:], wifiChecksum(ip))
		return append(ip, payload...), 4, nil
	}
	ip := make([]byte, 40)
	ip[0], ip[6], ip[7] = 0x60, proto, 64
	binary.BigEndian.PutUint16(ip[4:], uint16(len(payload)))
	copy(ip[8:24], src.To16())
	copy(ip[24:], dst.To16())
	return append(ip, payload...), 41, nil
}
func wifiParseIP(packet []byte, next byte) (src, dst net.IP, proto byte, payload []byte, err error) {
	if next == 4 {
		if len(packet) < 20 || packet[0]>>4 != 4 {
			return nil, nil, 0, nil, errWiFiESP
		}
		n := int(packet[0]&15) * 4
		if n < 20 || n > len(packet) || int(binary.BigEndian.Uint16(packet[2:])) != len(packet) || binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 || wifiChecksum(packet[:n]) != 0 {
			return nil, nil, 0, nil, errWiFiESP
		}
		return packet[12:16], packet[16:20], packet[9], packet[n:], nil
	}
	if next == 41 && len(packet) >= 40 && packet[0]>>4 == 6 && int(binary.BigEndian.Uint16(packet[4:]))+40 == len(packet) {
		return packet[8:24], packet[24:40], packet[6], packet[40:], nil
	}
	return nil, nil, 0, nil, errWiFiESP
}
func wifiReadUDP(packet []byte, next byte, local, remote net.IP, port, peer uint16) ([]byte, error) {
	src, dst, proto, udp, err := wifiParseIP(packet, next)
	if err != nil || proto != 17 || len(udp) < 8 || binary.BigEndian.Uint16(udp) == 0 || !src.Equal(remote) || !dst.Equal(local) || (peer != 0 && binary.BigEndian.Uint16(udp) != peer) || binary.BigEndian.Uint16(udp[2:]) != port || int(binary.BigEndian.Uint16(udp[4:])) != len(udp) {
		return nil, errWiFiESP
	}
	if binary.BigEndian.Uint16(udp[6:]) == 0 {
		if next == 41 {
			return nil, errWiFiESP
		}
	} else if wifiChecksum(append(wifiPseudo(src, dst, 17, len(udp)), udp...)) != 0 {
		return nil, errWiFiESP
	}
	return bytes.Clone(udp[8:]), nil
}

func (c *wifiChild) sendUDP(local, peer net.IP, port, target uint16, body []byte, protection *wifiChild) error {
	proto, sp, dp := byte(17), port, target
	if protection != nil {
		proto, sp, dp = 50, 0, 0
	}
	if !wifiTrafficAllowed(c.tsi, local, proto, sp) || !wifiTrafficAllowed(c.tsr, peer, proto, dp) {
		return errors.New("WIFI_TRAFFIC_REJECTED")
	}
	ip, kind, err := wifiUDP(local, peer, port, target, body)
	if err != nil {
		return err
	}
	defer clear(ip)
	if protection != nil {
		_, _, _, udp, err := wifiParseIP(ip, kind)
		if err != nil {
			return err
		}
		esp, err := protection.seal(udp, 17)
		if err != nil {
			return err
		}
		defer clear(esp)
		ip, kind, err = wifiIP(local, peer, 50, esp)
		if err != nil {
			return err
		}
		defer clear(ip)
	}
	wire, err := c.seal(ip, kind)
	if err != nil {
		return err
	}
	if _, err = c.ike.conn.Write(wire); err != nil {
		return errors.New("WIFI_NETWORK_WRITE_FAILED")
	}
	return nil
}
func (c *wifiChild) receiveUDP(wire []byte, local, peer net.IP, port, target uint16, protection *wifiChild) ([]byte, error) {
	plain, kind, err := c.open(wire)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	if protection != nil {
		src, dst, proto, esp, err := wifiParseIP(plain, kind)
		if err != nil || !src.Equal(peer) || !dst.Equal(local) || proto != 50 {
			return nil, errWiFiESP
		}
		udp, proto, err := protection.open(esp)
		if err != nil {
			return nil, err
		}
		defer clear(udp)
		if proto != 17 {
			return nil, errWiFiESP
		}
		plain, kind, err = wifiIP(peer, local, 17, udp)
		if err != nil {
			return nil, err
		}
		defer clear(plain)
	}
	return wifiReadUDP(plain, kind, local, peer, port, target)
}

// One owner reads both SIP and IKE; cancellation never races a later cleanup.
func wifiCancelRead(ctx context.Context, conn *net.UDPConn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); close(done) })
	return func() {
		if !stop() {
			<-done
		}
	}
}
func (c *wifiChild) udpExchange(ctx context.Context, local, peer net.IP, port, target, replyPort uint16, body []byte, protection *wifiChild, match func([]byte) bool) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c.ike == nil || c.ike.conn == nil {
		return nil, errWiFiESP
	}
	stop := wifiCancelRead(ctx, c.ike.conn)
	defer stop()
	replySource := target
	if protection != nil {
		replySource = 0
	}
	for attempt := 0; attempt < 4; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		deadline := time.Now().Add(time.Duration(1<<attempt) * time.Second)
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		c.ike.conn.SetDeadline(deadline)
		if err := c.sendUDP(local, peer, port, target, body, protection); err != nil {
			return nil, err
		}
		buf := make([]byte, 65536)
		for ignored := 0; ignored < 64; ignored++ {
			n, err := c.ike.conn.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if e, ok := err.(net.Error); ok && e.Timeout() {
					break
				}
				return nil, errors.New("WIFI_NETWORK_READ_FAILED")
			}
			handled, err := c.ike.incoming(buf[:n])
			if err != nil {
				return nil, err
			}
			if handled {
				continue
			}
			data, err := c.receiveUDP(buf[:n], local, peer, replyPort, replySource, protection)
			if err == nil && match(data) {
				return data, nil
			}
			clear(data)
		}
	}
	return nil, errors.New("WIFI_IMS_TIMEOUT")
}
