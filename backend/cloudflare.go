package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type tunnelError string

func (e tunnelError) Error() string { return string(e) }

var cfID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var tunnelID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var hostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func normalizeDomain(value string) string {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if len(value) > 253 || net.ParseIP(value) != nil {
		return ""
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return ""
	}
	for _, part := range parts {
		if !hostLabel.MatchString(part) {
			return ""
		}
	}
	switch parts[len(parts)-1] {
	case "localhost", "local", "internal", "test", "invalid", "example":
		return ""
	}
	return value
}
func authorizationURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "dash.cloudflare.com" || u.User != nil || u.Path != "/argotunnel" || u.RawQuery == "" {
		return ""
	}
	return u.String()
}

type originCertificate struct {
	ZoneID    string `json:"zoneID"`
	AccountID string `json:"accountID"`
	APIToken  string `json:"apiToken"`
}

func readCertificate(path string) (originCertificate, error) {
	var cert originCertificate
	data, err := os.ReadFile(path)
	if err != nil {
		return cert, tunnelError("AUTH_REQUIRED")
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "ARGO TUNNEL TOKEN" || json.Unmarshal(block.Bytes, &cert) != nil || !cfID.MatchString(cert.ZoneID) || !cfID.MatchString(cert.AccountID) || cert.APIToken == "" {
		return cert, tunnelError("AUTH_INVALID")
	}
	return cert, nil
}

type cfRequestError struct {
	code         tunnelError
	status       int
	providerCode int
	retryAfter   time.Duration
}

func (e *cfRequestError) Error() string { return string(e.code) }
func (e *cfRequestError) Unwrap() error { return e.code }

type cfClient struct {
	base string
	http *http.Client
	cert originCertificate
	wait func(context.Context, time.Duration) error
}

func newCFClient(cert originCertificate) *cfClient {
	return &cfClient{base: "https://api.cloudflare.com/client/v4", cert: cert, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect rejected") }}}
}
func (c *cfClient) request(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cert.APIToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &cfRequestError{code: "CLOUDFLARE_UNAVAILABLE"}
	}
	defer res.Body.Close()
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&envelope)
	failure := &cfRequestError{code: "CLOUDFLARE_REQUEST_FAILED", status: res.StatusCode}
	if len(envelope.Errors) > 0 {
		failure.providerCode = envelope.Errors[0].Code
	}
	if seconds, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && seconds > 0 {
		failure.retryAfter = time.Duration(min(seconds, 300)) * time.Second
	} else if until, err := http.ParseTime(res.Header.Get("Retry-After")); err == nil {
		failure.retryAfter = min(time.Until(until), 5*time.Minute)
	}
	switch {
	case res.StatusCode == 404:
		failure.code = "RESOURCE_NOT_FOUND"
	case res.StatusCode == 401 || res.StatusCode == 403:
		failure.code = "CLOUDFLARE_PERMISSION"
	case res.StatusCode == 429:
		failure.code = "CLOUDFLARE_RATE_LIMITED"
	case res.StatusCode == 408 || res.StatusCode >= 500:
		failure.code = "CLOUDFLARE_UNAVAILABLE"
	case res.StatusCode >= 200 && res.StatusCode < 300 && decodeErr != nil:
		failure.code = "CLOUDFLARE_RESPONSE_INVALID"
	case res.StatusCode >= 200 && res.StatusCode < 300 && envelope.Success:
		if result != nil && json.Unmarshal(envelope.Result, result) != nil {
			return tunnelError("CLOUDFLARE_RESPONSE_INVALID")
		}
		return nil
	}
	if method == "DELETE" && strings.HasPrefix(path, c.tunnelsPath()+"/") && failure.status == 400 && failure.providerCode == 1022 {
		failure.code = "TUNNEL_CONNECTIONS_ACTIVE"
	}
	return failure
}

type cfTunnel struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	DeletedAt *time.Time `json:"deleted_at"`
}
type cfDNS struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	Proxied bool   `json:"proxied"`
}

func (c *cfClient) tunnelsPath() string { return "/accounts/" + c.cert.AccountID + "/cfd_tunnel" }
func (c *cfClient) dnsPath() string     { return "/zones/" + c.cert.ZoneID + "/dns_records" }
func (c *cfClient) checkZone(ctx context.Context, domain string) error {
	var zone struct {
		Name    string `json:"name"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := c.request(ctx, "GET", "/zones/"+c.cert.ZoneID, nil, &zone); err != nil {
		return err
	}
	if zone.Account.ID != c.cert.AccountID || (domain != zone.Name && !strings.HasSuffix(domain, "."+zone.Name)) {
		return tunnelError("DOMAIN_NOT_AUTHORIZED")
	}
	return nil
}
func (c *cfClient) findTunnel(ctx context.Context, name string) (string, error) {
	var list []cfTunnel
	if err := c.request(ctx, "GET", c.tunnelsPath()+"?is_deleted=false&name="+url.QueryEscape(name), nil, &list); err != nil {
		return "", err
	}
	for _, item := range list {
		if item.Name == name && item.DeletedAt == nil {
			if !tunnelID.MatchString(item.ID) {
				return "", tunnelError("CLOUDFLARE_RESPONSE_INVALID")
			}
			return item.ID, nil
		}
	}
	return "", nil
}
func (c *cfClient) ensureTunnel(ctx context.Context, name string, secret []byte) (string, error) {
	id, err := c.findTunnel(ctx, name)
	if err != nil || id != "" {
		return id, err
	}
	var item cfTunnel
	err = c.request(ctx, "POST", c.tunnelsPath(), map[string]any{"name": name, "tunnel_secret": secret, "config_src": "local"}, &item)
	if err != nil {
		return "", err
	}
	if !tunnelID.MatchString(item.ID) || item.Name != name {
		return "", tunnelError("CLOUDFLARE_RESPONSE_INVALID")
	}
	return item.ID, nil
}
func (c *cfClient) ensureDNS(ctx context.Context, b tunnelBinding) (string, error) {
	var list []cfDNS
	if err := c.request(ctx, "GET", c.dnsPath()+"?name="+url.QueryEscape(b.Domain), nil, &list); err != nil {
		return "", err
	}
	for _, item := range list {
		if item.Name != b.Domain {
			return "", tunnelError("DNS_CONFLICT")
		}
		if item.Type == "CNAME" && item.Content == b.TunnelID+".cfargotunnel.com" && item.Comment == b.Name && item.Proxied && cfID.MatchString(item.ID) {
			return item.ID, nil
		}
		return "", tunnelError("DNS_CONFLICT")
	}
	var item cfDNS
	err := c.request(ctx, "POST", c.dnsPath(), map[string]any{"type": "CNAME", "name": b.Domain, "content": b.TunnelID + ".cfargotunnel.com", "proxied": true, "ttl": 1, "comment": b.Name}, &item)
	if err != nil {
		return "", err
	}
	if !cfID.MatchString(item.ID) {
		return "", tunnelError("CLOUDFLARE_RESPONSE_INVALID")
	}
	return item.ID, nil
}
func (c *cfClient) removeOwned(ctx context.Context, b tunnelBinding) error {
	step := func(name string, run func() error) error { return cleanupStep(ctx, name, c.wait, run) }
	// 先校验归属，再清理连接；不使用级联删除影响其他路由。
	if b.TunnelID == "" {
		if err := step("find_tunnel", func() error {
			var err error
			b.TunnelID, err = c.findTunnel(ctx, b.Name)
			return err
		}); err != nil {
			return err
		}
	}
	if b.TunnelID == "" {
		return nil
	}
	if !tunnelID.MatchString(b.TunnelID) {
		return &cleanupError{"check_tunnel", tunnelError("RESOURCE_OWNERSHIP_MISMATCH")}
	}
	var item cfTunnel
	exists := true
	if err := step("check_tunnel", func() error {
		err := c.request(ctx, "GET", c.tunnelsPath()+"/"+b.TunnelID, nil, &item)
		if errors.Is(err, tunnelError("RESOURCE_NOT_FOUND")) {
			exists = false
			return nil
		}
		if err != nil {
			return err
		}
		if item.ID != b.TunnelID || item.Name != b.Name {
			return tunnelError("RESOURCE_OWNERSHIP_MISMATCH")
		}
		exists = item.DeletedAt == nil
		return nil
	}); err != nil {
		return err
	}
	var list []cfDNS
	if err := step("list_dns", func() error {
		return c.request(ctx, "GET", c.dnsPath()+"?name="+url.QueryEscape(b.Domain), nil, &list)
	}); err != nil {
		return err
	}
	for _, record := range list {
		if record.Name != b.Domain || record.Type != "CNAME" || record.Content != b.TunnelID+".cfargotunnel.com" || record.Comment != b.Name {
			continue
		}
		if !cfID.MatchString(record.ID) {
			return &cleanupError{"delete_dns", tunnelError("CLOUDFLARE_RESPONSE_INVALID")}
		}
		if err := step("delete_dns", func() error {
			return ignoreMissing(c.request(ctx, "DELETE", c.dnsPath()+"/"+record.ID, nil, nil))
		}); err != nil {
			return err
		}
	}
	if !exists {
		return nil
	}
	if err := step("clear_connections", func() error {
		return ignoreMissing(c.request(ctx, "DELETE", c.tunnelsPath()+"/"+b.TunnelID+"/connections", nil, nil))
	}); err != nil {
		return err
	}
	return step("delete_tunnel", func() error {
		return ignoreMissing(c.request(ctx, "DELETE", c.tunnelsPath()+"/"+b.TunnelID, nil, nil))
	})
}

func ignoreMissing(err error) error {
	if errors.Is(err, tunnelError("RESOURCE_NOT_FOUND")) {
		return nil
	}
	return err
}
