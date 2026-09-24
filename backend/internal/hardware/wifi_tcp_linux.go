//go:build linux

package hardware

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// An isolated per-IMS stack: no TUN, host routes, privileged sockets or port forwarding.
// Only the calling goroutine reads IKE/ESP and changes SA replay/sequence state.
type wifiTCP struct {
	wire     [65536]byte
	s        *wifiIMS
	stack    *stack.Stack
	link     *channel.Endpoint
	client   net.Conn
	listener net.Listener
	protocol tcpip.NetworkProtocolNumber
	frames   chan wifiStreamFrame
	done     chan struct{}
	mu       sync.Mutex
	conns    map[net.Conn]bool
	closing  bool
	wg       sync.WaitGroup
}

func openWiFiTCP(ctx context.Context, s *wifiIMS) (*wifiTCP, error) {
	t := &wifiTCP{s: s, frames: make(chan wifiStreamFrame, 16), done: make(chan struct{}), conns: map[net.Conn]bool{}}
	t.stack = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	t.link = channel.New(128, 1280, "")
	if e := t.stack.CreateNIC(1, t.link); e != nil {
		t.close()
		return nil, errors.New("WIFI_TCP_SETUP_FAILED")
	}
	t.protocol = ipv6.ProtocolNumber
	local := s.local.To16()
	prefix := 128
	if s.local.To4() != nil {
		t.protocol = ipv4.ProtocolNumber
		local = s.local.To4()
		prefix = 32
	}
	addr := tcpip.AddrFromSlice(local)
	if e := t.stack.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: t.protocol, AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: prefix}}, stack.AddressProperties{}); e != nil {
		t.close()
		return nil, errors.New("WIFI_TCP_SETUP_FAILED")
	}
	t.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}, {Destination: header.IPv6EmptySubnet, NIC: 1}})
	if s.protection != nil {
		l, e := gonet.ListenTCP(t.stack, tcpip.FullAddress{NIC: 1, Addr: addr, Port: 49162}, t.protocol)
		if e != nil {
			t.close()
			return nil, errors.New("WIFI_TCP_LISTEN_FAILED")
		}
		t.listener = l
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			for {
				c, e := l.Accept()
				if e != nil {
					return
				}
				t.addReader(c)
			}
		}()
	}
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var conn *gonet.TCPConn
	peer := s.peer.To16()
	if t.protocol == ipv4.ProtocolNumber {
		peer = s.peer.To4()
	}
	e := t.drive(call, func() error {
		var e error
		conn, e = gonet.DialTCPWithBind(call, t.stack, tcpip.FullAddress{NIC: 1, Addr: addr, Port: 49160}, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(peer), Port: s.target}, t.protocol)
		return e
	})
	if e != nil {
		t.close()
		return nil, errors.New("WIFI_TCP_CONNECT_FAILED")
	}
	t.client = conn
	t.addReader(conn)
	return t, nil
}
func (t *wifiTCP) addReader(c net.Conn) {
	t.mu.Lock()
	if t.closing || len(t.conns) >= 3 {
		t.mu.Unlock()
		c.Close()
		return
	}
	t.conns[c] = true
	t.wg.Add(1)
	t.mu.Unlock()
	go func() {
		defer t.wg.Done()
		defer func() { c.Close(); t.mu.Lock(); delete(t.conns, c); t.mu.Unlock() }()
		reader := newWiFiSIPReader(c)
		for {
			data, e := reader.read()
			frame := wifiStreamFrame{data: data, conn: c, err: e}
			select {
			case t.frames <- frame:
			case <-t.done:
				return
			}
			if e != nil {
				return
			}
		}
	}()
}
func (t *wifiTCP) close() {
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return
	}
	t.closing = true
	close(t.done)
	if t.listener != nil {
		t.listener.Close()
	}
	for c := range t.conns {
		c.Close()
	}
	t.mu.Unlock()
	if t.stack != nil {
		t.stack.Close()
	}
	if t.link != nil {
		t.link.Close()
	}
	t.wg.Wait()
	if t.stack != nil {
		t.stack.Wait()
	}
}
func (t *wifiTCP) drive(ctx context.Context, fn func() error) error {
	result := make(chan error, 1)
	go func() { result <- fn() }()
	for {
		select {
		case e := <-result:
			return e
		default:
		}
		if e := t.pump(ctx, 10*time.Millisecond); e != nil {
			// Unblock all stream calls before joining; never leak a write/dial goroutine.
			t.stack.Close()
			<-result
			return e
		}
	}
}
func (t *wifiTCP) write(ctx context.Context, c net.Conn, data []byte) error {
	if c == nil {
		return errors.New("WIFI_TCP_CLOSED")
	}
	return t.drive(ctx, func() error {
		deadline := time.Now().Add(8 * time.Second)
		if v, ok := ctx.Deadline(); ok && v.Before(deadline) {
			deadline = v
		}
		c.SetWriteDeadline(deadline)
		for len(data) > 0 {
			n, e := c.Write(data)
			if e != nil {
				return errors.New("WIFI_TCP_WRITE_FAILED")
			}
			if n == 0 {
				return errors.New("WIFI_TCP_WRITE_FAILED")
			}
			data = data[n:]
		}
		return nil
	})
}
func (t *wifiTCP) pump(ctx context.Context, wait time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s := t.s
	s.child.ike.conn.SetWriteDeadline(time.Now().Add(time.Second))
	for i := 0; i < 128; i++ {
		p := t.link.Read()
		if p == nil {
			break
		}
		v := p.ToView()
		raw := bytes.Clone(v.AsSlice())
		v.Release()
		p.DecRef()
		kind := byte(4)
		if len(raw) > 0 && raw[0]>>4 == 6 {
			kind = 41
		}
		src, dst, proto, payload, e := wifiParseIP(raw, kind)
		if e != nil || proto != 6 || len(payload) < 20 || !src.Equal(s.local) || !dst.Equal(s.peer) {
			continue
		}
		sp, dp := binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:])
		protection := s.protection
		if sp == 49162 {
			if s.serverProtection == nil || dp != s.peerClientPort {
				continue
			}
			protection = s.serverProtection
		} else if sp != 49160 || dp != s.target {
			continue
		}
		if e = s.child.sendIP(src, dst, proto, sp, dp, raw, kind, protection); e != nil {
			return e
		}
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	s.child.ike.conn.SetReadDeadline(deadline)
	wire := t.wire[:]
	n, e := s.child.ike.conn.Read(wire)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e, ok := e.(net.Error); ok && e.Timeout() {
			return nil
		}
		return errors.New("WIFI_NETWORK_READ_FAILED")
	}
	handled, e := s.child.ike.incoming(wire[:n])
	if e != nil {
		return e
	}
	if handled {
		return nil
	}
	raw, kind, e := s.child.open(wire[:n])
	if e != nil {
		return nil
	}
	defer clear(raw)
	src, dst, proto, body, e := wifiParseIP(raw, kind)
	if e != nil || !src.Equal(s.peer) || !dst.Equal(s.local) {
		return nil
	}
	expectedPort := uint16(49160)
	remotePort := s.target
	if s.protection != nil {
		if proto != 50 || len(body) < 8 {
			return nil
		}
		protection := s.protection
		if binary.BigEndian.Uint32(body) == s.serverProtection.spiIn {
			protection = s.serverProtection
			expectedPort = 49162
			remotePort = s.peerClientPort
		}
		body, proto, e = protection.open(body)
		if e != nil {
			return nil
		}
		defer clear(body)
		raw, kind, e = wifiIP(src, dst, proto, body)
		if e != nil {
			return nil
		}
		defer clear(raw)
	}
	if proto != 6 || len(body) < 20 || binary.BigEndian.Uint16(body) != remotePort || binary.BigEndian.Uint16(body[2:]) != expectedPort {
		return nil
	}
	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(bytes.Clone(raw))})
	defer packet.DecRef()
	t.link.InjectInbound(t.protocol, packet)
	return nil
}
