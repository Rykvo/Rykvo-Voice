package hardware

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestWiFiActiveData(t *testing.T) {
	got, err := wifiActiveData([]string{"+CGACT: 1,1", "+CGACT: 2,0", "+CGACT: 3,1"})
	if err != nil || !reflect.DeepEqual(got, []int{1, 3}) {
		t.Fatal(got, err)
	}
	for _, lines := range [][]string{{"+CGACT: 0,1"}, {"+CGACT: 17,1"}, {"+CGACT: 1,2"}, {"+CGACT: 1"}, {"+CGACT: x,1"}, {"+CGACT: 1,1", "+CGACT: 1,0"}} {
		if _, err := wifiActiveData(lines); err == nil {
			t.Fatal("invalid PDP state accepted", lines)
		}
	}
}

type wifiDataTranscript struct {
	transcript
	replies []string
}

func (p *wifiDataTranscript) Write(b []byte) (int, error) {
	p.commands = append(p.commands, strings.TrimSpace(string(b)))
	p.response = p.replies[0] + "\r\n"
	p.replies = p.replies[1:]
	return len(b), nil
}
func TestWiFiStopsOnlyActivePDPContexts(t *testing.T) {
	for _, tc := range []struct {
		replies, commands []string
		failed            bool
	}{
		{[]string{"+CGACT: 1,0\r\nOK"}, []string{"AT+CGACT?"}, false},
		{[]string{"+CGACT: 1,1\r\n+CGACT: 2,0\r\n+CGACT: 3,1\r\nOK", "OK", "OK", "+CGACT: 1,0\r\n+CGACT: 3,0\r\nOK"}, []string{"AT+CGACT?", "AT+CGACT=0,1", "AT+CGACT=0,3", "AT+CGACT?"}, false},
		{[]string{"+CGACT: 1,1\r\nOK", "ERROR"}, []string{"AT+CGACT?", "AT+CGACT=0,1"}, true},
		{[]string{"+CGACT: 1,1\r\nOK", "OK", "+CGACT: 1,1\r\nOK"}, []string{"AT+CGACT?", "AT+CGACT=0,1", "AT+CGACT?"}, true},
		{[]string{"+CGACT: 1,2\r\nOK"}, []string{"AT+CGACT?"}, true},
	} {
		port := &wifiDataTranscript{replies: tc.replies}
		err := wifiStopData(context.Background(), &atSession{port: port})
		if (err != nil) != tc.failed || !reflect.DeepEqual(port.commands, tc.commands) {
			t.Fatal(err, port.commands)
		}
	}
}
func TestWiFiPreservesHostNetwork(t *testing.T) {
	if wifiHostData("") != nil || wifiHostData("rykvo-missing-fixture") == nil {
		t.Fatal("network presence guard")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 {
			if err := wifiHostData(iface.Name); err == nil || err.Error() != "WIFI_DATA_ACTIVE" {
				t.Fatal("active host interface accepted")
			}
			return
		}
	}
	t.Skip("no up interfaces")
}

func TestWiFiFlightPrecedesPDPTeardown(t *testing.T) {
	for _, tc := range []struct {
		before            int
		replies, commands []string
		failed            bool
	}{
		{1, []string{"OK", "+CFUN: 4\r\nOK", "+CGACT: 1,0\r\nOK"}, []string{"AT+CFUN=4", "AT+CFUN?", "AT+CGACT?"}, false},
		{4, []string{"+CFUN: 4\r\nOK", "+CGACT: 1,1\r\nOK", "OK", "+CGACT: 1,0\r\nOK"}, []string{"AT+CFUN?", "AT+CGACT?", "AT+CGACT=0,1", "AT+CGACT?"}, false},
		{1, []string{"ERROR"}, []string{"AT+CFUN=4"}, true},
		{1, []string{"OK", "+CFUN: 1\r\nOK"}, []string{"AT+CFUN=4", "AT+CFUN?"}, true},
	} {
		port := &wifiDataTranscript{replies: tc.replies}
		err := wifiRFOff(context.Background(), &atSession{port: port}, tc.before)
		if (err != nil) != tc.failed || !reflect.DeepEqual(port.commands, tc.commands) {
			t.Fatal(err, port.commands)
		}
	}
}
