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
	"strconv"
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
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, ErrNetwork
		}
	}
	if !BearerHost(u.Hostname()) {
		return nil, ErrNetwork
	}
	return u, nil
}

// Private carrier addresses are valid only inside the SIM-bound bearer.
func BearerHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || len(host) > 253 || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "local" || strings.HasSuffix(host, ".local") || host == "home.arpa" || strings.HasSuffix(host, ".home.arpa") {
		return false
	}
	// Reject ambiguous numeric IP spellings and modem command metacharacters.
	numeric := true
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.HasPrefix(label, "0x") {
			return false
		}
		for _, c := range label {
			if c >= 'a' && c <= 'z' || c == '-' {
				numeric = false
			} else if c < '0' || c > '9' {
				return false
			}
		}
	}
	return !numeric
}

// Notification gateways need not share the MMSC hostname, port or scheme.
// Network isolation, not a carrier-name allowlist, confines retrieval.
func ReceiveURL(p carrierconfig.Profile, raw string) (*url.URL, error) {
	if _, err := safeURL(p.MMSC); err != nil {
		return nil, ErrNetwork
	}
	return safeURL(raw)
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
			if _, e = safeURL("http://" + net.JoinHostPort(host, port)); e != nil {
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
	return SendConfirmation(v, id)
}

// A successful submission is not proof of handset delivery.
func SendConfirmation(v PDU, id string) (string, error) {
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
