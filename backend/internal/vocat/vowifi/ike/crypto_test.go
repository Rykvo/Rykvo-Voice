package ike

import (
	"bytes"
	"errors"
	"testing"
)

func legacyTestSuite() negotiatedSuite {
	return negotiatedSuite{
		EncryptionID:   encryptionAESCBC,
		EncryptionBits: 128,
		PRFID:          prfHMACSHA1,
		IntegrityID:    integrityHMACSHA1_96,
		DHID:           dhMODP1024,
	}
}

func TestMODP1024UsesGroup2PrimeAnd128ByteKE(t *testing.T) {
	exchange, err := newDHExchange(dhMODP1024, bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)))
	if err != nil {
		t.Fatalf("newDHExchange() error = %v", err)
	}
	if bits := exchange.prime.BitLen(); bits != 1024 {
		t.Fatalf("group 2 prime BitLen() = %d, want 1024", bits)
	}
	if length := len(exchange.Public); length != 128 {
		t.Fatalf("group 2 public KE length = %d, want 128", length)
	}
	peer, err := newDHExchange(dhMODP1024, bytes.NewReader(bytes.Repeat([]byte{0x24}, 128)))
	if err != nil {
		t.Fatalf("peer newDHExchange() error = %v", err)
	}
	firstSecret, err := exchange.shared(peer.Public)
	if err != nil {
		t.Fatalf("exchange.shared() error = %v", err)
	}
	secondSecret, err := peer.shared(exchange.Public)
	if err != nil {
		t.Fatalf("peer.shared() error = %v", err)
	}
	if len(firstSecret) != 128 || !bytes.Equal(firstSecret, secondSecret) {
		t.Fatal("MODP group 2 shared secrets differ or are not 128 bytes")
	}
}

func TestEncryptedPayloadRoundTripAndTamperDetection(t *testing.T) {
	suite := legacyTestSuite()
	encryptionKey := bytes.Repeat([]byte{0x11}, 16)
	integrityKey := bytes.Repeat([]byte{0x22}, 20)
	header := ikeHeader{
		InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		ResponderSPI: [8]byte{8, 7, 6, 5, 4, 3, 2, 1},
		Exchange:     exchangeIKEAuth,
		Flags:        flagInitiator,
		MessageID:    7,
	}
	inner := []payload{
		{Type: payloadIDi, Body: []byte{3, 0, 0, 0, 'u', '@', 'r'}},
		{Type: payloadEAP, Body: []byte{1, 9, 0, 5, 1}},
	}
	packet, err := encryptPayloads(
		header,
		inner,
		suite,
		encryptionKey,
		integrityKey,
		bytes.NewReader(bytes.Repeat([]byte{0x33}, 64)),
	)
	if err != nil {
		t.Fatalf("encryptPayloads() error = %v", err)
	}
	decodedHeader, decoded, err := decryptPayloads(packet, suite, encryptionKey, integrityKey)
	if err != nil {
		t.Fatalf("decryptPayloads() error = %v", err)
	}
	if decodedHeader.MessageID != header.MessageID || len(decoded) != len(inner) {
		t.Fatalf("decoded header/payload count mismatch: %#v %#v", decodedHeader, decoded)
	}
	for index := range inner {
		if decoded[index].Type != inner[index].Type || !bytes.Equal(decoded[index].Body, inner[index].Body) {
			t.Fatalf("decoded payload %d = %#v, want %#v", index, decoded[index], inner[index])
		}
	}

	tampered := append([]byte(nil), packet...)
	tampered[len(tampered)-1] ^= 0x80
	if _, _, err := decryptPayloads(tampered, suite, encryptionKey, integrityKey); !errors.Is(err, errIntegrityMismatch) {
		t.Fatalf("tampered decrypt error = %v, want errIntegrityMismatch", err)
	}
}

func TestIKEKeyDerivationSeparatesDirections(t *testing.T) {
	suite := legacyTestSuite()
	keys, err := deriveIKEKeys(
		suite,
		bytes.Repeat([]byte{0x44}, 128),
		bytes.Repeat([]byte{0x55}, 32),
		bytes.Repeat([]byte{0x66}, 32),
		[8]byte{1},
		[8]byte{2},
	)
	if err != nil {
		t.Fatalf("deriveIKEKeys() error = %v", err)
	}
	if len(keys.SKd) != 20 || len(keys.SKai) != 20 || len(keys.SKar) != 20 ||
		len(keys.SKei) != 16 || len(keys.SKer) != 16 ||
		len(keys.SKpi) != 20 || len(keys.SKpr) != 20 {
		t.Fatalf("unexpected key lengths: %+v", keys)
	}
	if bytes.Equal(keys.SKai, keys.SKar) || bytes.Equal(keys.SKei, keys.SKer) ||
		bytes.Equal(keys.SKpi, keys.SKpr) {
		t.Fatal("initiator and responder keys were not separated")
	}
}

func TestRFC7383FragmentationAndReassembly(t *testing.T) {
	suite := legacyTestSuite()
	encryptionKey := bytes.Repeat([]byte{0x11}, 16)
	integrityKey := bytes.Repeat([]byte{0x22}, 20)
	header := ikeHeader{
		InitiatorSPI: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		ResponderSPI: [8]byte{8, 7, 6, 5, 4, 3, 2, 1},
		Exchange:     exchangeIKEAuth,
		Flags:        flagInitiator,
		MessageID:    9,
	}

	largeCertData := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 400) // 1600 bytes
	inner := []payload{
		{Type: payloadIDi, Body: []byte{3, 0, 0, 0, 'u', 's', 'e', 'r'}},
		{Type: payloadCert, Body: largeCertData},
		{Type: payloadAuth, Body: bytes.Repeat([]byte{0x55}, 64)},
	}

	// Fragment into chunks with max fragment size 600 bytes
	packets, err := encryptPayloadsFragmented(
		header,
		inner,
		suite,
		encryptionKey,
		integrityKey,
		600,
		bytes.NewReader(bytes.Repeat([]byte{0x77}, 1024)),
	)
	if err != nil {
		t.Fatalf("encryptPayloadsFragmented() error = %v", err)
	}

	if len(packets) < 3 {
		t.Fatalf("expected at least 3 fragments for large payload, got %d", len(packets))
	}

	for i, pkt := range packets {
		hdr, body, parseErr := parseIKEPacket(pkt)
		if parseErr != nil {
			t.Fatalf("fragment %d parse error: %v", i+1, parseErr)
		}
		if hdr.NextPayload != payloadEncryptedFragment {
			t.Fatalf("fragment %d NextPayload = %d, want %d (payloadEncryptedFragment)", i+1, hdr.NextPayload, payloadEncryptedFragment)
		}
		if len(body) < 8 {
			t.Fatalf("fragment %d body too short", i+1)
		}
	}

	// Decrypt and reassemble
	decodedHeader, decoded, err := decryptPayloadsAny(nil, packets, suite, encryptionKey, integrityKey)
	if err != nil {
		t.Fatalf("decryptPayloadsAny() error = %v", err)
	}

	if decodedHeader.MessageID != header.MessageID || len(decoded) != len(inner) {
		t.Fatalf("reassembled payload mismatch: header=%#v, count=%d, want=%d", decodedHeader, len(decoded), len(inner))
	}

	for index := range inner {
		if decoded[index].Type != inner[index].Type || !bytes.Equal(decoded[index].Body, inner[index].Body) {
			t.Fatalf("decoded payload %d = %#v, want %#v", index, decoded[index], inner[index])
		}
	}

	// Test tamper detection on second fragment
	tamperedPackets := make([][]byte, len(packets))
	for i := range packets {
		tamperedPackets[i] = append([]byte(nil), packets[i]...)
	}
	tamperedPackets[1][len(tamperedPackets[1])-1] ^= 0x55
	if _, _, err := decryptPayloadsAny(nil, tamperedPackets, suite, encryptionKey, integrityKey); !errors.Is(err, errIntegrityMismatch) {
		t.Fatalf("tampered fragment decrypt error = %v, want errIntegrityMismatch", err)
	}
}
