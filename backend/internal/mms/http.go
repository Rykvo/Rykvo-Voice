package mms

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
)

var ErrNetwork = errors.New("MMS_NETWORK_REQUIRED")
var ErrUnknown = errors.New("MMS_OUTCOME_UNKNOWN")

// These failures are known to occur before MMS submission.
func WaitingNetwork(err error) bool {
	return err != nil && (errors.Is(err, ErrNetwork) || err.Error() == "MMS_BUSY" || strings.HasPrefix(err.Error(), "MMS_IWLAN_"))
}

type Client struct {
	http    *http.Client
	base    *url.URL
	profile carrierconfig.Profile
}
type DialContext func(context.Context, string, string) (net.Conn, error)

func safeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(raw, "\x00\r\n") {
		return nil, ErrNetwork
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" && port != "8080" && port != "8002" {
		return nil, ErrNetwork
	}
	return u, nil
}

// The carrier's retrieval gateway can differ from its submission endpoint.
// Names use exact carrier aliases. Private retrieval IPs stay inside the bound bearer.
func ReceiveURL(p carrierconfig.Profile, raw string) (*url.URL, error) {
	u, err := safeURL(raw)
	base, be := safeURL(p.MMSC)
	if err != nil || be != nil || u.Scheme != base.Scheme {
		return nil, ErrNetwork
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.To4() != nil && ip.IsPrivate() {
		return u, nil
	}
	if strings.EqualFold(u.Host, base.Host) {
		return u, nil
	}
	if (p.MCC == "310" || p.MCC == "311") && strings.EqualFold(base.Host, "mms.msg.eng.t-mobile.com") && strings.EqualFold(u.Host, "mpc.t-mobile.com") {
		return u, nil
	}
	return nil, ErrNetwork
}

// A caller must supply a SIM-bound bearer. There is no host-internet fallback.
func NewClient(p carrierconfig.Profile, dial DialContext) (*Client, error) {
	base, err := safeURL(p.MMSC)
	if err != nil || dial == nil {
		return nil, ErrNetwork
	}
	tr := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 16384}
	proxyHost := ""
	if p.MMSProxy != "" {
		port := p.MMSPort
		if port == "" {
			port = "80"
		}
		proxy, err := safeURL("http://" + net.JoinHostPort(p.MMSProxy, port))
		if err != nil {
			return nil, err
		}
		proxyHost = proxy.Host
		tr.Proxy = http.ProxyURL(proxy)
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if proxyHost != "" {
			if !strings.EqualFold(address, proxyHost) {
				return nil, ErrNetwork
			}
		} else {
			host, port, e := net.SplitHostPort(address)
			if e != nil {
				return nil, ErrNetwork
			}
			scheme := base.Scheme
			target := scheme + "://" + net.JoinHostPort(host, port)
			if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
				target = scheme + "://" + host
			}
			if _, e = ReceiveURL(p, target); e != nil {
				return nil, ErrNetwork
			}
		}
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, ErrNetwork
		}
		return conn, nil
	}
	client := &http.Client{Transport: tr, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrNetwork }}
	return &Client{client, base, p}, nil
}

func exchange(ctx context.Context, client *http.Client, method, address string, body []byte) (PDU, error) {
	req, e := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if e != nil {
		return PDU{}, ErrNetwork
	}
	req.Header.Set("Accept", "application/vnd.wap.mms-message")
	req.Header.Set("User-Agent", "Rykvo-Voice/1.5")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/vnd.wap.mms-message")
	}
	var wrote atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }}))
	res, e := client.Do(req)
	if e != nil {
		if !wrote.Load() && errors.Is(e, ErrNetwork) {
			return PDU{}, ErrNetwork
		}
		return PDU{}, ErrUnknown
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return PDU{}, ErrUnknown
	}
	kind, _, e := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if e != nil || kind != "application/vnd.wap.mms-message" {
		return PDU{}, ErrUnknown
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, MaxSize+1))
	if e != nil || len(b) > MaxSize {
		return PDU{}, ErrUnknown
	}
	v, e := Parse(b)
	if e != nil {
		return v, ErrUnknown
	}
	return v, nil
}
func (c *Client) Send(ctx context.Context, id, to, text string, image *Part) (string, error) {
	b, e := SendRequest(id, to, text, image)
	if e != nil {
		return "", e
	}
	client, u := c.http, c.base
	v, e := exchange(ctx, client, "POST", u.String(), b)
	if e != nil {
		return "", e
	}
	if v.Type != 0x81 || v.Transaction != id {
		return "", ErrUnknown
	}
	if v.Status != 0x80 {
		return "", errors.New("MMS_REJECTED")
	}
	if v.MessageID == "" {
		return "", ErrUnknown
	}
	return v.MessageID, nil
}
func (c *Client) Retrieve(ctx context.Context, location string) (PDU, error) {
	client := c.http
	target, e := ReceiveURL(c.profile, location)
	if e != nil {
		return PDU{}, ErrNetwork
	}
	v, e := exchange(ctx, client, "GET", target.String(), nil)
	if e != nil {
		return v, e
	}
	if v.Type != 0x84 || (v.Status != 0 && v.Status != 0x80) {
		return v, ErrPDU
	}
	return v, nil
}

// Notify the MMSC only after all retrieved content has been durably committed.
func (c *Client) Acknowledge(ctx context.Context, id string) error {
	client, u := c.http, c.base
	req, e := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(NotifyResponse(id, 0x81)))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/vnd.wap.mms-message")
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
	if res.StatusCode != 200 && res.StatusCode != 204 {
		return ErrUnknown
	}
	return nil
}
