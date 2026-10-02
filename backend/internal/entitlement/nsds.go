// Package entitlement obtains carrier-owned web sessions. It never saves an
// emergency address, accepts terms or exposes SIM authentication material.
package entitlement

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
)

var (
	ErrUnavailable = errors.New("EMERGENCY_UNAVAILABLE")
	ErrRejected    = errors.New("EMERGENCY_REJECTED")
	ErrInvalid     = errors.New("EMERGENCY_INVALID_RESPONSE")
)

type Page struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (p Page) Valid(provider carrierconfig.EmergencyProvider) bool {
	u, err := url.Parse(p.URL)
	return err == nil && len(p.URL) <= 2048 && u.Scheme == "https" && u.Host == provider.PageHost && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.ContainsAny(p.URL, "\\\x00\r\n") && u.RawPath == "" && path.Clean(u.Path) == u.Path && strings.HasPrefix(u.Path, provider.PagePrefix) && len(p.Token) > 0 && len(p.Token) <= 16384 && !strings.ContainsAny(p.Token, "\x00\r\n")
}

type AKA interface {
	Respond(context.Context, []byte) ([]byte, error)
	Verified() bool
}

// Discover uses a fresh HTTPS session and the module's real identity. Neither
// HTTP redirects nor environment proxies can forward subscriber credentials.
func Discover(ctx context.Context, provider carrierconfig.EmergencyProvider, identity, imei string, aka AKA) (Page, error) {
	registered, ok := carrierconfig.EmergencyByID(provider.ID)
	if !ok || registered != provider {
		return Page{}, ErrInvalid
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxConnsPerHost = 1
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.DialContext = publicDial
	defer transport.CloseIdleConnections()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Transport: transport, Jar: jar, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return discover(ctx, client, provider, identity, imei, aka)
}

func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, ErrUnavailable
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, ErrUnavailable
	}
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			return nil, ErrUnavailable
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if e == nil {
			return conn, nil
		}
	}
	return nil, ErrUnavailable
}

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, cidr := range []string{"100.64.0.0/10", "198.18.0.0/15", "192.0.0.0/24", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

type response struct {
	ID          int    `json:"message-id"`
	Code        int    `json:"response-code"`
	Challenge   string `json:"aka-challenge"`
	Token       string `json:"aka-token"`
	Fingerprint string `json:"service-fingerprint"`
	URL         string `json:"server-url"`
	Data        string `json:"server-data"`
}
type request map[string]any

func discover(ctx context.Context, client *http.Client, provider carrierconfig.EmergencyProvider, identity, imei string, aka AKA) (Page, error) {
	if aka == nil || len(identity) < 16 || len(identity) > 128 || len(imei) != 15 || strings.ContainsAny(identity+imei, "\r\n\x00") {
		return Page{}, ErrInvalid
	}
	device := base64.StdEncoding.EncodeToString([]byte(imei))
	auth := request{"message-id": 1, "method": "3gppAuthentication", "device-id": device, "device-type": 0, "os-type": 0, "device-name": "EC20", "imsi-eap": identity}
	authenticated := false
	for round := 0; round < 4; round++ {
		items, err := post(ctx, client, provider.Endpoint, []request{auth})
		if err != nil {
			return Page{}, err
		}
		if len(items) != 1 || items[0].ID != 1 {
			return Page{}, ErrInvalid
		}
		r := items[0]
		if len(r.Token) > 16384 || strings.ContainsAny(r.Token, "\x00\r\n") {
			return Page{}, ErrInvalid
		}
		if r.Token != "" {
			auth["aka-token"] = r.Token
		}
		if r.Code == 1000 {
			if !aka.Verified() {
				return Page{}, ErrInvalid
			}
			authenticated = true
			break
		}
		if r.Code != 1003 {
			return Page{}, ErrRejected
		}
		packet, err := base64.StdEncoding.DecodeString(r.Challenge)
		if err != nil || len(packet) > 4096 {
			clear(packet)
			return Page{}, ErrInvalid
		}
		reply, err := aka.Respond(ctx, packet)
		clear(packet)
		if err != nil {
			clear(reply)
			return Page{}, ErrRejected
		}
		auth["aka-challenge-rsp"] = base64.StdEncoding.EncodeToString(reply)
		clear(reply)
	}
	if !authenticated {
		return Page{}, ErrRejected
	}
	delete(auth, "aka-challenge-rsp")
	items, err := post(ctx, client, provider.Endpoint, []request{auth, {"message-id": 2, "method": "getMSISDN", "device-id": device}})
	if err != nil {
		return Page{}, err
	}
	line, err := paired(items, 2)
	if err != nil || line.Fingerprint == "" || len(line.Fingerprint) > 4096 {
		return Page{}, ErrRejected
	}
	items, err = post(ctx, client, provider.Endpoint, []request{auth, {"message-id": 3, "method": "manageLocationAndTC", "device-id": device, "service-fingerprint": line.Fingerprint}})
	if err != nil {
		return Page{}, err
	}
	location, err := paired(items, 3)
	if err != nil {
		return Page{}, err
	}
	page := Page{URL: location.URL, Token: location.Data}
	if !page.Valid(provider) {
		return Page{}, ErrInvalid
	}
	return page, nil
}

func paired(items []response, id int) (response, error) {
	if len(items) != 2 {
		return response{}, ErrInvalid
	}
	if items[0].ID == id {
		items[0], items[1] = items[1], items[0]
	}
	if items[0].ID != 1 || items[1].ID != id {
		return response{}, ErrInvalid
	}
	if items[0].Code != 1000 || items[1].Code != 1000 {
		return response{}, ErrRejected
	}
	return items[1], nil
}

func post(ctx context.Context, client *http.Client, endpoint string, items []request) ([]response, error) {
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, ErrInvalid
	}
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	_, _ = gz.Write(raw)
	_ = gz.Close()
	clear(raw)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, &body)
	if err != nil {
		return nil, ErrInvalid
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Accept": "application/json", "Accept-Encoding": "gzip", "Content-Encoding": "gzip", "x-generic-protocol-version": "1.0", "x-generic-version": "1.0", "x-protocol-version": "1", "User-Agent": "Rykvo-Voice (Linux; EC20)"} {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	var reader io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		z, e := gzip.NewReader(resp.Body)
		if e != nil {
			return nil, ErrInvalid
		}
		defer z.Close()
		reader = z
	case "":
	default:
		return nil, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(reader, 256*1024+1))
	defer clear(data)
	if err != nil || len(data) > 256*1024 {
		return nil, ErrInvalid
	}
	var out []response
	if json.Unmarshal(data, &out) != nil || len(out) > 2 {
		return nil, ErrInvalid
	}
	return out, nil
}
