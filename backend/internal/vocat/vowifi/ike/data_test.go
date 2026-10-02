package ike

import (
	"encoding/binary"
	"testing"
)

func TestDataAuthDoesNotReplaceIMSAndRequestsCorrectFamily(t *testing.T) {
	for _, protocol := range []string{"IP", "IPV6", "IPV4V6"} {
		input := []payload{makeNotify(notifyInitialContact, nil), makeNotify(notifyMOBIKESupported, nil), configurationRequest()}
		result := dataAuthPayloads(input, protocol)
		if len(result) != 2 || len(input) != 3 {
			t.Fatal("mutated IMS payloads")
		}
		kind, _, _ := parseNotify(result[0])
		if kind != notifyMOBIKESupported {
			t.Fatal("wrong notification retained")
		}
		attrs := map[uint16]bool{}
		for i := 4; i < len(result[1].Body); i += 4 {
			attrs[binary.BigEndian.Uint16(result[1].Body[i:i+2])] = true
		}
		if attrs[configPCSCFIPv4Address] || attrs[configPCSCFIPv6Address] {
			t.Fatal("MMS requests IMS P-CSCF")
		}
		if attrs[configInternalIPv4Address] != (protocol != "IPV6") || attrs[configInternalIPv6Address] != (protocol != "IP") {
			t.Fatal(protocol, attrs)
		}
		kind, _, _ = parseNotify(input[0])
		if kind != notifyInitialContact {
			t.Fatal("IMS initial contact changed")
		}
	}
}
