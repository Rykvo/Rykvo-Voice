package hardware

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"time"
)

var errWiFiIKE = errors.New("WIFI_IKE_INVALID")

type ikePart struct {
	kind byte
	data []byte
}
type wifiIKE struct {
	conn                                   *net.UDPConn
	si, sr                                 [8]byte
	ni, nr, initRequest, initResponse, idr []byte
	skd, ai, ar, ei, er, pi, pr            []byte
	prf                                    func() hash.Hash
	macLen                                 int
	marker                                 bool
	id                                     uint32
	host                                   string
	private                                *big.Int
	eap                                    *wifiEAP
	authenticated                          bool
	inID                                   uint32
	inSeen                                 bool
	inRequest, inResponse                  []byte
}

const ikePrime = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF"

func ikePayloads(parts []ikePart) []byte {
	var data []byte
	for i, p := range parts {
		next := byte(0)
		if i+1 < len(parts) {
			next = parts[i+1].kind
		}
		data = append(data, next, 0, byte((len(p.data)+4)>>8), byte(len(p.data)+4))
		data = append(data, p.data...)
	}
	return data
}
func ikeParts(first byte, data []byte) ([]ikePart, error) {
	var out []ikePart
	for first != 0 {
		if len(data) < 4 || len(out) >= 32 {
			return nil, errWiFiIKE
		}
		n := int(binary.BigEndian.Uint16(data[2:4]))
		if n < 4 || n > len(data) {
			return nil, errWiFiIKE
		}
		if data[1]&128 != 0 {
			switch first {
			case 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48:
			default:
				return nil, errWiFiIKE
			}
		}
		out = append(out, ikePart{first, bytes.Clone(data[4:n])})
		first, data = data[0], data[n:]
	}
	if len(data) != 0 {
		return nil, errWiFiIKE
	}
	return out, nil
}
func ikeOne(parts []ikePart, kind byte) ([]byte, error) {
	var result []byte
	for _, p := range parts {
		if p.kind == kind {
			if result != nil {
				return nil, errWiFiIKE
			}
			result = p.data
		}
	}
	if result == nil {
		return nil, errWiFiIKE
	}
	return result, nil
}
func ikeNotify(kind uint16, data []byte) ikePart {
	return ikePart{41, append([]byte{0, 0, byte(kind >> 8), byte(kind)}, data...)}
}
func ikeErrors(parts []ikePart) error {
	for _, p := range parts {
		if p.kind == 41 {
			if len(p.data) < 4 || int(p.data[1])+4 > len(p.data) {
				return errWiFiIKE
			}
			n := binary.BigEndian.Uint16(p.data[2:4])
			if n < 16384 {
				return fmt.Errorf("WIFI_IKE_NOTIFY_%d", n)
			}
		}
	}
	return nil
}
func ikeTransform(kind byte, id uint16, bits int, last bool) []byte {
	next := byte(3)
	if last {
		next = 0
	}
	size := 8
	if bits > 0 {
		size = 12
	}
	b := []byte{next, 0, 0, byte(size), kind, 0, byte(id >> 8), byte(id)}
	if bits > 0 {
		b = append(b, 0x80, 14, byte(bits>>8), byte(bits))
	}
	return b
}
func ikeProposal(number, protocol byte, spi []byte, prf, integ uint16, last bool) []byte {
	ts := ikeTransform(1, 12, 128, false)
	count := byte(3)
	if protocol == 1 {
		ts = append(ts, ikeTransform(2, prf, 0, false)...)
		count = 4
	}
	ts = append(ts, ikeTransform(3, integ, 0, false)...)
	if protocol == 1 {
		ts = append(ts, ikeTransform(4, 14, 0, true)...)
	} else {
		ts = append(ts, ikeTransform(5, 0, 0, true)...)
	}
	next := byte(2)
	if last {
		next = 0
	}
	b := []byte{next, 0, byte((8 + len(spi) + len(ts)) >> 8), byte(8 + len(spi) + len(ts)), number, protocol, byte(len(spi)), count}
	b = append(b, spi...)
	return append(b, ts...)
}
func ikeSelection(data []byte, protocol byte) (map[byte]uint16, []byte, error) {
	if len(data) < 8 || data[0] != 0 || int(binary.BigEndian.Uint16(data[2:4])) != len(data) || data[5] != protocol {
		return nil, nil, errWiFiIKE
	}
	number := data[4]
	spiLen := int(data[6])
	if spiLen+8 > len(data) || protocol == 1 && spiLen != 0 || protocol == 3 && spiLen != 4 {
		return nil, nil, errWiFiIKE
	}
	spi := bytes.Clone(data[8 : 8+spiLen])
	want := int(data[7])
	data = data[8+spiLen:]
	out := map[byte]uint16{}
	for i := 0; i < want; i++ {
		if len(data) < 8 {
			return nil, nil, errWiFiIKE
		}
		n := int(binary.BigEndian.Uint16(data[2:4]))
		if n < 8 || n > len(data) || i == want-1 && data[0] != 0 || i < want-1 && data[0] != 3 {
			return nil, nil, errWiFiIKE
		}
		kind, id := data[4], binary.BigEndian.Uint16(data[6:8])
		if _, ok := out[kind]; ok {
			return nil, nil, errWiFiIKE
		}
		if kind == 1 {
			if id != 12 || n != 12 || !bytes.Equal(data[8:12], []byte{128, 14, 0, 128}) {
				return nil, nil, errors.New("WIFI_IKE_SUITE_UNSUPPORTED")
			}
		} else if n != 8 {
			return nil, nil, errWiFiIKE
		}
		out[kind] = id
		data = data[n:]
	}
	if len(data) != 0 || out[1] != 12 {
		return nil, nil, errWiFiIKE
	}
	if protocol == 1 {
		if want != 4 || out[4] != 14 || !(number == 1 && out[2] == 5 && out[3] == 12 || number == 2 && out[2] == 2 && out[3] == 2) {
			return nil, nil, errors.New("WIFI_IKE_SUITE_UNSUPPORTED")
		}
	} else if number != 1 || want != 3 || out[5] != 0 || out[3] != 12 {
		return nil, nil, errors.New("WIFI_ESP_SUITE_UNSUPPORTED")
	}
	return out, spi, nil
}
func ikeMAC(newHash func() hash.Hash, key []byte, parts ...[]byte) []byte {
	h := hmac.New(newHash, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}
func ikeExpand(newHash func() hash.Hash, key, seed []byte, size int) []byte {
	var out, last []byte
	for n := byte(1); len(out) < size; n++ {
		next := ikeMAC(newHash, key, last, seed, []byte{n})
		clear(last)
		last = next
		out = append(out, next...)
	}
	clear(last)
	return out[:size]
}
func (s *wifiIKE) header(first, exchange byte, id uint32, body []byte) []byte {
	b := make([]byte, 28, len(body)+28)
	copy(b, s.si[:])
	copy(b[8:], s.sr[:])
	b[16], b[17], b[18], b[19] = first, 32, exchange, 8
	binary.BigEndian.PutUint32(b[20:24], id)
	binary.BigEndian.PutUint32(b[24:28], uint32(28+len(body)))
	return append(b, body...)
}
func (s *wifiIKE) exchange(ctx context.Context, packet []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	stop := wifiCancelRead(ctx, s.conn)
	defer stop()
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		deadline := time.Now().Add(time.Duration(2<<attempt) * time.Second)
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		if err := s.conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		wire := packet
		if s.marker {
			wire = append(make([]byte, 4), packet...)
		}
		if _, err := s.conn.Write(wire); err != nil {
			return nil, errors.New("WIFI_NETWORK_WRITE_FAILED")
		}
		for ignored := 0; ignored < 16; ignored++ {
			buf := make([]byte, 65536)
			n, err := s.conn.Read(buf)
			if err != nil {
				if e, ok := err.(net.Error); ok && e.Timeout() {
					break
				}
				return nil, errors.New("WIFI_NETWORK_READ_FAILED")
			}
			buf = buf[:n]
			if s.authenticated {
				handled, err := s.incoming(buf)
				if err != nil {
					return nil, err
				}
				if handled && (len(buf) < 24 || buf[23]&0x20 == 0) {
					continue
				}
			}
			if s.marker {
				if len(buf) < 4 || !bytes.Equal(buf[:4], make([]byte, 4)) {
					continue
				}
				buf = buf[4:]
			}
			if len(buf) < 28 || !bytes.Equal(buf[:8], s.si[:]) || buf[17]>>4 != 2 || buf[18] != packet[18] || buf[19]&0x28 != 0x20 || !bytes.Equal(buf[20:24], packet[20:24]) || int(binary.BigEndian.Uint32(buf[24:28])) != len(buf) {
				continue
			}
			if s.sr != [8]byte{} && !bytes.Equal(buf[8:16], s.sr[:]) {
				continue
			}
			return buf, nil
		}
	}
	return nil, errors.New("WIFI_NETWORK_TIMEOUT")
}
func (s *wifiIKE) seal(exchange byte, id uint32, parts []ikePart) ([]byte, error) {
	return s.sealFlags(exchange, id, parts, 8)
}
func (s *wifiIKE) sealFlags(exchange byte, id uint32, parts []ikePart, flags byte) ([]byte, error) {
	plain := ikePayloads(parts)
	padding := (16 - (len(plain)+1)%16) % 16
	plain = append(plain, make([]byte, padding)...)
	plain = append(plain, byte(padding))
	defer clear(plain)
	iv := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(s.ei)
	if err != nil {
		return nil, err
	}
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, plain)
	length := 4 + 16 + len(encrypted) + s.macLen
	first := byte(0)
	if len(parts) > 0 {
		first = parts[0].kind
	}
	body := []byte{first, 0, byte(length >> 8), byte(length)}
	body = append(body, iv...)
	body = append(body, encrypted...)
	body = append(body, make([]byte, s.macLen)...)
	packet := s.header(46, exchange, id, body)
	packet[19] = flags
	mac := ikeMAC(s.prf, s.ai, packet[:len(packet)-s.macLen])
	copy(packet[len(packet)-s.macLen:], mac[:s.macLen])
	clear(mac)
	return packet, nil
}
func (s *wifiIKE) open(packet []byte) ([]ikePart, error) {
	if len(packet) < 28+4+16+16+s.macLen || packet[16] != 46 || int(binary.BigEndian.Uint16(packet[30:32])) != len(packet)-28 {
		return nil, errWiFiIKE
	}
	end := len(packet) - s.macLen
	mac := ikeMAC(s.prf, s.ar, packet[:end])
	defer clear(mac)
	if !hmac.Equal(mac[:s.macLen], packet[end:]) {
		return nil, errors.New("WIFI_IKE_INTEGRITY_FAILED")
	}
	ciphertext := packet[48:end]
	if len(ciphertext)%16 != 0 {
		return nil, errWiFiIKE
	}
	block, err := aes.NewCipher(s.er)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ciphertext))
	defer clear(plain)
	cipher.NewCBCDecrypter(block, packet[32:48]).CryptBlocks(plain, ciphertext)
	pad := int(plain[len(plain)-1]) + 1
	if pad > len(plain) {
		return nil, errWiFiIKE
	}
	return ikeParts(packet[28], plain[:len(plain)-pad])
}
func (s *wifiIKE) request(ctx context.Context, parts []ikePart) ([]ikePart, error) {
	s.id++
	packet, err := s.seal(35, s.id, parts)
	if err != nil {
		return nil, err
	}
	response, err := s.exchange(ctx, packet)
	if err != nil {
		return nil, err
	}
	decoded, err := s.open(response)
	if err != nil {
		return nil, err
	}
	if err := ikeErrors(decoded); err != nil {
		return nil, fmt.Errorf("WIFI_IKE_AUTH_%d: %w", s.id, err)
	}
	return decoded, nil
}
func publicWiFiIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] != 0 && v4[0] != 127 && v4[0] < 224 && !(v4[0] == 100 && v4[1] >= 64 && v4[1] < 128)
	}
	// Reject synthesized NAT64 loopback/private destinations too.
	if bytes.Equal(ip[:12], []byte{0, 100, 255, 155, 0, 0, 0, 0, 0, 0, 0, 0}) {
		return publicWiFiIP(net.IP(ip[12:]))
	}
	return true
}
func openWiFiIKE(ctx context.Context, sim *wifiSIM) (s *wifiIKE, err error) {
	eap, err := newWiFiEAP(sim)
	if err != nil {
		return nil, err
	}
	s = &wifiIKE{host: sim.id.epdg(), eap: eap}
	current := s
	defer func() {
		if err != nil {
			current.close()
		}
	}()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", s.host)
	if err != nil {
		return nil, errors.New("WIFI_DNS_FAILED")
	}
	for _, ip := range ips {
		if publicWiFiIP(ip) {
			s.conn, err = net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: 500})
			break
		}
	}
	if s.conn == nil {
		return nil, errors.New("WIFI_EPDG_ADDRESS_INVALID")
	}
	if _, err = io.ReadFull(rand.Reader, s.si[:]); err != nil {
		return nil, err
	}
	s.ni = make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, s.ni); err != nil {
		return nil, err
	}
	prime, _ := new(big.Int).SetString(ikePrime, 16)
	seed := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, seed); err != nil {
		return nil, err
	}
	seed[0] |= 128
	s.private = new(big.Int).SetBytes(seed)
	clear(seed)
	pub := new(big.Int).Exp(big.NewInt(2), s.private, prime).FillBytes(make([]byte, 256))
	natHash := func(a *net.UDPAddr) []byte {
		data := append(bytes.Clone(s.si[:]), make([]byte, 8)...)
		data = append(data, a.IP.To4()...)
		data = append(data, byte(a.Port>>8), byte(a.Port))
		sum := sha1.Sum(data)
		return sum[:]
	}
	parts := []ikePart{{33, append(ikeProposal(1, 1, nil, 5, 12, false), ikeProposal(2, 1, nil, 2, 2, true)...)}, {34, append([]byte{0, 14, 0, 0}, pub...)}, {40, s.ni}, ikeNotify(16388, natHash(s.conn.LocalAddr().(*net.UDPAddr))), ikeNotify(16389, natHash(s.conn.RemoteAddr().(*net.UDPAddr)))}
	var responseParts []ikePart
	for attempt := 0; attempt < 3; attempt++ {
		s.initRequest = s.header(parts[0].kind, 34, 0, ikePayloads(parts))
		s.initResponse, err = s.exchange(ctx, s.initRequest)
		if err != nil {
			return nil, err
		}
		responseParts, err = ikeParts(s.initResponse[16], s.initResponse[28:])
		if err != nil {
			return nil, err
		}
		var cookie []byte
		for _, p := range responseParts {
			if p.kind == 41 && len(p.data) >= 5 && p.data[0] == 0 && p.data[1] == 0 && binary.BigEndian.Uint16(p.data[2:4]) == 16390 {
				cookie = p.data[4:]
			}
		}
		if len(cookie) == 0 {
			break
		}
		if len(cookie) > 64 || attempt == 2 {
			return nil, errWiFiIKE
		}
		if parts[0].kind == 41 {
			parts = parts[1:]
		}
		parts = append([]ikePart{ikeNotify(16390, cookie)}, parts...)
	}
	if err = ikeErrors(responseParts); err != nil {
		return nil, err
	}
	copy(s.sr[:], s.initResponse[8:16])
	if s.sr == [8]byte{} {
		return nil, errWiFiIKE
	}
	sa, err := ikeOne(responseParts, 33)
	if err != nil {
		return nil, err
	}
	suite, _, err := ikeSelection(sa, 1)
	if err != nil {
		return nil, err
	}
	s.prf, s.macLen = sha256.New, 16
	if suite[2] == 2 {
		s.prf, s.macLen = sha1.New, 12
	}
	ke, err := ikeOne(responseParts, 34)
	if err != nil || len(ke) != 260 || binary.BigEndian.Uint16(ke[:2]) != 14 {
		return nil, errWiFiIKE
	}
	y := new(big.Int).SetBytes(ke[4:])
	q := new(big.Int).Sub(prime, big.NewInt(1))
	q.Rsh(q, 1)
	if y.Cmp(big.NewInt(2)) < 0 || y.Cmp(new(big.Int).Sub(prime, big.NewInt(2))) > 0 || new(big.Int).Exp(y, q, prime).Cmp(big.NewInt(1)) != 0 {
		return nil, errWiFiIKE
	}
	shared := new(big.Int).Exp(y, s.private, prime).FillBytes(make([]byte, 256))
	defer clear(shared)
	clear(s.private.Bits())
	s.private = nil
	s.nr, err = ikeOne(responseParts, 40)
	if err != nil || len(s.nr) < 16 || len(s.nr) > 256 {
		return nil, errWiFiIKE
	}
	nonces := append(bytes.Clone(s.ni), s.nr...)
	skey := ikeMAC(s.prf, nonces, shared)
	defer clear(skey)
	seed = append(nonces, s.si[:]...)
	seed = append(seed, s.sr[:]...)
	size := s.prf().Size()
	keys := ikeExpand(s.prf, skey, seed, size*5+32)
	defer clear(keys)
	take := func(n int) []byte { out := bytes.Clone(keys[:n]); keys = keys[n:]; return out }
	s.skd = take(size)
	s.ai = take(size)
	s.ar = take(size)
	s.ei = take(16)
	s.er = take(16)
	s.pi = take(size)
	s.pr = take(size)
	// The VM and production hosts may be behind NAT; keep each session's socket.
	remote := *s.conn.RemoteAddr().(*net.UDPAddr)
	local := *s.conn.LocalAddr().(*net.UDPAddr)
	s.conn.Close()
	remote.Port = 4500
	s.conn, err = net.DialUDP("udp4", &local, &remote)
	if err != nil {
		return nil, errors.New("WIFI_NATT_FAILED")
	}
	s.marker = true
	return s, nil
}
func (s *wifiIKE) initialPeer(parts []ikePart) error {
	idr, err := ikeOne(parts, 36)
	if err != nil || len(idr) < 5 {
		return errWiFiIKE
	}
	s.idr = bytes.Clone(idr)
	var auths [][]byte
	var certs []*x509.Certificate
	for _, p := range parts {
		if p.kind == 39 {
			auths = append(auths, p.data)
		}
		if p.kind == 37 {
			if len(p.data) < 2 || p.data[0] != 4 {
				return errors.New("WIFI_CERTIFICATE_INVALID")
			}
			c, err := x509.ParseCertificate(p.data[1:])
			if err != nil {
				return errors.New("WIFI_CERTIFICATE_INVALID")
			}
			certs = append(certs, c)
		}
	}
	if len(auths) == 0 {
		return nil
	} // Deferred EAP authentication must pass final MSK AUTH.
	if len(auths) != 1 || len(certs) == 0 || len(auths[0]) < 5 {
		return errors.New("WIFI_CERTIFICATE_INVALID")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return errors.New("WIFI_CERTIFICATE_INVALID")
	}
	chain := x509.NewCertPool()
	for _, c := range certs[1:] {
		chain.AddCert(c)
	}
	if _, err = certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: chain, DNSName: s.host, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return errors.New("WIFI_CERTIFICATE_INVALID")
	}
	signed := append(bytes.Clone(s.initResponse), s.ni...)
	signed = append(signed, ikeMAC(s.prf, s.pr, idr)...)
	method, sig := auths[0][0], auths[0][4:]
	var digest []byte
	alg := crypto.SHA1
	if method == 1 {
		sum := sha1.Sum(signed)
		digest = sum[:]
	} else if method == 14 {
		if len(sig) < 2 || int(sig[0])+1 >= len(sig) {
			return errWiFiIKE
		}
		var identifier struct {
			Algorithm  asn1.ObjectIdentifier
			Parameters asn1.RawValue
		}
		if rest, err := asn1.Unmarshal(sig[1:1+int(sig[0])], &identifier); err != nil || len(rest) != 0 {
			return errWiFiIKE
		}
		oid := identifier.Algorithm.String()
		switch oid {
		case "1.2.840.113549.1.1.11", "1.2.840.10045.4.3.2":
			alg = crypto.SHA256
			sum := sha256.Sum256(signed)
			digest = sum[:]
		default:
			return errors.New("WIFI_SIGNATURE_UNSUPPORTED")
		}
		sig = sig[1+int(sig[0]):]
	} else {
		return errors.New("WIFI_SIGNATURE_UNSUPPORTED")
	}
	switch key := certs[0].PublicKey.(type) {
	case *rsa.PublicKey:
		err = rsa.VerifyPKCS1v15(key, alg, digest, sig)
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, digest, sig) {
			err = errWiFiIKE
		}
	default:
		err = errWiFiIKE
	}
	if err != nil {
		return errors.New("WIFI_PEER_AUTH_FAILED")
	}
	return nil
}
func (s *wifiIKE) signedAUTH(msk []byte, initiator bool) []byte {
	raw, nonce, key, id := s.initResponse, s.ni, s.pr, s.idr
	if initiator {
		raw, nonce, key, id = s.initRequest, s.nr, s.pi, append([]byte{3, 0, 0, 0}, s.eap.identity...)
	}
	inner := ikeMAC(s.prf, msk, []byte("Key Pad for IKEv2"))
	defer clear(inner)
	return ikeMAC(s.prf, inner, raw, nonce, ikeMAC(s.prf, key, id))
}
func ikeSelectors(kind byte) ikePart {
	body := []byte{2, 0, 0, 0, 7, 0, 0, 16, 0, 0, 255, 255, 0, 0, 0, 0, 255, 255, 255, 255}
	v6 := make([]byte, 40)
	v6[0], v6[3], v6[6], v6[7] = 8, 40, 255, 255
	for i := 24; i < 40; i++ {
		v6[i] = 255
	}
	return ikePart{kind, append(body, v6...)}
}
func (s *wifiIKE) authenticate(ctx context.Context) ([]ikePart, []byte, error) {
	spi := make([]byte, 4)
	if _, err := io.ReadFull(rand.Reader, spi); err != nil {
		return nil, nil, err
	}
	cp := []byte{1, 0, 0, 0}
	for _, kind := range []uint16{1, 8, 3, 10, 20, 21, 7} {
		cp = append(cp, byte(kind>>8), byte(kind), 0, 0)
	}
	offers := ikeProposal(1, 3, spi, 0, 12, true)
	parts := []ikePart{{35, append([]byte{3, 0, 0, 0}, s.eap.identity...)}, {36, append([]byte{2, 0, 0, 0}, []byte("ims")...)}, ikeNotify(16417, nil), ikeNotify(16384, nil), {33, offers}, ikeSelectors(44), ikeSelectors(45), {47, cp}}
	response, err := s.request(ctx, parts)
	if err != nil {
		return nil, nil, err
	}
	if err = s.initialPeer(response); err != nil {
		return nil, nil, err
	}
	for round := 0; round < 12; round++ {
		packet, err := ikeOne(response, 48)
		if err != nil {
			return nil, nil, err
		}
		reply, err := s.eap.handle(ctx, packet)
		if err != nil {
			return nil, nil, err
		}
		if s.eap.complete {
			break
		}
		response, err = s.request(ctx, []ikePart{{48, reply}})
		clear(reply)
		if err != nil {
			return nil, nil, err
		}
	}
	msk, err := s.eap.masterKey()
	if err != nil {
		return nil, nil, err
	}
	defer clear(msk)
	auth := s.signedAUTH(msk, true)
	defer clear(auth)
	response, err = s.request(ctx, []ikePart{{39, append([]byte{2, 0, 0, 0}, auth...)}})
	if err != nil {
		return nil, nil, err
	}
	peer, err := ikeOne(response, 39)
	if err != nil || len(peer) < 5 || peer[0] != 2 {
		return nil, nil, errors.New("WIFI_PEER_AUTH_FAILED")
	}
	if !hmac.Equal(peer[4:], s.signedAUTH(msk, false)) {
		return nil, nil, errors.New("WIFI_PEER_AUTH_FAILED")
	}
	s.authenticated = true
	return response, spi, nil
}
func (s *wifiIKE) close() {
	if s == nil {
		return
	}
	if s.conn != nil {
		if s.authenticated {
			clean, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			s.id++
			if packet, err := s.seal(37, s.id, []ikePart{{42, []byte{1, 0, 0, 0}}}); err == nil {
				_, _ = s.exchange(clean, packet)
			}
			cancel()
			s.authenticated = false
		}
		s.conn.Close()
	}
	for _, b := range [][]byte{s.inRequest, s.inResponse, s.skd, s.ai, s.ar, s.ei, s.er, s.pi, s.pr, s.ni, s.nr, s.idr} {
		clear(b)
	}
	if s.private != nil {
		clear(s.private.Bits())
	}
	if s.eap != nil {
		s.eap.close()
	}
}

func (s *wifiIKE) incoming(wire []byte) (bool, error) {
	if len(wire) < 4 || !bytes.Equal(wire[:4], []byte{0, 0, 0, 0}) {
		return false, nil
	}
	p := wire[4:]
	if !s.authenticated || len(p) < 28 || !bytes.Equal(p[:8], s.si[:]) || !bytes.Equal(p[8:16], s.sr[:]) || p[17] != 32 || p[19]&0x28 != 0 || int(binary.BigEndian.Uint32(p[24:28])) != len(p) {
		return true, nil
	}
	id := binary.BigEndian.Uint32(p[20:24])
	if s.inSeen && id == s.inID && bytes.Equal(p, s.inRequest) {
		_, err := s.conn.Write(s.inResponse)
		return true, err
	}
	if !s.inSeen && id != 0 || s.inSeen && (s.inID == ^uint32(0) || id != s.inID+1) {
		return true, nil
	}
	parts, err := s.open(p)
	if err != nil {
		return true, nil
	}
	var reply []ikePart
	var terminal error
	switch p[18] {
	case 37:
		for _, v := range parts {
			if v.kind == 42 {
				if len(v.data) < 4 {
					return true, errWiFiIKE
				}
				size, count := int(v.data[1]), int(binary.BigEndian.Uint16(v.data[2:4]))
				if len(v.data) != 4+size*count {
					return true, errWiFiIKE
				}
				switch v.data[0] {
				case 1:
					if size != 0 || count != 0 {
						return true, errWiFiIKE
					}
				case 3:
					if size != 4 || count == 0 || count > 16 {
						return true, errWiFiIKE
					}
				default:
					return true, errWiFiIKE
				}
				terminal = errors.New("WIFI_TUNNEL_CLOSED")
			} else if v.kind != 41 {
				return true, errWiFiIKE
			}
		}
	case 36:
		reply = []ikePart{ikeNotify(35, nil)}
		terminal = errors.New("WIFI_REKEY_REQUIRED")
	default:
		return true, nil
	}
	response, err := s.sealFlags(p[18], id, reply, 0x28)
	if err != nil {
		return true, err
	}
	wire = append([]byte{0, 0, 0, 0}, response...)
	if _, err = s.conn.Write(wire); err != nil {
		return true, errors.New("WIFI_NETWORK_WRITE_FAILED")
	}
	s.inSeen, s.inID = true, id
	clear(s.inRequest)
	clear(s.inResponse)
	s.inRequest, s.inResponse = bytes.Clone(p), wire
	return true, terminal
}
func (s *wifiIKE) alive(ctx context.Context) error {
	if s.id == ^uint32(0) {
		return errors.New("WIFI_REKEY_REQUIRED")
	}
	s.id++
	packet, err := s.seal(37, s.id, nil)
	if err != nil {
		return err
	}
	response, err := s.exchange(ctx, packet)
	if err != nil {
		return err
	}
	parts, err := s.open(response)
	if err != nil {
		return err
	}
	if len(parts) != 0 {
		return errWiFiIKE
	}
	return nil
}
