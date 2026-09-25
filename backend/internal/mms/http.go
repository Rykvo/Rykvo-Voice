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
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
)

var ErrNetwork = errors.New("MMS_NETWORK_REQUIRED")
var ErrUnknown = errors.New("MMS_OUTCOME_UNKNOWN")

// Host-internet transport. Private carrier gateways need an explicitly bound
// bearer; never use the host LAN, environment proxy, or change its default route.
func publicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() {
		return false
	}
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "2001:db8::/32", "64:ff9b::/96", "2002::/16"} {
		if netip.MustParsePrefix(s).Contains(a) {
			return false
		}
	}
	return true
}
func safeURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(raw, "\r\n") {
		return nil, ErrNetwork
	}
	port := u.Port()
	if port != "" && port != "80" && port != "443" && port != "8080" && port != "8002" {
		return nil, ErrNetwork
	}
	return u, nil
}
func profileClient(p carrierconfig.Profile) (*http.Client, *url.URL, error) {
	u, e := safeURL(p.MMSC)
	if e != nil {
		return nil, nil, e
	}
	target := u
	tr := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 16384}
	if p.MMSProxy != "" {
		port := p.MMSPort
		if port == "" {
			port = "80"
		}
		if _, e := strconv.Atoi(port); e != nil {
			return nil, nil, ErrNetwork
		}
		proxy, e := safeURL("http://" + net.JoinHostPort(p.MMSProxy, port))
		if e != nil {
			return nil, nil, e
		}
		target = proxy
		tr.Proxy = http.ProxyURL(proxy)
	}
	host := target.Hostname()
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		h, port, e := net.SplitHostPort(address)
		if e != nil || !strings.EqualFold(h, host) {
			return nil, ErrNetwork
		}
		ips, e := net.DefaultResolver.LookupIP(ctx, "ip", h)
		if e != nil || len(ips) == 0 {
			return nil, ErrNetwork
		}
		// Reject the entire answer on a mixed public/private DNS response.
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, ErrNetwork
			}
		}
		for _, ip := range ips {
			c, e := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if e == nil {
				return c, nil
			}
		}
		return nil, ErrNetwork
	}
	return &http.Client{Transport: tr, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrNetwork }}, u, nil
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
func Send(ctx context.Context, p carrierconfig.Profile, id, to, text string, image *Part) (string, error) {
	b, e := SendRequest(id, to, text, image)
	if e != nil {
		return "", e
	}
	client, u, e := profileClient(p)
	if e != nil {
		return "", e
	}
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
func Retrieve(ctx context.Context, p carrierconfig.Profile, location string) (PDU, error) {
	client, u, e := profileClient(p)
	if e != nil {
		return PDU{}, e
	}
	target, e := safeURL(location)
	if e != nil || !strings.EqualFold(target.Host, u.Host) || target.Scheme != u.Scheme {
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
func Acknowledge(ctx context.Context, p carrierconfig.Profile, id string) error {
	client, u, e := profileClient(p)
	if e != nil {
		return e
	}
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
