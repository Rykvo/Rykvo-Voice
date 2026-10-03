package vowifi

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSMSPPEnvelope(t *testing.T) {
	tpdu := []byte{0, 1}
	rpdu := []byte{1, 9, 0, 0, 2, 0, 1}
	got, err := smsPPEnvelope(tpdu, rpdu)
	if err != nil || !bytes.Equal(got, []byte{0xd1, 8, 0x82, 2, 0x83, 0x81, 0x8b, 2, 0, 1}) {
		t.Fatalf("envelope=%X err=%v", got, err)
	}
	long := bytes.Repeat([]byte{0}, 140)
	rpdu = append([]byte{1, 9, 2, 0x91, 0x21, 0, 140}, long...)
	got, err = smsPPEnvelope(long, rpdu)
	if err != nil || !bytes.Equal(got[:3], []byte{0xd1, 0x81, 151}) || !bytes.Contains(got, []byte{0x8b, 0x81, 140}) {
		t.Fatalf("long envelope length=%d err=%v", len(got), err)
	}
	for _, raw := range [][]byte{nil, {1, 0, 255}, {1, 0, 0, 0, 9}, {1, 0, 0, 0, 2, 1, 1}, {1, 0, 0, 0, 2, 0, 1, 0}} {
		if _, err := smsPPEnvelope(tpdu, raw); err == nil {
			t.Fatal("malformed download accepted")
		}
	}
}

func TestSMSPPDownloadUsesBoundSIMAndSensitiveUICC(t *testing.T) {
	const card = "8986001234567890123"
	for _, status := range []string{"9000", "9101", "9300", "6300"} {
		t.Run(status, func(t *testing.T) {
			tpdu, rpdu := []byte{0, 1}, []byte{1, 9, 0, 0, 2, 0, 1}
			envelope, _ := smsPPEnvelope(tpdu, rpdu)
			apdu := append([]byte{0x80, 0xc2, 0, 0, byte(len(envelope))}, envelope...)
			apdu = append(apdu, 0)
			transcript := &ec20Transcript{t: t, steps: []ec20TranscriptStep{
				{command: "AT+CCID", lines: []string{"+CCID: " + card}},
				{command: `AT+CSIM=24,"00A4040407A0000000871002"`, lines: []string{`+CSIM: 4,"9000"`}},
				{command: fmt.Sprintf(`AT+CSIM=%d,"%s"`, len(apdu)*2, strings.ToUpper(hex.EncodeToString(apdu))), sensitive: true, lines: []string{`+CSIM: 4,"` + status + `"`}},
				{command: "AT+CCID", lines: []string{"+CCID: " + card}},
			}}
			adapter, _ := NewEC20Adapter(transcript, EC20AdapterOptions{})
			adapter.bindings[card] = ec20SIMBinding{deviceID: "fixture", iccid: card, imsi: "310260123456789", aid: "A0000000871002", application: "USIM"}
			result, err := adapter.SMSPPDownload(context.Background(), SIMIdentity{ICCID: card, IMSI: "310260123456789"}, tpdu, rpdu)
			want, _ := hex.DecodeString(status)
			if err != nil || result.Status != uint16(want[0])<<8|uint16(want[1]) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			transcript.assertDone()
		})
	}
	adapter, _ := NewEC20Adapter(&ec20Transcript{t: t}, EC20AdapterOptions{})
	adapter.bindings[card] = ec20SIMBinding{iccid: card, imsi: "310260123456789"}
	if _, err := adapter.SMSPPDownload(context.Background(), SIMIdentity{ICCID: card, IMSI: "310260000000000"}, []byte{0, 1}, []byte{1, 9, 0, 0, 2, 0, 1}); !errors.Is(err, ErrEC20IdentityChanged) {
		t.Fatal("wrong subscription was not rejected", err)
	}
}
