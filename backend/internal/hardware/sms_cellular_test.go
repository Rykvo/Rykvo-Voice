package hardware

import (
	"context"
	"io"
	"testing"
)

type smsTestPort struct {
	data []byte
	zero bool
}

func (p *smsTestPort) Read(b []byte) (int, error) {
	if len(p.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.data)
	p.data = p.data[n:]
	return n, nil
}
func (p *smsTestPort) Write(b []byte) (int, error) {
	if p.zero {
		return 0, nil
	}
	return len(b), nil
}
func (p *smsTestPort) Close() error { return nil }
func TestSMSATResponse(t *testing.T) {
	for _, v := range []struct {
		raw    string
		prompt bool
		ref    int
		fail   bool
	}{
		{"\r\n> ", true, 0, false}, {"\r\n+CMGS: 41\r\nOK\r\n", false, 41, false},
		{"+CMGS: nonsense\r\nOK\r\n", false, 0, true}, {"+CMGS: 256\r\nOK\r\n", false, 0, true},
		{"OK\r\n", false, 0, true}, {"+CMS ERROR: 500\r\n", false, 0, true},
	} {
		a := &atSession{port: &smsTestPort{data: []byte(v.raw)}}
		n, e := smsATRead(context.Background(), a, v.prompt)
		if (e != nil) != v.fail || !v.fail && n != v.ref {
			t.Fatalf("%q: %d %v", v.raw, n, e)
		}
	}
	if e := smsATWrite(context.Background(), &atSession{port: &smsTestPort{zero: true}}, []byte("AT")); e != io.ErrShortWrite {
		t.Fatal(e)
	}
}
