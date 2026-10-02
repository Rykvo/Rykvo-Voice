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
			roundtrip := func(device, session string, expires, sequence int, auth string) *sip.Response {
				raw := fmt.Sprintf("REGISTER sip:%s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=z9hG4bK-wire-%d;rport\r\nFrom: <sip:1001@localhost>;tag=client\r\nTo: <sip:1001@localhost>\r\nCall-ID: %s\r\nCSeq: %d REGISTER\r\nContact: <sip:1001@%s>;expires=%d;+sip.instance=\"%s\"\r\n%sContent-Length: 0\r\n\r\n", address, strings.ToUpper(transport), conn.LocalAddr(), sequence, session, sequence, conn.LocalAddr(), expires, device, auth)
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
			for i, test := range []struct {
				device, session string
				expires, count  int
			}{
				{"A", "A-1", 300, 1}, {"B", "B-1", 300, 2}, {"A", "A-2", 300, 2},
				{"C", "C-1", 300, 3}, {"B", "B-2", 0, 2}, {"A", "A-3", 0, 1},
				{"C", "C-2", 0, 0}, {"B", "B-3", 300, 1}, {"A", "A-4", 300, 2},
			} {
				first := roundtrip(test.device, test.session, test.expires, i*2+1, "")
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
				accepted := roundtrip(test.device, test.session, test.expires, i*2+2, "Authorization: "+credential.String()+"\r\n")
				if accepted.StatusCode != 200 || len(g.registrar.Registrations()) != test.count || g.registrar.Online("test-1") != (test.count > 0) {
					t.Fatal("multi-client registration", accepted.StatusCode, test.count, g.registrar.Registrations())
				}
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
