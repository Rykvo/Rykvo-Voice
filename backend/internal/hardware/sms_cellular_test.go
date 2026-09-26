package hardware

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type smsTestPort struct {
	data []byte
	zero bool
}

func TestSMSReportRouting(t *testing.T) {
	for _, tc := range []struct {
		current, command string
		fail             bool
	}{
		{"+CNMI: 2,1,0,0,0", "AT+CNMI=2,1,0,2,0", false},
		{"+CNMI: 1,1,2,1,1", "AT+CNMI=1,1,2,2,1", false},
		{"+CNMI: 2,1,0,2,0", "", false},
		{"+CNMI: 2,1,0,0", "", true},
		{"+CNMI: 2,1,0,x,0", "", true},
	} {
		var commands []string
		query := func(_ context.Context, command string) ([]string, error) {
			commands = append(commands, command)
			if command == "AT+CNMI?" {
				return []string{tc.current}, nil
			}
			if command != tc.command {
				t.Fatal(command)
			}
			return nil, nil
		}
		err := ensureSMSReports(context.Background(), query)
		wantCalls := 1
		if tc.command != "" {
			wantCalls++
		}
		if (err != nil) != tc.fail || len(commands) != wantCalls {
			t.Fatal(tc, commands, err)
		}
	}
	want := errors.New("COMMAND_UNSUPPORTED")
	if err := ensureSMSReports(context.Background(), func(context.Context, string) ([]string, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestCellularStatusReportStore(t *testing.T) {
	for _, failStore := range []bool{false, true} {
		current := "ME"
		port := &mmsTestPort{onWrite: func(b []byte) string {
			command := strings.TrimSpace(string(b))
			switch {
			case command == "AT+CPMS?":
				return "+CPMS: \"ME\",0,255,\"ME\",0,255,\"ME\",0,255\r\nOK\r\n"
			case strings.HasPrefix(command, "AT+CPMS="):
				current = strings.Trim(strings.TrimPrefix(command, "AT+CPMS="), `"`)
				return "OK\r\n"
			case command == "AT+CMGL=4":
				if current == "SR" {
					return "+CMGL: 1,0,,22\r\n00022A05912143F5421020304050004210203050500000\r\nOK\r\n"
				}
				return "OK\r\n"
			default:
				t.Fatal(command)
				return "ERROR\r\n"
			}
		}}
		count := 0
		err := readCellularInbox(context.Background(), &atSession{port: port}, func(_ context.Context, d SMSDelivery) error {
			count++
			if d.Status == nil || d.Status.Reference != 42 || d.Status.Code != 0 || d.From != "+12345" || d.SCTS == nil {
				t.Fatalf("%+v", d)
			}
			if failStore {
				return errors.New("storage failed")
			}
			return nil
		})
		if count != 1 || (err != nil) != failStore || current != "ME" {
			t.Fatal(count, err, current)
		}
		for _, command := range port.commands {
			if strings.Contains(command, "CMGD") {
				t.Fatal("deleted modem storage")
			}
		}
	}
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
