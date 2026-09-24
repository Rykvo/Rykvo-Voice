package hardware

import (
	"context"
	"strings"
	"testing"
)

func TestOperatorScanParsing(t *testing.T) {
	lines := []string{`+COPS: (2,"Test, Mobile (LTE): A","TM","00101",7),(1,"","Other","00102",2),(3,"Blocked","","00103",7),(1,"duplicate","","00101",7),,(0-4),(0-2)`}
	ops, err := parseOperators(lines)
	if err != nil || len(ops) != 3 || ops[0].Name != "Test, Mobile (LTE): A" || ops[0].PLMN != "00101" || *ops[0].Technology != 7 || ops[1].Name != "Other" || ops[2].Status != 3 {
		t.Fatalf("%+v %v", ops, err)
	}
	if ops, err = parseOperators([]string{`+COPS: ,,(0-4),(0-2)`}); err != nil || len(ops) != 0 {
		t.Fatal("empty scan")
	}
	if _, err = parseOperators([]string{"RING"}); err == nil {
		t.Fatal("missing response accepted")
	}
}

func TestNetworkRequestValidationAndConfirmation(t *testing.T) {
	r := NetworkRequest{IMEI: "123456789012345", ICCID: "89123456789012345678", PLMN: "00101", Technology: integer("7")}
	if !r.Valid(false) {
		t.Fatal("valid request")
	}
	for _, plmn := range []string{"", "1234", "00101\rAT+CFUN=0", "00101\"", "abcdef"} {
		bad := r
		bad.PLMN = plmn
		if bad.Valid(false) {
			t.Fatal("invalid operator")
		}
	}
	bad := r
	bad.Technology = integer("99")
	if bad.Valid(false) {
		t.Fatal("invalid technology")
	}
	reading := Reading{Responsive: true, IMEI: r.IMEI, ICCID: r.ICCID, NetworkMode: integer("1"), PLMN: r.PLMN, AccessTechnology: integer("7")}
	if !r.Confirmed(reading) {
		t.Fatal("matching selection")
	}
	reading.ICCID = "89123456789012345679"
	if r.Confirmed(reading) {
		t.Fatal("another card")
	}
	reading.ICCID = r.ICCID
	reading.NetworkMode = integer("4")
	if r.Confirmed(reading) {
		t.Fatal("fallback is not manual-only")
	}
	r.Automatic = true
	r.PLMN = ""
	r.Technology = nil
	reading.NetworkMode = integer("0")
	if !r.Valid(false) || !r.Confirmed(reading) {
		t.Fatal("automatic")
	}
	reading.NetworkMode = nil
	if r.Confirmed(reading) {
		t.Fatal("unknown mode")
	}
}

type networkTranscript struct {
	transcript
	replies map[string]string
}

func (p *networkTranscript) Write(b []byte) (int, error) {
	cmd := strings.TrimSpace(string(b))
	if reply, ok := p.replies[cmd]; ok {
		p.commands = append(p.commands, cmd)
		p.response = reply
		return len(b), nil
	}
	return p.transcript.Write(b)
}

func TestNetworkCommandsAreScopedAndNeverReplay(t *testing.T) {
	for _, scan := range []bool{true, false} {
		r := NetworkRequest{IMEI: "123456789012345", ICCID: "89123456789012345678", Automatic: !scan}
		p := &networkTranscript{replies: map[string]string{"AT+COPS=?": "+COPS: (1,\"Test\",\"\",\"00101\",7)\r\nOK\r\n", "AT+COPS=0": "OK\r\n"}}
		ops, changed, err := networkSession(context.Background(), &atSession{port: p}, r, scan)
		if err != nil || changed == scan || (scan && len(ops) != 1) {
			t.Fatalf("%v %v", changed, err)
		}
		count := 0
		for _, cmd := range p.commands {
			if strings.HasPrefix(cmd, "AT+COPS=") {
				count++
			}
			if strings.Contains(cmd, "CFUN") || cmd == "AT+COPS=2" {
				t.Fatal("unexpected reset/detach")
			}
		}
		if count != 1 {
			t.Fatal("replayed operation")
		}
	}
	r := NetworkRequest{IMEI: "123456789012345", ICCID: "89123456789012345678", PLMN: "00101", Technology: integer("7")}
	p := &networkTranscript{replies: map[string]string{`AT+COPS=1,2,"00101",7`: "ERROR\r\n"}}
	_, changed, err := networkSession(context.Background(), &atSession{port: p}, r, false)
	if err == nil || !changed {
		t.Fatal("failed selection needs verification")
	}
	count := 0
	for _, cmd := range p.commands {
		if strings.HasPrefix(cmd, "AT+COPS=") {
			count++
		}
	}
	if count != 1 {
		t.Fatal("retry")
	}
	p = &networkTranscript{replies: map[string]string{"AT+CGSN": "999999999999999\r\nOK\r\n"}}
	_, changed, err = networkSession(context.Background(), &atSession{port: p}, r, false)
	if err == nil || changed || len(p.commands) != 1 {
		t.Fatal("identity mismatch touched radio")
	}
}
