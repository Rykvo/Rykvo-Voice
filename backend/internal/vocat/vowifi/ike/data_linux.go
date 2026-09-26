//go:build linux

package ike

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DNS and TCP use this SIM's negotiated bearer, never the host resolver or LAN.
func (session *Session) DialData(ctx context.Context, network, address string) (net.Conn, error) {
	session.mu.Lock()
	closed := session.closed
	evidence, info := session.evidence, session.network
	session.mu.Unlock()
	if closed || evidence.DataplaneMode != "userspace" || network != "tcp" {
		return nil, errors.New("MMS_NETWORK_REQUIRED")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	dialIP := func(ctx context.Context, protocol string, ip net.IP, port string) (net.Conn, error) {
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			return nil, errors.New("MMS_NETWORK_REQUIRED")
		}
		local, family := net.ParseIP(info.LocalIPv6), "6"
		if ip.To4() != nil {
			local, family = net.ParseIP(info.LocalIPv4), "4"
		}
		if local == nil {
			return nil, errors.New("MMS_NETWORK_REQUIRED")
		}
		var addr net.Addr = &net.TCPAddr{IP: local}
		if strings.HasPrefix(protocol, "udp") {
			addr = &net.UDPAddr{IP: local}
			protocol = "udp"
		} else {
			protocol = "tcp"
		}
		dialer := net.Dialer{Timeout: 8 * time.Second, LocalAddr: addr, Control: func(_, _ string, raw syscall.RawConn) error {
			var bindErr error
			if err := raw.Control(func(fd uintptr) {
				bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, evidence.Name)
			}); err != nil {
				return err
			}
			return bindErr
		}}
		return dialer.DialContext(ctx, protocol+family, net.JoinHostPort(ip.String(), port))
	}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, protocol, _ string) (net.Conn, error) {
			for _, dns := range info.DNS {
				if conn, err := dialIP(ctx, protocol, net.ParseIP(dns), "53"); err == nil {
					return conn, nil
				}
			}
			return nil, errors.New("MMS_NETWORK_REQUIRED")
		}}
		family := "ip"
		if info.LocalIPv4 == "" {
			family = "ip6"
		} else if info.LocalIPv6 == "" {
			family = "ip4"
		}
		ips, err = resolver.LookupIP(ctx, family, host)
		if err != nil {
			return nil, errors.New("MMS_NETWORK_REQUIRED")
		}
	}
	for _, ip := range ips {
		if conn, err := dialIP(ctx, "tcp", ip, port); err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("MMS_NETWORK_REQUIRED")
}
