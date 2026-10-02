package hardware

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func testIMSI(value string) []byte {
	out := []byte{8, (value[0]-'0')<<4 | 9}
	for i := 1; i < len(value); i += 2 {
		out = append(out, value[i]-'0'|(value[i+1]-'0')<<4)
	}
	return out
}

func TestWiFiSIMIdentity(t *testing.T) {
	for _, c := range []struct {
		imsi, mnc, domain string
		length            byte
	}{
		{"001010123456789", "01", "epdg.epc.mnc001.mcc001.pub.3gppnetwork.org", 2},
		{"001001123456789", "001", "epdg.epc.mnc001.mcc001.pub.3gppnetwork.org", 3},
	} {
		id, err := wifiSIMIdentity(testIMSI(c.imsi), []byte{0, 0, 0, c.length})
		if err != nil || id.IMSI != c.imsi || id.MNC != c.mnc || id.epdg() != c.domain {
			t.Fatalf("identity mismatch: %v", err)
		}
	}
	for _, ad := range [][]byte{nil, {0, 0, 0}, {0, 0, 0, 0xff}, {0, 0, 0, 1}} {
		if _, err := wifiSIMIdentity(testIMSI("001010123456789"), ad); err == nil {
			t.Fatal("guessed MNC length")
		}
	}
	for _, data := range [][]byte{nil, {0}, bytes.Repeat([]byte{0xff}, 9), {9, 9, 0, 0, 0, 0, 0, 0, 0}} {
		if _, err := wifiSIMIdentity(data, []byte{0, 0, 0, 2}); err == nil {
			t.Fatal("invalid IMSI accepted")
		}
	}
}

type wifiTranscript struct {
	transcript
	iccidReads int
	change     bool
	badAD      bool
}

func (p *wifiTranscript) Write(b []byte) (int, error) {
	command := strings.TrimSpace(string(b))
	if command == "AT+QCCID" {
		p.iccidReads++
		if p.change && p.iccidReads > 1 {
			p.commands = append(p.commands, command)
			p.response = "+QCCID: 89123456789012345679\r\nOK\r\n"
			return len(b), nil
		}
	}
	if !strings.HasPrefix(command, "AT+CSIM=") {
		return p.transcript.Write(b)
	}
	p.commands = append(p.commands, command)
	_, data, _ := strings.Cut(command, ",\"")
	data = strings.TrimSuffix(data, "\"")
	response := "9000"
	switch data {
	case "0070000001":
		response = "019000"
	case "01A4040007A0000000871002", "01A4000C026F07", "01A4000C026FAD", "0070800100":
	case "01B0000009":
		response = strings.ToUpper(hex.EncodeToString(testIMSI("001010123456789"))) + "9000"
	case "01B0000004":
		response = "000000029000"
		if p.badAD {
			response = "000000FF9000"
		}
	default:
		p.response = "ERROR\r\n"
		return len(b), nil
	}
	p.response = fmt.Sprintf("+CSIM: %d,\"%s\"\r\nOK\r\n", len(response), response)
	return len(b), nil
}

func TestWiFiInspectionReadOnlyAndCleanup(t *testing.T) {
	for _, mode := range []string{"ok", "changed", "unknown-mnc"} {
		t.Run(mode, func(t *testing.T) {
			port := &wifiTranscript{change: mode == "changed", badAD: mode == "unknown-mnc"}
			sim, err := inspectWiFiSIM(context.Background(), &atSession{port: port}, "89123456789012345678")
			if mode == "ok" {
				if err != nil {
					t.Fatal(err)
				}
				if err := sim.close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("bad identity accepted")
			}
			if port.commands[len(port.commands)-1] != `AT+CSIM=10,"0070800100"` {
				t.Fatal("channel not closed")
			}
			for _, command := range port.commands {
				if strings.Contains(command, "CFUN=") || strings.Contains(command, "CGACT=") || strings.Contains(command, "01880081") {
					t.Fatal("inspection mutated radio or authenticated")
				}
			}
		})
	}
}

func TestWiFiInspectionRequiresCurrentReadyCard(t *testing.T) {
	for _, reply := range []string{"+CPIN: SIM PIN\r\nOK", "+CME ERROR: 10"} {
		port := &wifiTranscript{transcript: transcript{simReply: reply}}
		if _, err := inspectWiFiSIM(context.Background(), &atSession{port: port}, "89123456789012345678"); err == nil {
			t.Fatal("unready card accepted")
		}
		if len(port.commands) != 1 {
			t.Fatal("probed application before ready")
		}
	}
	port := &wifiTranscript{}
	if _, err := inspectWiFiSIM(context.Background(), &atSession{port: port}, "89123456789012345679"); err == nil {
		t.Fatal("wrong card accepted")
	}
	if len(port.commands) != 2 {
		t.Fatal("opened channel on wrong card")
	}
}

func TestWiFiAKAResponseBoundsAndClearing(t *testing.T) {
	data := append([]byte{0xdb, 4}, bytes.Repeat([]byte{1}, 4)...)
	data = append(data, 16)
	data = append(data, bytes.Repeat([]byte{2}, 16)...)
	data = append(data, 16)
	data = append(data, bytes.Repeat([]byte{3}, 16)...)
	for _, kc := range []bool{false, true} {
		input := bytes.Clone(data)
		if kc {
			input = append(input, append([]byte{8}, make([]byte, 8)...)...)
		}
		result, err := parseAKA(input)
		if err != nil || len(result.RES) != 4 || len(result.CK) != 16 || len(result.IK) != 16 {
			t.Fatal("valid AKA rejected")
		}
		res := result.RES
		result.clear()
		if result.RES != nil || !bytes.Equal(res, make([]byte, 4)) {
			t.Fatal("keys retained")
		}
	}
	for i := 0; i < len(data); i++ {
		if result, err := parseAKA(data[:i]); err == nil || len(result.RES) != 0 {
			t.Fatal("truncated AKA accepted")
		}
	}
	auts := append([]byte{0xdc, 14}, make([]byte, 14)...)
	result, err := parseAKA(auts)
	if err != nil || len(result.AUTS) != 14 || len(result.RES) != 0 {
		t.Fatal("AUTS not separated")
	}
	for _, malformed := range [][]byte{append(bytes.Clone(data), 0), append(auts, 0), {0xdc, 13}, {0xdb, 32}} {
		if _, err := parseAKA(malformed); err == nil {
			t.Fatal("malformed AKA accepted")
		}
	}
}

func TestWiFiAKARequiresMatchingCardAndChallenge(t *testing.T) {
	port := &wifiTranscript{}
	sim := &wifiSIM{session: &atSession{port: port}, id: wifiIdentity{ICCID: "89123456789012345679"}}
	if _, err := sim.authenticate(context.Background(), nil, nil); err == nil || len(port.commands) != 0 {
		t.Fatal("invalid challenge sent")
	}
	if _, err := sim.authenticate(context.Background(), make([]byte, 16), make([]byte, 16)); err == nil || len(port.commands) != 1 {
		t.Fatal("AKA ran on another card")
	}
}

func TestWiFiAPDUResponseBoundaries(t *testing.T) {
	for _, responses := range [][][]byte{
		{{0x6c, 2}, {1, 2, 0x90, 0}},
		{{1, 0x61, 1}, {2, 0x90, 0}},
	} {
		i := 0
		s := &wifiSIM{card: &cardChannel{channel: 1, send: func(_ context.Context, cmd []byte) ([]byte, error) {
			if cmd[0] != 1 {
				t.Fatal("wrong logical channel")
			}
			result := bytes.Clone(responses[i])
			i++
			return result, nil
		}}}
		data, err := s.exchange(context.Background(), []byte{1, 0xb0, 0, 0, 1})
		if err != nil || !bytes.Equal(data, []byte{1, 2}) {
			t.Fatal("procedure bytes mishandled")
		}
	}
	count := 0
	s := &wifiSIM{card: &cardChannel{channel: 1, send: func(context.Context, []byte) ([]byte, error) { count++; return []byte{0x61, 1}, nil }}}
	if _, err := s.exchange(context.Background(), []byte{1, 0xb0, 0, 0, 1}); err == nil || count != 8 {
		t.Fatal("unbounded GET RESPONSE")
	}
}
