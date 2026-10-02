package ims

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSIPStreamLeadingCRLF(t *testing.T) {
	for _, prefix := range []string{"\r\n", "\r\n\r\n", strings.Repeat("\r\n", 8)} {
		reader := bufio.NewReader(strings.NewReader(prefix + "SIP/2.0 200 OK\r\nContent-Length: 4\r\n\r\nbody" + "\r\nMESSAGE sip:u@example.test SIP/2.0\r\nContent-Length: 4\r\n\r\n\r\n\r\n"))
		first, err := readSIPPacket(reader, nil)
		if err != nil || first.Response == nil || string(first.Response.Body) != "body" {
			t.Fatalf("prefix %q: response=%v err=%v", prefix, first.Response, err)
		}
		second, err := readSIPPacket(reader, nil)
		if err != nil || second.Request == nil || string(second.Request.Body) != "\r\n\r\n" {
			t.Fatalf("body treated as keepalive: request=%v err=%v", second.Request, err)
		}
	}
}

func TestSIPInitialRegistrationKeepalive(t *testing.T) {
	client, network := net.Pipe()
	defer client.Close()
	defer network.Close()
	_ = network.SetDeadline(time.Now().Add(2 * time.Second))
	session := &Session{conn: client, reader: bufio.NewReader(client), transport: "tcp", callID: "register-test",
		provider: &Provider{config: Config{TransactionTimeout: time.Second}}}
	done := make(chan error, 1)
	go func() {
		response, err := session.exchange(context.Background(), []byte("REGISTER sip:example.test SIP/2.0\r\nContent-Length: 0\r\n\r\n"), 1)
		if err == nil && response.StatusCode != 200 {
			err = errors.New("unexpected response")
		}
		done <- err
	}()
	if _, err := readSIPPacket(bufio.NewReader(network), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(network, "\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var pong [2]byte
	if _, err := io.ReadFull(network, pong[:]); err != nil || string(pong[:]) != "\r\n" {
		t.Fatal(pong, err)
	}
	if _, err := io.WriteString(network, "\r\nSIP/2.0 200 OK\r\nCall-ID: register-test\r\nCSeq: 1 REGISTER\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func FuzzSIPStreamFraming(f *testing.F) {
	for _, value := range []string{"\r\n\r\nSIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n", "\r\n", "MESSAGE sip:u@example.test SIP/2.0\r\nContent-Length: 4\r\n\r\n\r\n\r\n"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 2<<20 {
			t.Skip()
		}
		_, _ = readSIPPacket(bufio.NewReaderSize(strings.NewReader(value), 128), func() error { return nil })
	})
}

func TestSIPStreamKeepaliveReplyAndLimits(t *testing.T) {
	for _, blanks := range []int{1, 2, 3, 8, 10000} {
		pongs := 0
		packet, err := readSIPPacket(bufio.NewReader(strings.NewReader(strings.Repeat("\r\n", blanks)+"SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n")), func() error { pongs++; return nil })
		if err != nil || packet.Response == nil || pongs != blanks/2 {
			t.Fatalf("blanks=%d pongs=%d err=%v", blanks, pongs, err)
		}
	}
	want := errors.New("write failed")
	if _, err := readSIPPacket(bufio.NewReader(strings.NewReader("\r\n\r\n")), func() error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	for _, input := range []string{
		strings.Repeat("x", maxSIPHeaderBytes+1),
		"SIP/2.0 200 OK\r\nX: " + strings.Repeat("x", maxSIPHeaderBytes) + "\r\n\r\n",
		"garbage\r\n\r\n", "\nSIP/2.0 200 OK\r\n\r\n",
		"SIP/2.0 200 OK\r\nContent-Length: 4\r\n\r\nx",
		"SIP/2.0 200 OK\r\nContent-Length: 1048577\r\n\r\n",
	} {
		if _, err := readSIPPacket(bufio.NewReaderSize(strings.NewReader(input), 64), nil); err == nil {
			t.Fatalf("accepted invalid stream (length %d)", len(input))
		}
	}
}

func TestSIPRuntimeFragmentedKeepalive(t *testing.T) {
	for _, inbound := range []bool{false, true} {
		t.Run(map[bool]string{false: "main", true: "protected"}[inbound], func(t *testing.T) {
			client, network := net.Pipe()
			defer client.Close()
			defer network.Close()
			_ = network.SetDeadline(time.Now().Add(3 * time.Second))
			key := sipTransactionKey{callID: "keepalive-test", cseq: 1, method: "REGISTER"}
			responses := make(chan *sipResponse, 2)
			session := &Session{conn: client, reader: bufio.NewReader(client), transport: "tcp", failures: make(chan error, 1),
				transactions: map[sipTransactionKey]chan *sipResponse{key: responses}, inboundConnections: map[net.Conn]struct{}{client: {}}}
			session.receiveDone.Add(1)
			if inbound {
				go session.readInboundTCP(client)
			} else {
				go session.readMainConnection()
			}
			defer session.receiveDone.Wait()
			defer client.Close()
			for n := 0; n < 3; n++ {
				for _, fragment := range []string{"\r", "\n\r", "\n"} {
					if _, err := io.WriteString(network, fragment); err != nil {
						t.Fatal(err)
					}
				}
				var pong [2]byte
				if _, err := io.ReadFull(network, pong[:]); err != nil || string(pong[:]) != "\r\n" {
					t.Fatalf("ping without subsequent SIP: %q %v", pong, err)
				}
			}
			// A single pong must not elicit another pong or delay the next packet.
			if _, err := io.WriteString(network, "\r\nSIP/2.0 200 OK\r\nCall-ID: keepalive-test\r\nCSeq: 1 REGISTER\r\nContent-Length: 4\r\n\r\n\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case response := <-responses:
				if string(response.Body) != "\r\n\r\n" {
					t.Fatal("body changed")
				}
			case failure := <-session.failures:
				t.Fatal(failure)
			case <-time.After(time.Second):
				t.Fatal("SIP packet not dispatched after keepalive")
			}
		})
	}
}
