package main

import (
	"encoding/hex"
	"testing"
)

func TestMMSNotificationUsesSenderNotGateway(t *testing.T) {
	b := []byte{1, 6, 1, 0xbe, 0x8c, 0x82, 0x8d, 0x92, 0x98, 't', 0, 0x89, 23, 0x80}
	b = append(b, []byte("12025550123/TYPE=PLMN\x00")...)
	// From length is token plus encoded address, not the SMS gateway length.
	b[12] = byte(len("12025550123/TYPE=PLMN\x00") + 1)
	b = append(b, 0x83)
	b = append(b, []byte("http://mpc.t-mobile.com/test\x00")...)
	state, peer, meta := decodeMMSPush(hex.EncodeToString(b), "2300")
	if state != "download_pending" || peer != "12025550123" || meta["gateway"] != "2300" {
		t.Fatal(state, peer, meta)
	}
}
func TestMMSDeliveryReportIsNotEmptyMessage(t *testing.T) {
	b := []byte{1, 6, 1, 0xbe, 0x8c, 0x86, 0x8d, 0x91, 0x8b, 'i', 'd', 0, 0x97}
	b = append(b, []byte("+12025550123/TYPE=PLMN\x00")...)
	b = append(b, 0x95, 0x82)
	state, _, meta := decodeMMSPush(hex.EncodeToString(b), "106581570001")
	if state != "mms_report" || meta["status"] != byte(0x82) || meta["messageId"] != "id" {
		t.Fatal(state, meta)
	}
}
