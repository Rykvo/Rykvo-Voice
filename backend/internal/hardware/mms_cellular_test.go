package hardware

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
)

type mmsTestPort struct {
	bytes.Buffer
	commands []string
	onWrite  func([]byte) string
}

func (p *mmsTestPort) Write(b []byte) (int, error) {
	p.commands = append(p.commands, string(b))
	p.Buffer.WriteString(p.onWrite(b))
	return len(b), nil
}
func (p *mmsTestPort) Close() error { return nil }
func testMMSProfile() carrierconfig.Profile {
	return carrierconfig.Profile{APN: "cmwap", MMSC: "http://mmsc.example.test", MMSProxy: "10.0.0.172", MMSPort: "80"}
}

func TestMMSProfileBounds(t *testing.T) {
	p := testMMSProfile()
	if _, _, e := mmsProfile(p); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*carrierconfig.Profile){
		func(p *carrierconfig.Profile) { p.APN = "ims" }, func(p *carrierconfig.Profile) { p.APN = "sos" },
		func(p *carrierconfig.Profile) { p.User = "x\rAT+CFUN=0" }, func(p *carrierconfig.Profile) { p.Password = "x\"" },
		func(p *carrierconfig.Profile) { p.MMSC = "https://mmsc.example.test" }, func(p *carrierconfig.Profile) { p.MMSC = "http://user@host" },
		func(p *carrierconfig.Profile) { p.MMSProxy = "10.0.0.172\r" }, func(p *carrierconfig.Profile) { p.MMSPort = "0" },
		func(p *carrierconfig.Profile) { p.Auth = "9" },
	} {
		q := p
		change(&q)
		if _, _, e := mmsProfile(q); e == nil {
			t.Fatalf("accepted invalid profile: %+v", q)
		}
	}
}
func TestMMSContextDoesNotStealDataOrIMS(t *testing.T) {
	contexts := []APNContext{{1, "cmwap", "IP"}, {4, "cmwap", "IP"}, {5, "ims", "IPV4V6"}}
	if id, created := selectMMSContext(contexts, map[int]bool{}, "CMWAP"); id != 4 || created {
		t.Fatal(id, created)
	}
	if id, created := selectMMSContext(contexts, map[int]bool{4: true}, "cmwap"); id != 16 || !created {
		t.Fatal(id, created)
	}
	for id := 6; id <= 16; id++ {
		contexts = append(contexts, APNContext{id, "other", "IP"})
	}
	if id, _ := selectMMSContext(contexts, map[int]bool{4: true}, "cmwap"); id != 0 {
		t.Fatal("stole a context", id)
	}
}
func TestMMSLinePreservesBinary(t *testing.T) {
	at := &atSession{port: &mmsTestPort{}, buffer: "\r\nCONNECT\r\n\x00\r\nOK\r\n\xff"}
	if line, e := mmsLine(context.Background(), at); e != nil || line != "CONNECT" {
		t.Fatal(line, e)
	}
	data, e := mmsReadBytes(context.Background(), at, 8)
	if e != nil || !bytes.Equal(data, []byte{0, 13, 10, 'O', 'K', 13, 10, 255}) {
		t.Fatal(data, e)
	}
}
func TestMMSChecksum(t *testing.T) {
	for _, test := range []struct {
		data string
		sum  uint16
	}{{"", 0}, {"a", 0x6100}, {"ab", 0x6162}, {"abc", 0x0262}, {"abcd", 0x0206}} {
		if v := mmsChecksum([]byte(test.data)); v != test.sum {
			t.Fatalf("%q: %x", test.data, v)
		}
	}
}
func TestMMSUploadAcknowledgesBlocksAndChecksData(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 900)
	offset := 0
	port := &mmsTestPort{}
	port.onWrite = func(b []byte) string {
		if strings.HasPrefix(string(b), "AT+QFUPL=") {
			if !strings.Contains(string(b), ",60,1") {
				t.Fatal("no ACK mode")
			}
			return "\r\nCONNECT\r\n"
		}
		if !bytes.Equal(b, data[offset:offset+len(b)]) {
			t.Fatal("corrupted upload")
		}
		offset += len(b)
		if offset == len(data) {
			return fmt.Sprintf("\r\n+QFUPL: %d,%x\r\nOK\r\n", len(data), mmsChecksum(data))
		}
		if len(b) > 511 {
			t.Fatal("unflushed USB packet")
		}
		if offset%1024 == 0 {
			return "A"
		}
		return ""
	}
	b := &mmsBearer{at: &atSession{port: port}, healthy: true}
	if e := b.upload(context.Background(), "RAM:abc12345.png", data); e != nil {
		t.Fatal(e)
	}
	if offset != len(data) {
		t.Fatal(offset)
	}
}
func TestNativeMMSRequiresTerminalResultAndNeverResends(t *testing.T) {
	for _, terminal := range []string{"+QMMSEND: 0,200\r\n", "+QMMSEND: 775\r\n", ""} {
		t.Run(terminal, func(t *testing.T) {
			port := &mmsTestPort{}
			upload := false
			port.onWrite = func(b []byte) string {
				cmd := string(b)
				if upload {
					upload = false
					return fmt.Sprintf("\r\n+QFUPL: %d,%x\r\nOK\r\n", len(b), mmsChecksum(b))
				}
				if strings.HasPrefix(cmd, "AT+QFUPL=") {
					upload = true
					return "\r\nCONNECT\r\n"
				}
				if strings.HasPrefix(cmd, "AT+QMMSEND=") {
					return "\r\nOK\r\n" + terminal
				}
				return "\r\nOK\r\n"
			}
			b := &mmsBearer{at: &atSession{port: port}, cid: 4, healthy: true}
			r, e := sendNativeMMS(context.Background(), b, testMMSProfile(), 80, "+8613800000000", "hello", nil)
			if !r.Attempted {
				t.Fatal(r, e)
			}
			if terminal == "+QMMSEND: 0,200\r\n" {
				if !r.Accepted || e != nil {
					t.Fatal(r, e)
				}
			} else if r.Accepted || e == nil {
				t.Fatal("accepted OK without success URC", r, e)
			}
			count := 0
			for _, cmd := range port.commands {
				if strings.HasPrefix(cmd, "AT+QMMSEND=") {
					count++
				}
			}
			if count != 1 {
				t.Fatal("replayed submission", count)
			}
			if terminal == "" && b.healthy {
				t.Fatal("unsafe cleanup after desynchronization")
			}
		})
	}
}
func TestNativeMMSUploadFailureDoesNotSubmit(t *testing.T) {
	port := &mmsTestPort{}
	upload := false
	port.onWrite = func(b []byte) string {
		if upload {
			upload = false
			return "+QFUPL: 1,0\r\nOK\r\n"
		}
		if strings.HasPrefix(string(b), "AT+QFUPL=") {
			upload = true
			return "CONNECT\r\n"
		}
		return "OK\r\n"
	}
	b := &mmsBearer{at: &atSession{port: port}, cid: 4, healthy: true}
	r, e := sendNativeMMS(context.Background(), b, testMMSProfile(), 80, "+8613800000000", "hello", nil)
	if e == nil || r.Attempted {
		t.Fatal(r, e)
	}
	for _, cmd := range port.commands {
		if strings.HasPrefix(cmd, "AT+QMMSEND") {
			t.Fatal("submitted corrupt attachment")
		}
	}
}
func TestMMSHTTPUsesCarrierProxyAndBinaryBoundaries(t *testing.T) {
	data := []byte{0x8c, 0x84, 0x84, 0xa3, 0, 13, 10, 'O', 'K', 13, 10, 0xff}
	response := append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.wap.mms-message\r\nContent-Length: %d\r\n\r\n", len(data))), data...)
	port := &mmsTestPort{}
	request := false
	sent := ""
	port.onWrite = func(b []byte) string {
		cmd := string(b)
		if request {
			request = false
			sent += cmd
			return "\r\nSEND OK\r\n"
		}
		switch {
		case strings.HasPrefix(cmd, "AT+QIOPEN="):
			if !strings.Contains(cmd, ",\"10.0.0.172\",80,0,0") {
				t.Fatal("not carrier gateway", cmd)
			}
			return "OK\r\n+QIOPEN: 11,0\r\n"
		case strings.HasPrefix(cmd, "AT+QISEND="):
			request = true
			return "\r\n> "
		case strings.HasPrefix(cmd, "AT+QIRD="):
			if len(response) == 0 {
				return "+QIRD: 0\r\nOK\r\n"
			}
			n := len(response)
			if n > 17 {
				n = 17
			}
			v := response[:n]
			response = response[n:]
			return fmt.Sprintf("\r\n+QIRD: %d\r\n%s\r\nOK\r\n", n, v)
		default:
			return "\r\nOK\r\n"
		}
	}
	b := &mmsBearer{at: &atSession{port: port}, cid: 4, healthy: true}
	got, e := b.http(context.Background(), testMMSProfile(), "GET", "http://mmsc.example.test/image?id=123", nil)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal(got, e)
	}
	if !strings.HasPrefix(sent, "GET http://mmsc.example.test/image?id=123 HTTP/1.1\r\n") || !strings.Contains(sent, "Host: mmsc.example.test\r\n") {
		t.Fatal(sent)
	}
}
func TestMMSReceiveRejectsUntrustedLocations(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1/", "https://mmsc.example.test/", "http://mmsc.example.test@other/", "http://mmsc.example.test:65536/", "http://mmsc.example.test/\r\nx"} {
		if _, e := mmsReceiveURL(testMMSProfile(), u); e == nil {
			t.Fatal(u)
		}
	}
}
func TestMMSWaitBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := mmsWait(ctx, &atSession{port: &mmsTestPort{}}, "+QMMSEND"); e == nil {
		t.Fatal("ignored cancellation")
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, e := mmsWait(ctx, &atSession{port: &mmsTestPort{}, buffer: "OK\r\n"}, "+QMMSEND"); e != io.EOF {
		t.Fatal(e)
	}
}
func TestMMSChunkNeverEntersEscapeMode(t *testing.T) {
	for _, data := range [][]byte{[]byte("+++++++"), []byte("abc+++def"), bytes.Repeat([]byte("+"), 2048)} {
		for len(data) > 0 {
			n := mmsChunk(data)
			if n < 1 || n > 1024 || bytes.Contains(data[:n], []byte("+++")) {
				t.Fatal(n)
			}
			data = data[n:]
		}
	}
}
func TestMMSChunkUploadPreservesEscapeBytes(t *testing.T) {
	data := append(bytes.Repeat([]byte("a"), 1030), []byte("+++++++\r\nOK\r\n")...)
	port := &mmsTestPort{}
	stored := []byte{}
	offset := 0
	pending := 0
	written := 0
	port.onWrite = func(b []byte) string {
		if pending > 0 {
			if len(b) > pending || bytes.Contains(b, []byte("+++")) {
				t.Fatal("unsafe data chunk")
			}
			stored = append(stored, b...)
			pending -= len(b)
			written += len(b)
			if pending > 0 {
				return ""
			}
			return fmt.Sprintf("\r\n+QFWRITE: %d,%d\r\nOK\r\n", written, len(stored))
		}
		cmd := string(b)
		switch {
		case strings.HasPrefix(cmd, "AT+QFOPEN="):
			return "+QFOPEN: 3000\r\nOK\r\n"
		case strings.HasPrefix(cmd, "AT+QFWRITE="):
			written = 0
			fmt.Sscanf(cmd, "AT+QFWRITE=3000,%d,10", &pending)
			return "CONNECT\r\n"
		case strings.HasPrefix(cmd, "AT+QFREAD="):
			var n int
			fmt.Sscanf(cmd, "AT+QFREAD=3000,%d", &n)
			v := stored[offset : offset+n]
			offset += n
			return fmt.Sprintf("CONNECT %d\r\n%s\r\nOK\r\n", n, v)
		default:
			return "OK\r\n"
		}
	}
	b := &mmsBearer{at: &atSession{port: port}, healthy: true}
	if e := b.upload(context.Background(), "RAM:12345678.png", data); e != nil || !bytes.Equal(data, stored) || offset != len(data) {
		t.Fatal(e, offset)
	}
}

type mmsBackpressurePort struct {
	mmsTestPort
	calls   int
	data    []byte
	blocked bool
}

func (p *mmsBackpressurePort) Write(data []byte) (int, error) {
	p.calls++
	if p.blocked || p.calls%3 == 1 {
		return 0, nil
	}
	n := len(data)
	if n > 17 {
		n = 17
	}
	p.data = append(p.data, data[:n]...)
	return n, nil
}
func TestMMSDataWriteBackpressure(t *testing.T) {
	data := bytes.Repeat([]byte{0, 0xff, '+', 0x1a}, 600)
	p := &mmsBackpressurePort{}
	if err := mmsDataWrite(context.Background(), &atSession{port: p}, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.data, data) {
		t.Fatal("data lost or duplicated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	p = &mmsBackpressurePort{blocked: true}
	if err := mmsDataWrite(ctx, &atSession{port: p}, data); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
}

func TestMMSDNSUsesPDPAndRejectsLocalAddresses(t *testing.T) {
	for _, tc := range []struct{ response, want string }{
		{"+QIURC: \"dnsgip\",0,2,60\r\n+QIURC: \"dnsgip\",\"127.0.0.1\"\r\n+QIURC: \"dnsgip\",\"10.2.3.4\"\r\n", "10.2.3.4"},
		{"+QIURC: \"dnsgip\",0,1,60\r\n+QIURC: \"dnsgip\",\"169.254.169.254\"\r\n", ""},
		{"+QIURC: \"dnsgip\",565\r\n", ""},
		{"+QIURC: \"dnsgip\",0,999,60\r\n", ""},
	} {
		port := &mmsTestPort{onWrite: func(data []byte) string {
			if string(data) != "AT+QIDNSGIP=4,\"download.example\"\r" {
				t.Fatal(string(data))
			}
			return "OK\r\n" + tc.response
		}}
		b := &mmsBearer{at: &atSession{port: port}, cid: 4, healthy: true}
		got, err := b.resolveHost(context.Background(), "download.example")
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Fatal(got, err, tc)
		}
	}
}
