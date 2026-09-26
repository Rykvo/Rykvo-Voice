package hardware

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/mms"
)

// EC20 has no QMMSRECV command. WAP Push supplies the URL; retrieve MM1 over
// a socket bound to the MMS PDP, then acknowledge only after durable storage.
func (s *System) ReceiveCellularMMS(ctx context.Context, c Candidate, identity, card string, p carrierconfig.Profile, location, transaction string, store func(mms.PDU) error) error {
	if _, _, e := mmsProfile(p); e != nil {
		return e
	}
	if _, e := mmsReceiveURL(p, location); e != nil {
		return e
	}
	if len(transaction) == 0 || len(transaction) > 80 || strings.ContainsAny(transaction, "\x00\r\n") {
		return errors.New("MMS_INVALID_NOTIFICATION")
	}
	at, e := s.cellularSession(ctx, c, identity, card)
	if e != nil {
		return errors.New("MMS_NOT_READY")
	}
	b, e := prepareMMSBearer(ctx, at, p)
	if e != nil {
		return e
	}
	defer b.close()
	data, e := b.http(ctx, p, "GET", location, nil)
	if e != nil {
		return e
	}
	v, e := mms.Parse(data)
	if e != nil || v.Type != 0x84 || (v.Status != 0 && v.Status != 0x80) {
		return mms.ErrPDU
	}
	if e = store(v); e != nil {
		return e
	}
	// A failed acknowledgement must not undo a received message or re-download it.
	_, _ = b.http(ctx, p, "POST", p.MMSC, mms.NotifyResponse(transaction, 0x81))
	return nil
}

func mmsReceiveURL(p carrierconfig.Profile, raw string) (*url.URL, error) {
	u, e := mms.ReceiveURL(p, raw)
	if e != nil || u.Scheme != "http" {
		return nil, errors.New("MMS_LOCATION_UNSUPPORTED")
	}
	return u, nil
}

// Resolve on this PDP and pin the checked address for QIOPEN.
func (b *mmsBearer) resolveHost(parent context.Context, host string) (string, error) {
	if !mms.BearerHost(host) {
		return "", errors.New("MMS_LOCATION_UNSUPPORTED")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	if err := smsATWrite(ctx, b.at, []byte(fmt.Sprintf("AT+QIDNSGIP=%d,%q\r", b.cid, host))); err != nil {
		b.healthy = false
		return "", errors.New("MMS_DNS_FAILED")
	}
	count, address := -1, ""
	for {
		line, err := mmsWait(ctx, b.at, "+QIURC")
		if err != nil {
			b.healthy = false
			return "", errors.New("MMS_DNS_FAILED")
		}
		f := fields(line)
		if len(f) == 0 || f[0] != "dnsgip" {
			continue
		}
		if count < 0 {
			if len(f) != 4 || f[1] != "0" {
				return "", errors.New("MMS_DNS_FAILED")
			}
			count, err = strconv.Atoi(f[2])
			if err != nil || count < 1 || count > 16 {
				b.healthy = false
				return "", errors.New("MMS_DNS_FAILED")
			}
			continue
		}
		if len(f) != 2 || net.ParseIP(f[1]) == nil {
			b.healthy = false
			return "", errors.New("MMS_DNS_FAILED")
		}
		if address == "" && mms.BearerHost(f[1]) {
			address = net.ParseIP(f[1]).String()
		}
		count--
		if count == 0 {
			if address == "" {
				return "", errors.New("MMS_LOCATION_UNSUPPORTED")
			}
			return address, nil
		}
	}
}

func mmsReadBytes(ctx context.Context, at *atSession, n int) ([]byte, error) {
	if n < 0 || n > mms.MaxSize {
		return nil, errors.New("MMS_TOO_LARGE")
	}
	for len(at.buffer) < n {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		buf := make([]byte, 2048)
		count, e := at.port.Read(buf)
		at.buffer += string(buf[:count])
		if e != nil {
			return nil, e
		}
	}
	v := []byte(at.buffer[:n])
	at.buffer = at.buffer[n:]
	return v, nil
}

type mmsSocket struct {
	ctx context.Context
	b   *mmsBearer
	id  int
}

func (s *mmsSocket) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for {
		n := len(dst)
		if n > 1500 {
			n = 1500
		}
		if e := smsATWrite(s.ctx, s.b.at, []byte(fmt.Sprintf("AT+QIRD=%d,%d\r", s.id, n))); e != nil {
			s.b.healthy = false
			return 0, e
		}
		line, e := mmsWait(s.ctx, s.b.at, "+QIRD")
		if e != nil {
			s.b.healthy = false
			return 0, e
		}
		count, e := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "+QIRD:")))
		if e != nil || count < 0 || count > n {
			s.b.healthy = false
			return 0, errors.New("MMS_INVALID_RESPONSE")
		}
		data, e := mmsReadBytes(s.ctx, s.b.at, count)
		if e == nil {
			_, e = mmsWait(s.ctx, s.b.at, "OK")
		}
		if e != nil {
			s.b.healthy = false
			return 0, e
		}
		if count > 0 {
			return copy(dst, data), nil
		}
		lines, e := s.b.command(s.ctx, fmt.Sprintf("AT+QISTATE=1,%d", s.id), 2*time.Second)
		if e != nil {
			return 0, e
		}
		connected := false
		for _, v := range lines {
			if strings.HasPrefix(v, "+QISTATE:") {
				f := fields(v)
				connected = len(f) > 5 && f[0] == strconv.Itoa(s.id) && f[5] == "2"
			}
		}
		if !connected {
			return 0, io.EOF
		}
		select {
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (b *mmsBearer) http(parent context.Context, p carrierconfig.Profile, method, address string, body []byte) ([]byte, error) {
	return b.httpExchange(parent, p, method, address, body, nil)
}

func (b *mmsBearer) httpExchange(parent context.Context, p carrierconfig.Profile, method, address string, body []byte, submit *MMSSubmitResult) ([]byte, error) {
	if len(body) > mms.MaxSize {
		return nil, errors.New("MMS_TOO_LARGE")
	}

	u, e := mmsReceiveURL(p, address)
	if e != nil {
		return nil, e
	}
	_, proxyPort, e := mmsProfile(p)
	if e != nil {
		return nil, e
	}
	host, port := u.Hostname(), 80
	if u.Port() != "" {
		port, e = strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return nil, errors.New("MMS_LOCATION_UNSUPPORTED")
		}
	}
	if p.MMSProxy != "" {
		host, port = p.MMSProxy, proxyPort
	}
	if !mmsASCII(host, 253) {
		return nil, errors.New("MMS_LOCATION_UNSUPPORTED")
	}
	ctx, cancel := context.WithTimeout(parent, 180*time.Second)
	defer cancel()
	host, e = b.resolveHost(ctx, host)
	if e != nil {
		return nil, e
	}
	lines, e := b.command(ctx, "AT+QISTATE?", 2*time.Second)
	if e != nil {
		return nil, e
	}
	used := map[int]bool{}
	for _, line := range lines {
		if strings.HasPrefix(line, "+QISTATE:") {
			f := fields(line)
			if len(f) == 0 {
				return nil, errors.New("MMS_SOCKET_BUSY")
			}
			id, e := strconv.Atoi(f[0])
			if e != nil {
				return nil, e
			}
			used[id] = true
		}
	}
	id := -1
	for i := 11; i >= 0; i-- {
		if !used[i] {
			id = i
			break
		}
	}
	if id < 0 {
		return nil, errors.New("MMS_SOCKET_BUSY")
	}
	defer func() {
		if b.healthy {
			clean, stop := context.WithTimeout(context.Background(), 12*time.Second)
			defer stop()
			_, _ = b.command(clean, fmt.Sprintf("AT+QICLOSE=%d,10", id), 12*time.Second)
		}
	}()
	e = smsATWrite(ctx, b.at, []byte(fmt.Sprintf("AT+QIOPEN=%d,%d,\"TCP\",%q,%d,0,0\r", b.cid, id, host, port)))
	var line string
	if e == nil {
		line, e = mmsWait(ctx, b.at, "+QIOPEN")
	}
	if e != nil {
		b.healthy = false
		return nil, errors.New("MMS_SOCKET_FAILED")
	}
	f := fields(line)
	if len(f) != 2 || f[0] != strconv.Itoa(id) || f[1] != "0" {
		return nil, errors.New("MMS_SOCKET_FAILED")
	}
	req, e := http.NewRequest(method, u.String(), bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Close = true
	req.Header.Set("Accept", "application/vnd.wap.mms-message")
	req.Header.Set("User-Agent", "Rykvo-Voice/1.5")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/vnd.wap.mms-message")
	}
	var request bytes.Buffer
	if p.MMSProxy != "" {
		e = req.WriteProxy(&request)
	} else {
		e = req.Write(&request)
	}
	if e != nil || request.Len() > mms.MaxSize+8192 {
		return nil, errors.New("MMS_INVALID_REQUEST")
	}
	data := request.Bytes()
	for len(data) > 0 {
		n := len(data)
		if n > 1460 {
			n = 1460
		}
		e = smsATWrite(ctx, b.at, []byte(fmt.Sprintf("AT+QISEND=%d,%d\r", id, n)))
		if e == nil {
			_, e = smsATRead(ctx, b.at, true)
		}
		if e == nil {
			e = mmsDataReady(ctx)
		}
		if e == nil {
			if submit != nil {
				submit.Attempted = true
				submit.Stage = "submit"
			}
			e = mmsDataWrite(ctx, b.at, data[:n])
		}
		if e == nil {
			_, e = mmsWait(ctx, b.at, "SEND OK")
		}
		if e != nil {
			b.healthy = false
			return nil, errors.New("MMS_SOCKET_FAILED")
		}
		data = data[n:]
	}
	reader := bufio.NewReaderSize(io.LimitReader(&mmsSocket{ctx: ctx, b: b, id: id}, mms.MaxSize+16384), 4096)
	response, e := http.ReadResponse(reader, req)
	if e != nil {
		return nil, errors.New("MMS_HTTP_INVALID")
	}
	defer response.Body.Close()
	if submit != nil {
		submit.HTTPCode = response.StatusCode
		submit.Stage = "response"
	}
	if method == "POST" && submit == nil && response.StatusCode == 204 {
		return nil, nil
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("MMS_HTTP_%d", response.StatusCode)
	}
	if method == "GET" || submit != nil {
		kind, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if e != nil || kind != "application/vnd.wap.mms-message" {
			return nil, errors.New("MMS_HTTP_CONTENT_TYPE")
		}
	}
	data, e = io.ReadAll(io.LimitReader(response.Body, mms.MaxSize+1))
	if e != nil {
		return nil, errors.New("MMS_HTTP_INCOMPLETE")
	}
	if len(data) > mms.MaxSize {
		return nil, errors.New("MMS_TOO_LARGE")
	}
	return data, nil
}
