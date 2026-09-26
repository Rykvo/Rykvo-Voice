package hardware

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/mms"
)

// Accepted is the MMSC submission result, not a handset delivery receipt.
type MMSSubmitResult struct {
	Attempted bool   `json:"attempted"`
	Accepted  bool   `json:"accepted"`
	Stage     string `json:"stage"`
	Code      int    `json:"code"`
	HTTPCode  int    `json:"httpCode"`
}

type mmsBearer struct {
	at      *atSession
	cid     int
	created bool
	active  bool
	healthy bool
}

func mmsASCII(v string, max int) bool {
	if len(v) > max || strings.ContainsAny(v, "\"\\") {
		return false
	}
	for _, c := range v {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

func mmsProfile(p carrierconfig.Profile) (int, int, error) {
	u, e := url.Parse(p.MMSC)
	if e != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || !mmsASCII(p.MMSC, 100) ||
		!apnNamePattern.MatchString(p.APN) || strings.EqualFold(p.APN, "ims") || strings.EqualFold(p.APN, "sos") ||
		!mmsASCII(p.User, 64) || !mmsASCII(p.Password, 64) || !mmsASCII(p.MMSProxy, 50) || strings.ContainsAny(p.MMSProxy, " /:@") {
		return 0, 0, errors.New("MMS_PROFILE_UNSUPPORTED")
	}
	auth := 0
	if p.Auth != "" && p.Auth != "-1" {
		auth, e = strconv.Atoi(p.Auth)
		if e != nil || auth < 0 || auth > 3 {
			return 0, 0, errors.New("MMS_PROFILE_UNSUPPORTED")
		}
	} else if p.User != "" || p.Password != "" {
		auth = 3
	}
	port := 80
	if p.MMSPort != "" {
		port, e = strconv.Atoi(p.MMSPort)
	}
	if e != nil || port < 1 || port > 65535 {
		return 0, 0, errors.New("MMS_PROFILE_UNSUPPORTED")
	}
	return auth, port, nil
}

// Only claim an inactive matching context or an undefined context above CID 3.
// Default, IMS, SOS and contexts activated by another service stay untouched.
func selectMMSContext(contexts []APNContext, active map[int]bool, apn string) (int, bool) {
	used := map[int]bool{}
	for _, c := range contexts {
		used[c.CID] = true
		if c.CID >= 4 && !active[c.CID] && strings.EqualFold(c.APN, apn) {
			return c.CID, false
		}
	}
	for id := 16; id >= 4; id-- {
		if !used[id] && !active[id] {
			return id, true
		}
	}
	return 0, false
}

func (b *mmsBearer) command(ctx context.Context, cmd string, wait time.Duration) ([]string, error) {
	v, e := b.at.exchange(ctx, cmd, wait)
	if e != nil && !strings.HasPrefix(e.Error(), "COMMAND_") {
		b.healthy = false
	}
	return v, e
}
func (b *mmsBearer) close() {
	defer b.at.port.Close()
	if !b.healthy {
		return
	} // Late/data-mode responses must not satisfy cleanup commands.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if b.active {
		if _, e := b.command(ctx, fmt.Sprintf("AT+QIDEACT=%d", b.cid), 40*time.Second); e != nil {
			return
		}
	}
	if b.created {
		_, _ = b.command(ctx, fmt.Sprintf("AT+CGDCONT=%d", b.cid), 2*time.Second)
	}
}

func prepareMMSBearer(ctx context.Context, at *atSession, p carrierconfig.Profile) (*mmsBearer, error) {
	b := &mmsBearer{at: at, healthy: true}
	fail := func(code string) (*mmsBearer, error) { b.close(); return nil, errors.New(code) }
	auth, _, e := mmsProfile(p)
	if e != nil {
		return fail(e.Error())
	}
	lines, e := b.command(ctx, "AT+CGDCONT?", 2*time.Second)
	if e != nil {
		return fail("MMS_CONTEXT_UNAVAILABLE")
	}
	contexts, e := parseAPNContexts(lines)
	if e != nil {
		return fail("MMS_CONTEXT_UNAVAILABLE")
	}
	active := map[int]bool{}
	for _, cmd := range []string{"AT+CGACT?", "AT+QIACT?"} {
		lines, e = b.command(ctx, cmd, 2*time.Second)
		if e != nil {
			return fail("MMS_CONTEXT_UNAVAILABLE")
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "+CGACT:") && !strings.HasPrefix(line, "+QIACT:") {
				continue
			}
			f := fields(line)
			if len(f) < 2 {
				return fail("MMS_CONTEXT_UNAVAILABLE")
			}
			cid, ce := strconv.Atoi(f[0])
			state, se := strconv.Atoi(f[1])
			if ce != nil || se != nil {
				return fail("MMS_CONTEXT_UNAVAILABLE")
			}
			if state != 0 {
				active[cid] = true
			}
		}
	}
	b.cid, b.created = selectMMSContext(contexts, active, p.APN)
	if b.cid == 0 {
		return fail("MMS_CONTEXT_BUSY")
	}
	if !b.created {
		lines, e = b.command(ctx, fmt.Sprintf("AT+QICSGP=%d", b.cid), 2*time.Second)
		if e != nil {
			return fail("MMS_CONTEXT_UNAVAILABLE")
		}
		matched := false
		for _, line := range lines {
			if strings.HasPrefix(line, "+QICSGP:") {
				f := fields(line)
				matched = len(f) == 5 && strings.EqualFold(f[1], p.APN) && f[2] == p.User && f[3] == p.Password && f[4] == strconv.Itoa(auth)
			}
		}
		if !matched {
			return fail("MMS_CONTEXT_CONFLICT")
		}
	} else {
		protocol := 1
		switch strings.ToUpper(p.Protocol) {
		case "IPV6":
			protocol = 2
		case "IPV4V6":
			protocol = 3
		}
		_, e = b.command(ctx, fmt.Sprintf("AT+QICSGP=%d,%d,%q,%q,%q,%d", b.cid, protocol, p.APN, p.User, p.Password, auth), 2*time.Second)
		if e != nil {
			return fail("MMS_CONTEXT_CONFIG_FAILED")
		}
	}
	// The documented command may take 150 seconds. A timeout is not followed by AT cleanup.
	_, e = b.command(ctx, fmt.Sprintf("AT+QIACT=%d", b.cid), 150*time.Second)
	if e != nil {
		return fail("MMS_PDP_ACTIVATION_FAILED")
	}
	b.active = true
	return b, nil
}

// Read CRLF lines without consuming bytes after CONNECT (which can be binary).
func mmsLine(ctx context.Context, at *atSession) (string, error) {
	for {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		if i := strings.IndexByte(at.buffer, '\n'); i >= 0 {
			line := strings.TrimSpace(at.buffer[:i])
			at.buffer = at.buffer[i+1:]
			if line != "" {
				return line, nil
			}
			continue
		}
		if len(at.buffer) > 8192 {
			return "", errors.New("MMS_INVALID_RESPONSE")
		}
		buf := make([]byte, 2048)
		n, e := at.port.Read(buf)
		at.buffer += string(buf[:n])
		if e != nil {
			return "", e
		}
	}
}
func mmsWait(ctx context.Context, at *atSession, prefix string) (string, error) {
	for {
		line, e := mmsLine(ctx, at)
		if e != nil {
			return "", e
		}
		if line == "ERROR" || strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "+CMS ERROR") {
			return "", errors.New("MMS_COMMAND_REJECTED")
		}
		if line == prefix || strings.HasPrefix(line, prefix+":") {
			return line, nil
		}
	}
}
func mmsChecksum(data []byte) uint16 {
	var value uint16
	for i, v := range data {
		if i%2 == 0 {
			value ^= uint16(v) << 8
		} else {
			value ^= uint16(v)
		}
	}
	return value
}
func (b *mmsBearer) upload(ctx context.Context, name string, data []byte) error {
	if bytes.Contains(data, []byte("+++")) {
		return b.uploadChunks(ctx, name, data)
	}
	call, cancel := context.WithTimeout(ctx, 65*time.Second)
	defer cancel()
	e := smsATWrite(call, b.at, []byte(fmt.Sprintf("AT+QFUPL=%q,%d,60,1\r", name, len(data))))
	if e == nil {
		_, e = mmsWait(call, b.at, "CONNECT")
	}
	if e == nil {
		e = mmsDataReady(call)
	}
	for offset := 0; e == nil && offset < len(data); {
		n := len(data) - offset
		if n > 1024 {
			n = 1024
		}
		e = mmsDataWrite(call, b.at, data[offset:offset+n])
		offset += n
		if e == nil && offset < len(data) {
			var ack []byte
			ack, e = mmsReadBytes(call, b.at, 1)
			if e == nil && (len(ack) != 1 || ack[0] != 'A') {
				e = errors.New("MMS_UPLOAD_ACK")
			}
		}
	}
	var result string
	if e == nil {
		for {
			result, e = mmsLine(call, b.at)
			if e != nil {
				break
			}
			result = strings.TrimPrefix(result, "A")
			if strings.HasPrefix(result, "+QFUPL:") {
				break
			}
			if strings.Contains(result, "ERROR") {
				e = errors.New("MMS_UPLOAD_FAILED")
				break
			}
		}
	}
	if e == nil {
		_, e = mmsWait(call, b.at, "OK")
	}
	if e != nil {
		b.healthy = false
		return errors.New("MMS_UPLOAD_FAILED")
	}
	f := fields(result)
	if len(f) != 2 {
		return errors.New("MMS_UPLOAD_FAILED")
	}
	size, e := strconv.Atoi(f[0])
	sum, se := strconv.ParseUint(f[1], 16, 16)
	if e != nil || se != nil || size != len(data) || uint16(sum) != mmsChecksum(data) {
		return errors.New("MMS_UPLOAD_CHECKSUM")
	}
	return nil
}

func (s *System) SendCellularMMS(ctx context.Context, c Candidate, identity, card string, p carrierconfig.Profile, id, to, text string, image *mms.Part) (MMSSubmitResult, error) {
	r := MMSSubmitResult{Stage: "validate"}
	if _, e := mms.SendRequest(id, to, text, image); e != nil {
		return r, e
	}
	_, port, e := mmsProfile(p)
	if e != nil {
		return r, e
	}
	at, e := s.cellularSession(ctx, c, identity, card)
	if e != nil {
		return r, errors.New("MMS_NOT_READY")
	}
	r.Stage = "activate"
	b, e := prepareMMSBearer(ctx, at, p)
	if e != nil {
		return r, e
	}
	defer b.close()
	return sendNativeMMS(ctx, b, p, port, to, text, image)
}

func sendNativeMMS(ctx context.Context, b *mmsBearer, p carrierconfig.Profile, port int, to, text string, image *mms.Part) (MMSSubmitResult, error) {
	r := MMSSubmitResult{Stage: "configure"}
	files := []string{}
	defer func() {
		if !b.healthy {
			return
		}
		clean, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if _, e := b.command(clean, "AT+QMMSEDIT=0", 2*time.Second); e != nil {
			return
		}
		for _, name := range files {
			if _, e := b.command(clean, "AT+QFDEL="+strconv.Quote(name), 2*time.Second); e != nil {
				return
			}
		}
	}()
	commands := []string{
		fmt.Sprintf("AT+QMMSCFG=\"contextid\",%d", b.cid),
		"AT+QMMSCFG=\"mmsc\"," + strconv.Quote(p.MMSC),
		fmt.Sprintf("AT+QMMSCFG=\"proxy\",%q,%d", p.MMSProxy, port),
		"AT+QMMSCFG=\"sendparam\",6,1,1,0,2,0",
		"AT+QMMSCFG=\"connecttimeout\",60",
		"AT+QMMSCFG=\"character\",\"UTF8\"",
		"AT+QMMSEDIT=0", "AT+QMMSEDIT=1,1," + strconv.Quote(to),
	}
	for _, cmd := range commands {
		if _, e := b.command(ctx, cmd, 2*time.Second); e != nil {
			return r, errors.New("MMS_CONFIG_FAILED")
		}
	}
	token := make([]byte, 4)
	if _, e := rand.Read(token); e != nil {
		return r, e
	}
	base := "RAM:" + hex.EncodeToString(token)
	parts := []mms.Part{}
	if text != "" {
		parts = append(parts, mms.Part{Type: "text/plain", Data: []byte(text)})
	}
	if image != nil {
		parts = append(parts, *image)
	}
	r.Stage = "upload"
	for _, part := range parts {
		ext := map[string]string{"text/plain": "txt", "image/jpeg": "jpg", "image/png": "png", "image/gif": "gif"}[part.Type]
		if ext == "" {
			return r, errors.New("MMS_UNSUPPORTED_CONTENT")
		}
		name := base + "." + ext
		if e := b.fileAvailable(ctx, name); e != nil {
			return r, e
		}
		files = append(files, name)
		if e := b.upload(ctx, name, part.Data); e != nil {
			return r, e
		}
		if _, e := b.command(ctx, "AT+QMMSEDIT=5,1,"+strconv.Quote(name), 2*time.Second); e != nil {
			return r, errors.New("MMS_ATTACHMENT_FAILED")
		}
	}
	r.Stage = "submit"
	r.Attempted = true
	call, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	if e := smsATWrite(call, b.at, []byte("AT+QMMSEND=90\r")); e != nil {
		b.healthy = false
		return r, mms.ErrUnknown
	}
	line, e := mmsWait(call, b.at, "+QMMSEND") // An initial OK is not acceptance.
	if e != nil {
		b.healthy = false
		return r, mms.ErrUnknown
	}
	f := fields(line)
	if len(f) < 1 || len(f) > 3 {
		return r, mms.ErrUnknown
	}
	r.Code, e = strconv.Atoi(f[0])
	if e != nil {
		return r, mms.ErrUnknown
	}
	if len(f) > 1 {
		r.HTTPCode, e = strconv.Atoi(f[1])
		if e != nil {
			return r, mms.ErrUnknown
		}
	}
	r.Stage = "complete"
	if r.Code == 0 && r.HTTPCode == 200 {
		r.Accepted = true
		return r, nil
	}
	// Socket/response failures can occur after submission. Never resend automatically.
	return r, fmt.Errorf("MMS_MODEM_%d_HTTP_%d", r.Code, r.HTTPCode)
}
