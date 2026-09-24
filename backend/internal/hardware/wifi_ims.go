package hardware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type wifiSIP struct {
	code    int
	headers map[string][]string
	body    []byte
}

func parseWiFiSIP(data []byte) (wifiSIP, error) {
	bad := errors.New("WIFI_IMS_INVALID")
	if len(data) > 65536 {
		return wifiSIP{}, bad
	}
	cut := bytes.Index(data, []byte("\r\n\r\n"))
	if cut < 0 {
		return wifiSIP{}, bad
	}
	lines := strings.Split(string(data[:cut]), "\r\n")
	status := strings.SplitN(lines[0], " ", 3)
	if len(status) < 3 || status[0] != "SIP/2.0" {
		return wifiSIP{}, bad
	}
	code, err := strconv.Atoi(status[1])
	if err != nil || code < 100 || code > 699 {
		return wifiSIP{}, bad
	}
	s := wifiSIP{code: code, headers: map[string][]string{}, body: bytes.Clone(data[cut+4:])}
	last := ""
	for _, line := range lines[1:] {
		if strings.ContainsAny(line, "\r\n\x00") {
			return wifiSIP{}, bad
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if last == "" {
				return wifiSIP{}, bad
			}
			v := s.headers[last]
			v[len(v)-1] += " " + strings.TrimSpace(line)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key = strings.ToLower(strings.TrimSpace(key))
		if !ok || key == "" || len(s.headers) >= 64 {
			return wifiSIP{}, bad
		}
		switch key {
		case "v":
			key = "via"
		case "i":
			key = "call-id"
		case "l":
			key = "content-length"
		case "m":
			key = "contact"
		}
		s.headers[key] = append(s.headers[key], strings.TrimSpace(value))
		last = key
	}
	lengths := s.headers["content-length"]
	if len(lengths) != 1 {
		return wifiSIP{}, bad
	}
	n, err := strconv.Atoi(lengths[0])
	if err != nil || n != len(s.body) {
		return wifiSIP{}, bad
	}
	for _, key := range []string{"call-id", "cseq", "from", "to"} {
		if len(s.headers[key]) != 1 {
			return wifiSIP{}, bad
		}
	}
	return s, nil
}
func wifiToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(b[:])
}

type wifiIMS struct {
	transport        string
	tcp              *wifiTCP
	replyConn        net.Conn
	serverProtection *wifiChild
	peerClientPort   uint16
	sipObserved      bool

	child                                               *wifiChild
	sim                                                 *wifiSIM
	local, peer                                         net.IP
	identity, domain, call, tag, contact, offer, verify string
	spiClient, spiServer                                uint32
	target                                              uint16
	protection                                          *wifiChild
	credentials                                         map[string]string
	res                                                 []byte
	nc, cseq                                            uint32
	registered                                          bool
	renewAt                                             time.Time
	retryAfter                                          time.Time
}

func wifiParameters(value string, separator byte) (map[string]string, error) {
	bad := errors.New("WIFI_IMS_AUTH_INVALID")
	if len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, bad
	}
	out := map[string]string{}
	for value != "" {
		value = strings.TrimSpace(value)
		if value == "" {
			break
		}
		if len(out) >= 32 {
			return nil, bad
		}
		i := strings.IndexByte(value, '=')
		if i < 1 {
			return nil, bad
		}
		key := strings.ToLower(strings.TrimSpace(value[:i]))
		if strings.ContainsAny(key, " \t,;\"") {
			return nil, bad
		}
		if _, ok := out[key]; ok {
			return nil, bad
		}
		value = strings.TrimSpace(value[i+1:])
		var v string
		if strings.HasPrefix(value, "\"") {
			var b strings.Builder
			closed := false
			index := 1
			for ; index < len(value); index++ {
				ch := value[index]
				if ch == '\\' {
					index++
					if index == len(value) {
						return nil, bad
					}
					b.WriteByte(value[index])
				} else if ch == '"' {
					index++
					closed = true
					break
				} else {
					b.WriteByte(ch)
				}
			}
			if !closed {
				return nil, bad
			}
			v = b.String()
			value = strings.TrimSpace(value[index:])
			if value != "" && value[0] != separator {
				return nil, bad
			}
		} else {
			index := strings.IndexByte(value, separator)
			if index < 0 {
				index = len(value)
			}
			v = strings.TrimSpace(value[:index])
			value = value[index:]
		}
		out[key] = v
		if value != "" {
			value = value[1:]
			if strings.TrimSpace(value) == "" {
				return nil, bad
			}
		}
	}
	return out, nil
}
func wifiQuoted(s string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s) + "\""
}
func wifiMD5(parts ...[]byte) string {
	h := md5.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func wifiDigest(user, realm, nonce, uri, method, qop, cnonce string, nc uint32, res []byte) string {
	a1 := wifiMD5([]byte(user+":"+realm+":"), res)
	a2 := wifiMD5([]byte(method + ":" + uri))
	input := a1 + ":" + nonce + ":"
	if qop != "" {
		input += fmt.Sprintf("%08x:%s:%s:", nc, cnonce, qop)
	}
	return wifiMD5([]byte(input + a2))
}
func wifiRandomSPI(except uint32) uint32 {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		n := binary.BigEndian.Uint32(b[:])
		if n >= 256 && n != except {
			return n
		}
	}
}
func newWiFiIMS(c *wifiChild, sim *wifiSIM) (*wifiIMS, error) {
	transport := sim.profile.Transport
	if sim.preferredTransport != "" {
		transport = sim.preferredTransport
	}
	if transport == "" {
		transport = "tcp"
	}
	return newWiFiIMSAt(c, sim, transport, c.pcscf)
}
func newWiFiIMSAt(c *wifiChild, sim *wifiSIM, transport string, peers []net.IP) (*wifiIMS, error) {
	s := &wifiIMS{transport: transport, child: c, sim: sim, target: 5060, call: wifiToken(), tag: wifiToken()}
	proto := byte(17)
	if transport == "tcp" {
		proto = 6
	}
	for _, p := range peers {
		for _, l := range c.local {
			if (p.To4() == nil) == (l.To4() == nil) && wifiTrafficAllowed(c.tsi, l, proto, 49160) && wifiTrafficAllowed(c.tsr, p, proto, 5060) {
				s.local, s.peer = l, p
				break
			}
		}
		if s.local != nil {
			break
		}
	}
	if s.local == nil {
		return nil, errors.New("WIFI_IMS_ADDRESS_MISSING")
	}
	s.domain = fmt.Sprintf("ims.mnc%03s.mcc%s.3gppnetwork.org", sim.id.MNC, sim.id.MCC)
	s.identity = sim.id.IMSI + "@" + s.domain
	s.spiClient = wifiRandomSPI(0)
	s.spiServer = wifiRandomSPI(s.spiClient)
	s.offer = fmt.Sprintf("ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;spi-c=%d;spi-s=%d;port-c=49160;port-s=49162", s.spiClient, s.spiServer)
	s.contact = "<sip:" + sim.id.IMSI + "@" + net.JoinHostPort(s.local.String(), "49162") + ">;+g.3gpp.smsip;+g.3gpp.icsi-ref=\"urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel\""
	if sim.profile.Contact == "gsma" {
		s.contact = "<sip:" + net.JoinHostPort(s.local.String(), "49162") + ">;+g.3gpp.icsi-ref=\"urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel\""
	}
	seed := sha256.Sum256([]byte("rykvo-voice-sip-instance:" + sim.id.IMSI))
	seed[6] = (seed[6] & 15) | 0x50
	seed[8] = (seed[8] & 63) | 0x80
	instance := fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", seed[:4], seed[4:6], seed[6:8], seed[8:10], seed[10:16])
	s.contact += ";+sip.instance=\"<" + instance + ">\""
	for _, tag := range sim.profile.Tags {
		if !strings.Contains(s.contact, tag) {
			s.contact += ";" + tag
		}
	}
	return s, nil
}
func (s *wifiIMS) authorize(ctx context.Context, r wifiSIP) error {
	if len(r.headers["www-authenticate"]) != 1 || len(r.headers["security-server"]) < 1 {
		return fmt.Errorf("WIFI_IMS_AUTH_HEADERS_%d_%d", len(r.headers["www-authenticate"]), len(r.headers["security-server"]))
	}
	raw := r.headers["www-authenticate"][0]
	scheme, parameters, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(scheme, "Digest") {
		return errors.New("WIFI_IMS_AUTH_UNSUPPORTED")
	}
	auth, err := wifiParameters(parameters, ',')
	if err != nil {
		return errors.New("WIFI_IMS_DIGEST_PARAMETERS_INVALID")
	}
	if !strings.EqualFold(auth["algorithm"], "AKAv1-MD5") || auth["realm"] == "" || auth["nonce"] == "" {
		return errors.New("WIFI_IMS_AUTH_UNSUPPORTED")
	}
	if auth["qop"] != "" {
		offered := false
		for _, q := range strings.Split(auth["qop"], ",") {
			offered = offered || strings.EqualFold(strings.TrimSpace(q), "auth")
		}
		if !offered {
			return errors.New("WIFI_IMS_AUTH_UNSUPPORTED")
		}
		auth["qop"] = "auth"
	}
	nonce, err := base64.StdEncoding.DecodeString(auth["nonce"])
	if err != nil {
		nonce, err = base64.RawStdEncoding.DecodeString(auth["nonce"])
	}
	if err != nil || len(nonce) < 32 || len(nonce) > 256 {
		return errors.New("WIFI_IMS_NONCE_INVALID")
	}
	defer clear(nonce)
	sec, raw, err := wifiSecuritySelection(r.headers["security-server"])
	if err != nil {
		return err
	}
	spi, err := strconv.ParseUint(sec["spi-s"], 10, 32)
	if err != nil || spi < 256 {
		return errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	client, err := strconv.ParseUint(sec["spi-c"], 10, 32)
	if err != nil || client < 256 || client == spi {
		return errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	port, err := strconv.ParseUint(sec["port-s"], 10, 16)
	if err != nil || port == 0 {
		return errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	cp, err := strconv.ParseUint(sec["port-c"], 10, 16)
	if err != nil || cp == 0 || cp == port {
		return errors.New("WIFI_IMS_SECURITY_INVALID")
	}
	keys, err := s.sim.authenticate(ctx, nonce[:16], nonce[16:32])
	if err != nil {
		return err
	}
	defer keys.clear()
	if len(keys.AUTS) > 0 {
		return errors.New("WIFI_IMS_AKA_RESYNC_REQUIRED")
	}
	if len(keys.RES) == 0 || len(keys.CK) != 16 || len(keys.IK) != 16 {
		return errors.New("WIFI_IMS_AKA_RESULT_INVALID")
	}
	if s.tcp != nil {
		s.tcp.close()
		s.tcp = nil
	}
	if s.serverProtection != nil {
		s.serverProtection.close()
		s.serverProtection = nil
	}
	if s.protection != nil {
		s.protection.close()
	}
	clear(s.res)
	s.protection = &wifiChild{spiIn: s.spiClient, spiOut: uint32(spi), encIn: bytes.Clone(keys.CK), encOut: bytes.Clone(keys.CK), authIn: bytes.Clone(keys.IK), authOut: bytes.Clone(keys.IK), mac: sha1.New, macLen: 12}
	s.serverProtection = &wifiChild{spiIn: s.spiServer, spiOut: uint32(client), encIn: bytes.Clone(keys.CK), encOut: bytes.Clone(keys.CK), authIn: bytes.Clone(keys.IK), authOut: bytes.Clone(keys.IK), mac: sha1.New, macLen: 12}
	s.peerClientPort = uint16(cp)
	s.target = uint16(port)
	s.verify = raw
	s.credentials = auth
	s.credentials["cnonce"] = wifiToken()
	s.res = bytes.Clone(keys.RES)
	s.nc = 0
	return nil
}
func (s *wifiIMS) authorization(uri string) (string, uint32) {
	auth := "Digest username=" + wifiQuoted(s.identity) + ", realm=" + wifiQuoted(s.domain) + ", nonce=\"\", uri=" + wifiQuoted(uri) + ", response=\"\", algorithm=AKAv1-MD5, integrity-protected=no"
	var nc uint32
	if s.protection != nil {
		auth = strings.TrimSuffix(auth, "integrity-protected=no") + "integrity-protected=yes"
	}
	if s.protection != nil && s.credentials != nil {
		s.nc++
		nc = s.nc
		a := s.credentials
		response := wifiDigest(s.identity, a["realm"], a["nonce"], uri, "REGISTER", a["qop"], a["cnonce"], nc, s.res)
		auth = "Digest username=" + wifiQuoted(s.identity) + ", realm=" + wifiQuoted(a["realm"]) + ", nonce=" + wifiQuoted(a["nonce"]) + ", uri=" + wifiQuoted(uri) + ", response=" + wifiQuoted(response) + ", algorithm=AKAv1-MD5, integrity-protected=yes"
		if a["qop"] != "" {
			auth += fmt.Sprintf(", qop=auth, nc=%08x, cnonce=%s", nc, wifiQuoted(a["cnonce"]))
		}
		if a["opaque"] != "" {
			auth += ", opaque=" + wifiQuoted(a["opaque"])
		}
	}
	return auth, nc
}
func (s *wifiIMS) exchange(ctx context.Context, expires int) (wifiSIP, error) {
	if time.Now().Before(s.retryAfter) {
		return wifiSIP{}, &wifiRegistrarDelay{s.retryAfter}
	}
	s.cseq++
	branch := "z9hG4bK" + wifiToken()
	address := net.JoinHostPort(s.local.String(), "49160")
	uri := "sip:" + s.domain
	auth, nc := s.authorization(uri)
	sequence := fmt.Sprintf("%d REGISTER", s.cseq)
	lines := []string{"REGISTER " + uri + " SIP/2.0", "Via: SIP/2.0/" + strings.ToUpper(s.transport) + " " + address + ";branch=" + branch + ";rport", "Max-Forwards: 70", "Route: <sip:" + net.JoinHostPort(s.peer.String(), strconv.Itoa(int(s.target))) + ";transport=" + s.transport + ";lr>", "From: <sip:" + s.identity + ">;tag=" + s.tag, "To: <sip:" + s.identity + ">", "Call-ID: " + s.call, "CSeq: " + sequence, "Contact: " + s.contact, fmt.Sprintf("Expires: %d", expires), "Supported: path, gruu, sec-agree", "Require: sec-agree", "Proxy-Require: sec-agree", "Security-Client: " + s.offer, "Authorization: " + auth, "P-Access-Network-Info: " + s.pani(), "User-Agent: " + s.sim.profile.Agent}
	if s.verify != "" {
		lines = append(lines, "Security-Verify: "+s.verify)
	}
	lines = append(lines, "Content-Length: 0", "", "")
	body := []byte(strings.Join(lines, "\r\n"))
	defer clear(body)
	replyPort := uint16(49160)
	if s.protection != nil {
		replyPort = 49162
	}
	match := func(data []byte) bool {
		if s.protection != nil && !bytes.HasPrefix(data, []byte("SIP/2.0 ")) {
			_ = s.request(data)
			return false
		}
		r, e := parseWiFiSIP(data)
		if e == nil && r.headers["call-id"][0] == s.call && r.headers["cseq"][0] == sequence && len(r.headers["via"]) == 1 && wifiViaBranch(r.headers["via"][0], branch) {
			s.sipObserved = true
		}
		return e == nil && r.code >= 200 && r.headers["call-id"][0] == s.call && r.headers["cseq"][0] == sequence && len(r.headers["via"]) == 1 && wifiViaBranch(r.headers["via"][0], branch)
	}
	var data []byte
	var err error
	if s.transport == "tcp" {
		data, err = s.tcpExchange(ctx, body, match)
	} else {
		data, err = s.child.udpExchange(ctx, s.local, s.peer, 49160, s.target, replyPort, body, s.protection, match, s.receiveDatagram)
	}
	if err != nil {
		return wifiSIP{}, err
	}
	defer clear(data)
	r, err := parseWiFiSIP(data)
	if err != nil {
		return r, err
	}
	if r.code == 503 {
		s.retryAfter = time.Now().Add(wifiRetryAfter(r.headers["retry-after"]))
		return r, &wifiRegistrarDelay{s.retryAfter}
	}

	if nc > 0 && r.code == 200 && len(r.headers["authentication-info"]) > 0 {
		if len(r.headers["authentication-info"]) != 1 {
			return r, errors.New("WIFI_IMS_PEER_AUTH_FAILED")
		}
		info, err := wifiParameters(r.headers["authentication-info"][0], ',')
		if err != nil {
			return r, err
		}
		a := s.credentials
		want := wifiDigest(s.identity, a["realm"], a["nonce"], uri, "", a["qop"], a["cnonce"], nc, s.res)
		if info["rspauth"] != "" && !hmac.Equal([]byte(strings.ToLower(info["rspauth"])), []byte(want)) {
			return r, errors.New("WIFI_IMS_PEER_AUTH_FAILED")
		}
	}
	// Without qop/nc, a second digest would replay the AKA response. Subsequent
	// registrations retain the authenticated IPsec association, not that response.
	if r.code == 200 && s.credentials != nil && s.credentials["qop"] == "" {
		clear(s.res)
		s.res = nil
		s.credentials = nil
	}
	return r, nil
}
func (s *wifiIMS) register(ctx context.Context) (wifiSIP, error) {
	r, err := s.exchange(ctx, 600)
	if err != nil {
		return r, err
	}
	if r.code != 401 {
		return r, fmt.Errorf("WIFI_IMS_STATUS_%d", r.code)
	}
	if err = s.authorize(ctx, r); err != nil {
		return r, err
	}
	r, err = s.exchange(ctx, 600)
	if err != nil {
		return r, fmt.Errorf("WIFI_IMS_PROTECTED: %w", err)
	}
	if r.code != 200 {
		return r, fmt.Errorf("WIFI_IMS_STATUS_%d", r.code)
	}
	s.registered = true
	if err = s.setExpiry(r); err != nil {
		return r, err
	}
	return r, nil
}
func (s *wifiIMS) close() error {
	var err error
	if s.registered {
		if time.Now().Before(s.retryAfter) {
			err = errors.New("WIFI_IMS_DEREGISTER_UNCONFIRMED")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			r, e := s.authenticatedExchange(ctx, 0)
			cancel()
			err = e
			if err == nil && r.code != 200 {
				err = fmt.Errorf("WIFI_IMS_DEREGISTER_UNCONFIRMED_STATUS_%d", r.code)
			}
		}
	}
	if s.tcp != nil {
		s.tcp.close()
		s.tcp = nil
	}
	if s.serverProtection != nil {
		s.serverProtection.close()
		s.serverProtection = nil
	}
	if s.protection != nil {
		s.protection.close()
		s.protection = nil
	}
	clear(s.res)
	s.credentials = nil
	s.registered = false
	return err
}

// A new AKA challenge requires a fresh security agreement, not resetting replay
// state on the existing SPIs. Let the caller establish a new session instead.
func (s *wifiIMS) authenticatedExchange(ctx context.Context, expires int) (wifiSIP, error) {
	r, e := s.exchange(ctx, expires)
	if e != nil || r.code != 401 {
		return r, e
	}
	return r, errors.New("WIFI_IMS_REAUTH_REQUIRED")
}
func (s *wifiIMS) renew(ctx context.Context) error {
	r, err := s.authenticatedExchange(ctx, 600)
	if err != nil {
		return err
	}
	if r.code != 200 {
		return fmt.Errorf("WIFI_IMS_STATUS_%d", r.code)
	}
	return s.setExpiry(r)
}
func (s *wifiIMS) setExpiry(r wifiSIP) error {
	seconds := 600
	for _, value := range r.headers["expires"] {
		if n, e := strconv.Atoi(value); e == nil && n >= 0 && n <= 86400 {
			seconds = n
		}
	}
	own := wifiContactURI(s.contact)
	found := false
	for _, header := range r.headers["contact"] {
		values, e := wifiHeaderValues(header)
		if e != nil {
			return errors.New("WIFI_IMS_INVALID")
		}
		for _, value := range values {
			_, tail, ok := strings.Cut(value, ">")
			if !ok || wifiContactURI(value) != own {
				continue
			}
			found = true
			for _, part := range strings.Split(tail, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
				if ok && strings.EqualFold(k, "expires") {
					n, e := strconv.Atoi(strings.Trim(v, "\""))
					if e != nil || n < 0 || n > 86400 {
						return errors.New("WIFI_IMS_INVALID")
					}
					seconds = n
				}
			}
		}
	}
	if len(r.headers["contact"]) > 0 && !found {
		return errors.New("WIFI_IMS_CONTACT_UNCONFIRMED")
	}
	if seconds < 10 {
		return errors.New("WIFI_IMS_REGISTRATION_EXPIRED")
	}
	s.renewAt = time.Now().Add(time.Duration(seconds) * time.Second / 2)
	return nil
}

func wifiContactURI(value string) string {
	_, value, ok := strings.Cut(value, "<")
	if !ok {
		return ""
	}
	uri, _, ok := strings.Cut(value, ">")
	if !ok {
		return ""
	}
	uri, _, _ = strings.Cut(uri, ";")
	return strings.ToLower(strings.TrimSpace(uri))
}
func (s *wifiIMS) request(data []byte) error {
	cut := bytes.Index(data, []byte("\r\n"))
	if cut < 0 || cut > 2048 || bytes.HasPrefix(data, []byte("SIP/2.0 ")) {
		return nil
	}
	first := strings.Fields(string(data[:cut]))
	if len(first) != 3 || first[2] != "SIP/2.0" {
		return nil
	}
	r, err := parseWiFiSIP(append([]byte("SIP/2.0 200 OK"), data[cut:]...))
	if err != nil || len(r.headers["via"]) < 1 || len(r.headers["via"]) > 4 {
		return nil
	}
	seq := strings.Fields(r.headers["cseq"][0])
	if len(seq) != 2 || seq[1] != first[0] {
		return nil
	}
	if _, e := strconv.ParseUint(seq[0], 10, 32); e != nil {
		return nil
	}
	code, reason := 501, "Not Implemented"
	terminated := false
	switch first[0] {
	case "ACK":
		return nil
	case "OPTIONS":
		code, reason = 200, "OK"
	case "NOTIFY":
		code, reason = 200, "OK"
		for _, v := range r.headers["subscription-state"] {
			terminated = terminated || strings.HasPrefix(strings.ToLower(v), "terminated")
		}
	case "MESSAGE":
		code, reason = 503, "Service Unavailable"
	case "INVITE":
		code, reason = 480, "Temporarily Unavailable"
	}
	lines := []string{fmt.Sprintf("SIP/2.0 %d %s", code, reason)}
	for _, v := range r.headers["via"] {
		lines = append(lines, "Via: "+v)
	}
	to := r.headers["to"][0]
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + s.tag
	}
	lines = append(lines, "From: "+r.headers["from"][0], "To: "+to, "Call-ID: "+r.headers["call-id"][0], "CSeq: "+r.headers["cseq"][0], "Allow: REGISTER, OPTIONS, NOTIFY", "Content-Length: 0", "", "")
	body := []byte(strings.Join(lines, "\r\n"))
	defer clear(body)
	if err := s.sendResponse(body); err != nil {
		return err
	}
	if terminated {
		return errors.New("WIFI_IMS_REGISTRATION_EXPIRED")
	}
	return nil
}

func (s *wifiIMS) pani() string {
	v := "IEEE-802.11"
	if s.sim.profile.Country != "" {
		v += ";country=" + s.sim.profile.Country
	}
	return v + ";i-wlan-node-id=ffffffffffff"
}
func (s *wifiIMS) sendResponse(body []byte) error {
	if s.transport == "tcp" {
		call, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn := s.replyConn
		if conn == nil {
			conn = s.tcp.client
		}
		return s.tcp.write(call, conn, body)
	}
	return s.child.sendUDP(s.local, s.peer, 49160, s.target, body, s.protection)
}

// One event loop owns the socket and SIM. No per-request goroutines or timers.
func (s *wifiIMS) maintain(ctx context.Context, emit func(string), testUntil time.Time, stopAfterRenewal bool) error {
	heartbeat := time.Now()
	aliveAt := time.Now().Add(60 * time.Second)
	natAt := time.Now().Add(15 * time.Second)
	buf := make([]byte, 65536)
	stop := wifiCancelRead(ctx, s.child.ike.conn)
	defer stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now := time.Now()
		if !testUntil.IsZero() && !now.Before(testUntil) {
			return nil
		}
		if !now.Before(natAt) {
			s.child.ike.conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, e := s.child.ike.conn.Write([]byte{255}); e != nil {
				return errors.New("WIFI_NETWORK_WRITE_FAILED")
			}
			natAt = time.Now().Add(15 * time.Second)
		}
		if !now.Before(heartbeat) {
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.sim.verifyCard(call)
			if err == nil {
				mode, e := wifiRadioMode(call, s.sim.session)
				if e != nil || mode != 4 {
					err = errors.New("WIFI_RADIO_UNCONFIRMED")
				}
			}
			cancel()
			if err != nil {
				return err
			}
			emit("connected")
			heartbeat = time.Now().Add(30 * time.Second)
		}
		if !now.Before(s.renewAt) {
			call, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := s.renew(call)
			cancel()
			if err != nil {
				return err
			}
			emit("renewed")
			if stopAfterRenewal {
				return nil
			}
		}
		if !now.Before(aliveAt) {
			call, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := s.child.ike.alive(call)
			cancel()
			if err != nil {
				return err
			}
			aliveAt = time.Now().Add(60 * time.Second)
		}
		deadline := time.Now().Add(15 * time.Second)
		for _, limit := range []time.Time{heartbeat, aliveAt, natAt, s.renewAt, testUntil} {
			if !limit.IsZero() && limit.Before(deadline) {
				deadline = limit
			}
		}
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		if s.transport == "tcp" {
			select {
			case f := <-s.tcp.frames:
				if f.err != nil {
					if f.conn == s.tcp.client {
						return errors.New("WIFI_TCP_CLOSED")
					}
					continue
				}
				s.replyConn = f.conn
				e := s.request(f.data)
				s.replyConn = nil
				clear(f.data)
				if e != nil {
					return e
				}
			default:
				if e := s.tcp.pump(ctx, 100*time.Millisecond); e != nil {
					return e
				}
			}
			continue
		}
		s.child.ike.conn.SetDeadline(deadline)
		n, err := s.child.ike.conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			return errors.New("WIFI_NETWORK_READ_FAILED")
		}
		handled, err := s.child.ike.incoming(buf[:n])
		if err != nil {
			return err
		}
		if handled {
			continue
		}
		data, err := s.receiveDatagram(buf[:n])
		if err != nil {
			continue
		}
		err = s.request(data)
		clear(data)
		if err != nil {
			return err
		}
	}
}

func wifiViaBranch(v, want string) bool {
	found := false
	for _, part := range strings.Split(v, ";")[1:] {
		k, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if strings.EqualFold(k, "branch") {
			if !ok || found || value != want {
				return false
			}
			found = true
		}
	}
	return found
}
