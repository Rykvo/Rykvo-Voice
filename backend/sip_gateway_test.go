package main

import (
	"bufio"
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"rykvo.local/auth/internal/sipregistrar"
)

func TestSIPGatewayUDPAndTCPRegistration(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			s := &server{}
			g := newSIPGateway(s)
			reserve, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := reserve.Addr().String()
			port := reserve.Addr().(*net.TCPAddr).Port
			reserve.Close()
			a := md5.Sum([]byte("1001:rykvo:test-secret"))
			b := sha256.Sum256([]byte("1001:rykvo:test-secret"))
			g.registrar.Replace([]sipregistrar.Account{{ID: "test-1", Username: "1001", Port: port, Revision: 1, MD5: a[:], SHA256: b[:]}})
			listener, err := g.listen(address, port)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.close()
			conn, err := net.Dial(transport, address)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			reader := bufio.NewReader(conn)
			roundtrip := func(sequence int, auth string) *sip.Response {
				raw := fmt.Sprintf("REGISTER sip:%s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=z9hG4bK-wire-%d;rport\r\nFrom: <sip:1001@localhost>;tag=client\r\nTo: <sip:1001@localhost>\r\nCall-ID: wire-test\r\nCSeq: %d REGISTER\r\nContact: <sip:1001@%s>;expires=300\r\n%sContent-Length: 0\r\n\r\n", address, strings.ToUpper(transport), conn.LocalAddr(), sequence, sequence, conn.LocalAddr(), auth)
				if _, err := io.WriteString(conn, raw); err != nil {
					t.Fatal(err)
				}
				var wire []byte
				if transport == "udp" {
					wire = make([]byte, 8192)
					n, err := conn.Read(wire)
					if err != nil {
						t.Fatal(err)
					}
					wire = wire[:n]
				} else {
					for {
						line, err := reader.ReadString('\n')
						if err != nil {
							t.Fatal(err)
						}
						wire = append(wire, line...)
						if line == "\r\n" {
							break
						}
					}
				}
				parsed, err := sip.ParseMessage(wire)
				if err != nil {
					t.Fatal(err)
				}
				return parsed.(*sip.Response)
			}
			first := roundtrip(1, "")
			if first.StatusCode != 401 {
				t.Fatal(first.StatusCode)
			}
			challenge, err := digest.ParseChallenge(first.GetHeaders("WWW-Authenticate")[0].Value())
			if err != nil {
				t.Fatal(err)
			}
			credential, err := digest.Digest(challenge, digest.Options{Method: "REGISTER", URI: "sip:" + address, Username: "1001", Password: "test-secret", Count: 1, Cnonce: "wire-cnonce"})
			if err != nil {
				t.Fatal(err)
			}
			accepted := roundtrip(2, "Authorization: "+credential.String()+"\r\n")
			if accepted.StatusCode != 200 || !g.registrar.Online("test-1") {
				t.Fatal(accepted.StatusCode)
			}
		})
	}
}
func TestLocalSIPAddressesArePrivateUnicast(t *testing.T) {
	for _, host := range localSIPAddresses() {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.To4() == nil {
			t.Fatal(strconv.Quote(host))
		}
	}
}

func TestSIPGatewayOccupiedPortRejected(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			var port int
			if transport == "udp" {
				c, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				port = c.LocalAddr().(*net.UDPAddr).Port
			} else {
				c, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				port = c.Addr().(*net.TCPAddr).Port
			}
			g := newSIPGateway(&server{})
			if g.checkPort(port, sipAccountNetwork{Mode: "cloud", BindAddress: "127.0.0.1"}) == nil {
				t.Fatal("occupied port accepted")
			}
		})
	}
}
