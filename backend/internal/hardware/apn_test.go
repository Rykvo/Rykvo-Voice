package hardware

import (
	"context"
	"strings"
	"testing"
)

func TestAPNValidation(t *testing.T) {
	base := APNConfig{APN: "internet", Protocol: "IPV4V6", Auth: "NONE"}
	if !ValidAPNConfig(base) {
		t.Fatal("valid APN rejected")
	}
	for _, apn := range []string{"ims", "SOS", "a\"\rAT+CFUN=1", "a\\b", "a;b", strings.Repeat("a", 101)} {
		v := base
		v.APN = apn
		if ValidAPNConfig(v) {
			t.Fatal("unsafe APN accepted", apn)
		}
	}
	for _, pass := range []string{"bad\"pass", "a\nAT", "\\", strings.Repeat("a", 129)} {
		v := base
		v.Auth = "PAP"
		v.Password = pass
		if ValidAPNConfig(v) {
			t.Fatal("unsafe credential accepted")
		}
	}
	base.APN = ""
	if !ValidAPNConfig(base) {
		t.Fatal("operator default rejected")
	}
	base.Protocol = "unexpected"
	if ValidAPNConfig(base) {
		t.Fatal("protocol not validated")
	}
}
func TestAPNParseKeepsSystemContextsAndUnknownTypes(t *testing.T) {
	rows, e := parseAPNContexts([]string{`+CGDCONT: 1,"IPV4V6","","0.0.0.0",0,0`, `+CGDCONT: 2,"IPV4V6","ims"`, `+CGDCONT: 3,"IP","sos"`})
	if e != nil || len(rows) != 3 || rows[0].APN != "" || rows[1].APN != "ims" {
		t.Fatal(rows, e)
	}
	for _, lines := range [][]string{{`+CGDCONT: 1,"IP"`}, {`+CGDCONT: 0,"IP","x"`}, {`+CGDCONT: 1,"IP","x"`, `+CGDCONT: 1,"IP","y"`}} {
		if _, e := parseAPNContexts(lines); e == nil {
			t.Fatal("invalid context accepted")
		}
	}
}
func TestAPNApplyReadsBackWithoutActivatingData(t *testing.T) {
	const iccid = "89123456789012345678"
	card := "+QCCID: " + iccid + "\r\nOK"
	port := &wifiDataTranscript{replies: []string{card, "OK", card, "OK", `+CGDCONT: 1,"IPV4V6","internet"` + "\r\nOK", card}}
	rows, e := writeAPNConfig(context.Background(), &atSession{port: port}, iccid, APNConfig{APN: "internet", Protocol: "IPV4V6", Auth: "NONE"})
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	if port.commands[3] != `AT+CGAUTH=1,0,"",""` {
		t.Fatal("old authentication was not cleared")
	}
	for _, cmd := range port.commands {
		if strings.Contains(cmd, "CGACT=") || strings.Contains(cmd, "CGATT=") || strings.Contains(cmd, "CFUN=") {
			t.Fatal("APN unexpectedly enabled data", cmd)
		}
	}
}
func TestAPNPartialWriteNeverReportsSuccess(t *testing.T) {
	card := "+QCCID: 89123456789012345678\r\nOK"
	port := &wifiDataTranscript{replies: []string{card, "OK", card, "ERROR"}}
	_, e := writeAPNConfig(context.Background(), &atSession{port: port}, "89123456789012345678", APNConfig{APN: "internet", Protocol: "IP", Auth: "PAP", Username: "user", Password: "secret"})
	if e == nil || e.Error() != "APN_APPLY_UNCONFIRMED" || strings.Contains(e.Error(), "secret") {
		t.Fatal(e)
	}
	if len(port.commands) != 4 {
		t.Fatal("partial operation automatically retried")
	}
}
