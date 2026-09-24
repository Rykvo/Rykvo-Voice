package hardware

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Production CI roots from the pinned euicc-go dependency, plus system Web PKI.
//
//go:embed esim-roots.pem
var esimRoots []byte

func publicAddress(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsUnspecified() &&
		!inNetwork(ip, "100.64.0.0/10") && !inNetwork(ip, "198.18.0.0/15") &&
		!inNetwork(ip, "192.0.0.0/24") && !inNetwork(ip, "2001:db8::/32")
}
func inNetwork(ip net.IP, cidr string) bool {
	_, network, _ := net.ParseCIDR(cidr)
	return network.Contains(ip)
}
func validESIMURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		u.Opaque == "" && u.Fragment == "" && u.RawQuery == "" &&
		(u.Port() == "" || u.Port() == "443") && !strings.ContainsAny(u.Host, " \\\t\r\n")
}

type esimTransport struct {
	ctx       context.Context
	transport *http.Transport
}

func (t *esimTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !validESIMURL(r.URL) || r.Method != http.MethodPost {
		return nil, errors.New("ESIM_REMOTE_ADDRESS")
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	defer func() { stop(); cancel() }()
	r = r.Clone(ctx)
	response, err := t.transport.RoundTrip(r)
	if err != nil {
		return nil, errors.New("ESIM_NETWORK_FAILED")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return nil, errors.New("ESIM_INVALID_RESPONSE")
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}
func esimHTTP(ctx context.Context) (*http.Client, func()) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	roots.AppendCertsFromPEM(esimRoots)
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "443" {
				return nil, errors.New("ESIM_REMOTE_ADDRESS")
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("ESIM_NETWORK_FAILED")
			}
			for _, ip := range ips {
				if !publicAddress(ip.IP) {
					return nil, errors.New("ESIM_REMOTE_ADDRESS")
				}
			}
			dialer := net.Dialer{Timeout: 10 * time.Second}
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
			}
			return nil, errors.New("ESIM_NETWORK_FAILED")
		},
	}
	return &http.Client{Timeout: 45 * time.Second, Transport: &esimTransport{ctx, transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("ESIM_REMOTE_ADDRESS") }}, transport.CloseIdleConnections
}
