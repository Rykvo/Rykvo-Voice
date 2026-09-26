//go:build linux

package ike

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

type mediaHostRoute struct {
	refs   int
	remove []string
}

type mediaRouteLease struct {
	handle *linuxUserspaceHandle
	key    string
	once   sync.Once
	err    error
}

func (h *linuxUserspaceHandle) OpenMediaRoute(ctx context.Context, local, remote *net.UDPAddr) (io.Closer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.runContext.Err() != nil || ctx.Err() != nil {
		return nil, errors.New("ike: media tunnel closed")
	}
	if !validMediaEndpoints(h.config, local, remote) {
		return nil, errors.New("ike: media endpoint outside negotiated bearer")
	}
	key := remote.IP.String()
	if h.mediaRoutes == nil {
		h.mediaRoutes = make(map[string]*mediaHostRoute)
	}
	if route := h.mediaRoutes[key]; route != nil {
		route.refs++
		return &mediaRouteLease{handle: h, key: key}, nil
	}
	if len(h.mediaRoutes) >= 32 {
		return nil, errors.New("ike: media route limit")
	}
	route := &mediaHostRoute{refs: 1}
	pcscf := false
	for _, ip := range h.config.PCSCF {
		if ip.Equal(remote.IP) {
			pcscf = true
			break
		}
	}
	if !pcscf && !h.config.DataNetwork {
		family, bits := "-6", 128
		if remote.IP.To4() != nil {
			family, bits = "-4", 32
		}
		table, _ := userspaceRoutingIdentifiers(h.config.InboundSPI)
		args := []string{family, "route", "add", "table", strconv.FormatUint(uint64(table), 10), fmt.Sprintf("%s/%d", key, bits), "dev", h.config.Name, "src", local.IP.String()}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := h.run(ctx, "install negotiated media host route", args...); err != nil {
			return nil, err
		}
		args[2] = "delete"
		route.remove = args
	}
	h.mediaRoutes[key] = route
	return &mediaRouteLease{handle: h, key: key}, nil
}

func validMediaEndpoints(c ChildSAConfig, local, remote *net.UDPAddr) bool {
	if local == nil || remote == nil || local.Port < 1 || local.Port > 65535 || remote.Port < 1 || remote.Port > 65535 || remote.Zone != "" || local.Zone != "" {
		return false
	}
	for _, ip := range []net.IP{local.IP, remote.IP} {
		if ip == nil || ip.To16() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.Equal(net.IPv4bcast) {
			return false
		}
	}
	assigned := c.InnerLocalIPv6
	if remote.IP.To4() != nil {
		assigned = c.InnerLocalIPv4
	}
	if !local.IP.Equal(assigned) {
		return false
	}
	return packetAllowed(innerPacketMetadata{source: local.IP, destination: remote.IP, protocol: 17, sourcePort: uint16(local.Port), destinationPort: uint16(remote.Port)}, c.InitiatorSelectors, c.ResponderSelectors)
}

func (lease *mediaRouteLease) Close() error {
	lease.once.Do(func() {
		h := lease.handle
		h.mu.Lock()
		defer h.mu.Unlock()
		route := h.mediaRoutes[lease.key]
		if route == nil {
			return
		}
		route.refs--
		if route.refs > 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if len(route.remove) > 0 {
			lease.err = h.run(ctx, "remove negotiated media host route", route.remove...)
			if lease.err != nil {
				return
			} // Retain ownership for tunnel cleanup/reuse.
		}
		delete(h.mediaRoutes, lease.key)
	})
	return lease.err
}

func (h *linuxUserspaceHandle) cleanupMediaRoutes(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []error
	for key, route := range h.mediaRoutes {
		if len(route.remove) > 0 {
			if err := h.run(ctx, "remove remaining media host route", route.remove...); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		delete(h.mediaRoutes, key)
	}
	return errors.Join(errs...)
}
