//go:build linux

package hardware

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"net"
	"testing"
	"time"
)

func TestWiFiTCPOverESP(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		for _, protected := range []bool{false, true} {
			t.Run(map[bool]string{false: "ipv4", true: "ipv6"}[v6]+"/"+map[bool]string{false: "plain", true: "protected"}[protected], func(t *testing.T) {
				a, b := espPair(false)
				a.tsi = []wifiSelector{{first: 0, last: 65535, start: net.IP{0, 0, 0, 0}, end: net.IP{255, 255, 255, 255}}}
				if v6 {
					a.tsi = []wifiSelector{{first: 0, last: 65535, start: make(net.IP, 16), end: bytes.Repeat([]byte{255}, 16)}}
				}
				a.tsr = a.tsi
				serverSock, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if e != nil {
					t.Fatal(e)
				}
				defer serverSock.Close()
				clientSock, e := net.DialUDP("udp4", nil, serverSock.LocalAddr().(*net.UDPAddr))
				if e != nil {
					t.Fatal(e)
				}
				defer clientSock.Close()
				a.ike = &wifiIKE{conn: clientSock}
				s := &wifiIMS{child: a, local: net.IP{192, 0, 2, 1}, peer: net.IP{192, 0, 2, 2}, target: 5060, transport: "tcp"}
				proto := tcpip.NetworkProtocolNumber(ipv4.ProtocolNumber)
				prefix := 32
				kindOut := byte(4)
				if v6 {
					s.local = net.ParseIP("2001:db8::1")
					s.peer = net.ParseIP("2001:db8::2")
					proto = ipv6.ProtocolNumber
					prefix = 128
					kindOut = 41
				}
				var innerPeer, serverPeer *wifiChild
				if protected {
					s.protection, innerPeer = espPair(true)
					s.serverProtection, serverPeer = espPair(true)
					s.serverProtection.spiIn += 100
					s.serverProtection.spiOut += 100
					serverPeer.spiIn = s.serverProtection.spiOut
					serverPeer.spiOut = s.serverProtection.spiIn
					s.peerClientPort = 5062
				}
				st := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
				defer st.Close()
				link := channel.New(128, 1280, "")
				defer link.Close()
				if e := st.CreateNIC(1, link); e != nil {
					t.Fatal(e)
				}
				if e := st.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: proto, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(s.peer), PrefixLen: prefix}}, stack.AddressProperties{}); e != nil {
					t.Fatal(e)
				}
				st.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}, {Destination: header.IPv6EmptySubnet, NIC: 1}})
				listener, e := gonet.ListenTCP(st, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(s.peer), Port: 5060}, proto)
				if e != nil {
					t.Fatal(e)
				}
				defer listener.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					buf := make([]byte, 65536)
					for ctx.Err() == nil {
						for p := link.Read(); p != nil; p = link.Read() {
							v := p.ToView()
							ip := bytes.Clone(v.AsSlice())
							v.Release()
							p.DecRef()
							if protected {
								_, _, _, tcpBody, _ := wifiParseIP(ip, kindOut)
								peer := innerPeer
								if binary.BigEndian.Uint16(tcpBody) == s.peerClientPort {
									peer = serverPeer
								}
								esp, _ := peer.seal(tcpBody, 6)
								ip, _, _ = wifiIP(s.peer, s.local, 50, esp)
							}
							wire, _ := b.seal(append(bytes.Clone(ip), bytes.Repeat([]byte{0xa5}, 46)...), kindOut)
							serverSock.WriteToUDP(wire, clientSock.LocalAddr().(*net.UDPAddr))
						}
						serverSock.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
						n, _, e := serverSock.ReadFromUDP(buf)
						if e != nil {
							continue
						}
						ip, kind, e := b.open(buf[:n])
						if e != nil {
							continue
						}
						if protected {
							_, _, proto, esp, e := wifiParseIP(ip, kind)
							if e != nil || proto != 50 || len(esp) < 8 {
								continue
							}
							peer := innerPeer
							if binary.BigEndian.Uint32(esp) == serverPeer.spiIn {
								peer = serverPeer
							}
							body, proto, e := peer.open(esp)
							if e != nil {
								continue
							}
							ip, _, _ = wifiIP(s.local, s.peer, proto, body)
						}
						p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(ip)})
						link.InjectInbound(proto, p)
						p.DecRef()
					}
				}()
				received := make(chan error, 1)
				go func() {
					c, e := listener.Accept()
					if e != nil {
						received <- e
						return
					}
					defer c.Close()
					c.SetDeadline(time.Now().Add(5 * time.Second))
					msg, e := newWiFiSIPReader(c).read()
					if e == nil {
						_, e = c.Write(msg[:10])
						if e == nil {
							_, e = c.Write(msg[10:])
						}
					}
					received <- e
				}()
				tr, e := openWiFiTCP(ctx, s)
				if e != nil {
					cancel()
					<-done
					t.Fatal(e)
				}
				s.tcp = tr
				msg := []byte("SIP/2.0 200 OK\r\nContent-Length: 3\r\n\r\nabc")
				got, e := s.tcpExchange(ctx, msg, func(b []byte) bool { return bytes.Equal(b, msg) })
				if e == nil && protected {
					inbound := make(chan error, 1)
					go func() {
						c, err := gonet.DialTCPWithBind(ctx, st, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(s.peer), Port: s.peerClientPort}, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(s.local), Port: 49162}, proto)
						if err != nil {
							inbound <- err
							return
						}
						defer c.Close()
						c.SetDeadline(time.Now().Add(3 * time.Second))
						if _, err = c.Write(msg); err == nil {
							var reply []byte
							reply, err = newWiFiSIPReader(c).read()
							if err == nil && !bytes.Equal(reply, msg) {
								err = errors.New("wrong server stream reply")
							}
						}
						inbound <- err
					}()
				serverLoop:
					for {
						select {
						case err := <-inbound:
							e = err
							break serverLoop
						case f := <-tr.frames:
							if f.err == nil && f.conn != tr.client {
								e = tr.write(ctx, f.conn, f.data)
								clear(f.data)
							}
						default:
							e = tr.pump(ctx, 5*time.Millisecond)
						}
						if e != nil {
							break
						}
					}
				}
				tr.close()
				cancel()
				<-done
				if e != nil || !bytes.Equal(got, msg) {
					t.Fatal("TCP stream over ESP", e)
				}
				if e := <-received; e != nil {
					t.Fatal(e)
				}
			})
		}
	}

}
