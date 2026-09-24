package hardware

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T) *System {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sysfs path fixtures")
	}
	t.Helper()
	root := t.TempDir()
	s := &System{Sys: filepath.Join(root, "sys"), Dev: filepath.Join(root, "dev"), Readers: func(context.Context) ([]string, error) { return nil, nil }}
	os.MkdirAll(s.Sys, 0700)
	os.MkdirAll(s.Dev, 0700)
	return s
}
func put(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func usb(t *testing.T, s *System, name, serial string, base, offset int) {
	root := filepath.Join(s.Sys, "bus/usb/devices")
	put(t, filepath.Join(root, name, "idVendor"), "2c7c")
	put(t, filepath.Join(root, name, "idProduct"), "0125")
	put(t, filepath.Join(root, name, "serial"), serial)
	for i := 0; i < 4; i++ {
		p := filepath.Join(root, fmt.Sprintf("%s:1.%d", name, base+i))
		put(t, filepath.Join(p, "bInterfaceNumber"), fmt.Sprintf("%02x", base+i))
		os.MkdirAll(filepath.Join(p, fmt.Sprintf("ttyUSB%d", offset+i)), 0700)
	}
}
func TestUSBGroupingAndPortCompositions(t *testing.T) {
	for _, base := range []int{0, 2} {
		t.Run(fmt.Sprint(base), func(t *testing.T) {
			s := fixture(t)
			usb(t, s, "1-1", "Android", base, 0)
			usb(t, s, "1-2", "Android", base, 4)
			items, err := s.Discover(context.Background())
			if err != nil || len(items) != 2 {
				t.Fatalf("%v %v", items, err)
			}
			for i, c := range items {
				if len(c.Ports) != 2 || filepath.Base(c.Ports[0].Path) != fmt.Sprintf("ttyUSB%d", i*4+2) {
					t.Fatalf("wrong AT grouping: %+v", c)
				}
				if strings.Contains(c.Identity(Reading{}), "Android") {
					t.Fatal("identity leaked")
				}
			}
		})
	}
}
func TestDiscoveryHasNoFiveDeviceLimit(t *testing.T) {
	s := fixture(t)
	for i := 0; i < 21; i++ {
		usb(t, s, fmt.Sprintf("1-%02d", i), "same-default", 0, i*4)
	}
	v, err := s.Discover(context.Background())
	if err != nil || len(v) != 21 {
		t.Fatalf("%d %v", len(v), err)
	}
}
func TestMissingATStillVisibleAndUnrelatedUSBExcluded(t *testing.T) {
	s := fixture(t)
	root := filepath.Join(s.Sys, "bus/usb/devices")
	put(t, filepath.Join(root, "1-1", "idVendor"), "2c7c")
	put(t, filepath.Join(root, "1-1:1.0", "bInterfaceNumber"), "00")
	put(t, filepath.Join(root, "1-2", "idVendor"), "1234")
	put(t, filepath.Join(root, "1-2:1.0", "bInterfaceNumber"), "00")
	v, err := s.Discover(context.Background())
	if err != nil || len(v) != 1 || len(v[0].Ports) != 0 {
		t.Fatalf("%+v %v", v, err)
	}
}
func TestWWANAndReaders(t *testing.T) {
	s := fixture(t)
	for _, n := range []string{"wwan0at0", "wwan0at1", "wwan0qmi0"} {
		put(t, filepath.Join(s.Sys, "class/wwan", n, "uevent"), "")
	}
	s.Readers = func(context.Context) ([]string, error) { return []string{"USB SIM reader"}, nil }
	v, err := s.Discover(context.Background())
	if err != nil || len(v) != 2 {
		t.Fatalf("%v %v", v, err)
	}
	for _, c := range v {
		if c.Kind == "wwan" && filepath.Base(c.Ports[0].Path) != "wwan0at1" {
			t.Fatal(c)
		}
	}
}
func TestStableIdentityIndependentOfPortAndSIM(t *testing.T) {
	a := Candidate{Key: "usb:1", Serial: "Android"}
	b := Candidate{Key: "usb:2"}
	r := Reading{IMEI: "123456789012345", ICCID: "89123456789012345678"}
	if a.Identity(r) != b.Identity(r) {
		t.Fatal("identity depends on endpoint")
	}
	r.ICCID = "89999999999999999999"
	if a.Identity(r) != b.Identity(r) {
		t.Fatal("identity depends on SIM")
	}
}

type transcript struct {
	response string
	commands []string
}

func (p *transcript) Write(b []byte) (int, error) {
	cmd := strings.TrimSpace(string(b))
	p.commands = append(p.commands, cmd)
	responses := map[string]string{
		"AT+CGMM": "EC20", "AT+CGMR": "test-firmware", "AT+CGSN": "123456789012345", "AT+CPIN?": "+CPIN: READY", "AT+QCCID": "+QCCID: 89123456789012345678F", "AT+CNUM": "+CNUM: \"\",\"+12025550123\",145", "AT+CSQ": "+CSQ: 20,99", "AT+COPS?": "+COPS: 0,0,\"Test Network\",7", "AT+CEREG?": "+CEREG: 0,5", `AT+QENG="servingcell"`: `+QENG: "servingcell","NOCONN","LTE","FDD",001,01,0,0,0,0,0,0,0,-90,-12,-65,15`,
	}
	if v, ok := responses[cmd]; ok {
		p.response = cmd + "\r\n" + v + "\r\nOK\r\n"
	} else {
		p.response = "ERROR\r\n"
	}
	return len(b), nil
}
func (p *transcript) Read(b []byte) (int, error) {
	if len(p.response) == 0 {
		return 0, errors.New("empty response")
	}
	n := len(p.response)
	if n > 7 {
		n = 7
	}
	copy(b, p.response[:n])
	p.response = p.response[n:]
	return n, nil
}
func (p *transcript) Close() error { return nil }
func TestReadOnlyATSnapshot(t *testing.T) {
	p := &transcript{}
	r := Reading{}
	readATFields(context.Background(), &atSession{port: p}, &r)
	if r.IMEI != "123456789012345" || r.ICCID != "89123456789012345678" || r.Number != "+12025550123" || r.RSSI == nil || *r.RSSI != -73 || r.Registration != "roaming" {
		t.Fatalf("%+v", r)
	}
	for _, cmd := range p.commands {
		if strings.HasPrefix(cmd, "AT+CFUN") || strings.HasPrefix(cmd, "AT+CPBS") || strings.Contains(cmd, "CMGS") {
			t.Fatalf("unexpected control: %s", cmd)
		}
	}
}
func TestUnknownSignalAndNumber(t *testing.T) {
	if digits([]string{"+CCID: ERROR"}, 18, 22) != "" || validNumber("00123abc") || integer("unknown") != nil {
		t.Fatal("invalid value accepted")
	}
	if decodeICCID([]byte{0x98, 0x21, 0x43, 0x65, 0x87, 0x09, 0x21, 0x43, 0x65, 0x87}) != "89123456789012345678" {
		t.Fatal("ICCID BCD")
	}
}
func TestQMIValues(t *testing.T) {
	if qmiValue("\tRegistration state: 'registered'\n\tIMEI: '123456789012345'", "IMEI") != "123456789012345" {
		t.Fatal("QMI parsing")
	}
}
func TestCancelledReadSendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &transcript{}
	readATFields(ctx, &atSession{port: p}, &Reading{})
	if len(p.commands) != 0 {
		t.Fatal(p.commands)
	}
}
