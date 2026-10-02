package hardware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	eapAKA          = 23
	akaChallenge    = 1
	akaIdentity     = 5
	akaNotification = 12
	atRAND          = 1
	atAUTN          = 2
	atRES           = 3
	atAUTS          = 4
	atAnyID         = 13
	atMAC           = 11
	atPermanentID   = 10
	atFullID        = 17
	atIdentity      = 14
	atNotification  = 12
	atCheckcode     = 134
	atResult        = 135
)

var errWiFiEAP = errors.New("WIFI_AUTH_INVALID")

// One full EAP-AKA exchange. The caller serializes it and owns the SIM gate.
// IKE responder authentication is still required before a tunnel is usable.
type wifiEAP struct {
	identity                                  string
	authenticate                              func(context.Context, []byte, []byte) (akaResult, error)
	request, response, transcript             []byte
	seen                                      [256]bool
	rounds, identities, challenges            int
	keys                                      [160]byte
	verified, resultPending, complete, closed bool
	outerIdentity                             bool
}

func newWiFiEAP(sim *wifiSIM) (*wifiEAP, error) {
	if sim == nil {
		return nil, errWiFiEAP
	}
	id := sim.id
	if !decimal(id.IMSI, 6, 15) || !decimal(id.MCC, 3, 3) || !decimal(id.MNC, 2, 3) || !bytes.HasPrefix([]byte(id.IMSI), []byte(id.MCC+id.MNC)) {
		return nil, errWiFiEAP
	}
	return &wifiEAP{
		identity:     fmt.Sprintf("0%s@nai.epc.mnc%03s.mcc%s.3gppnetwork.org", id.IMSI, id.MNC, id.MCC),
		authenticate: sim.authenticate,
	}, nil
}

func (e *wifiEAP) close() {
	clear(e.keys[:])
	clear(e.request)
	clear(e.response)
	clear(e.transcript)
	e.request, e.response, e.transcript = nil, nil, nil
	e.identity, e.authenticate = "", nil
	e.verified, e.complete, e.closed = false, false, true
}

func (e *wifiEAP) masterKey() ([]byte, error) {
	if !e.complete || e.closed {
		return nil, errWiFiEAP
	}
	return bytes.Clone(e.keys[32:96]), nil
}

func (e *wifiEAP) handle(ctx context.Context, packet []byte) (reply []byte, err error) {
	defer func() {
		if err != nil {
			e.close()
		}
	}()
	if e.closed || ctx.Err() != nil || len(packet) < 4 || len(packet) > 4096 || int(binary.BigEndian.Uint16(packet[2:4])) != len(packet) {
		return nil, errWiFiEAP
	}
	code, id := packet[0], packet[1]
	if code == 4 {
		return nil, errors.New("WIFI_AUTH_REJECTED")
	}
	if code == 3 {
		if len(packet) != 4 || !e.verified || e.resultPending || len(e.response) < 2 || e.response[1] != id {
			return nil, errWiFiEAP
		}
		e.complete = true
		return nil, nil
	}
	if code != 1 || len(packet) < 5 || e.complete {
		return nil, errWiFiEAP
	}
	if bytes.Equal(packet, e.request) {
		return bytes.Clone(e.response), nil
	}
	if e.seen[id] || e.rounds >= 12 {
		return nil, errWiFiEAP
	}
	e.seen[id] = true
	e.rounds++
	if packet[4] == 1 {
		if e.verified || e.challenges > 0 || e.outerIdentity {
			return nil, errWiFiEAP
		}
		e.outerIdentity = true
		reply = append([]byte{2, id, 0, 0, 1}, e.identity...)
	} else {
		if packet[4] != eapAKA || len(packet) < 8 {
			return nil, errWiFiEAP
		}
		attrs, parseErr := parseAKAAttributes(packet)
		if parseErr != nil {
			return nil, parseErr
		}
		switch packet[5] {
		case akaIdentity:
			if e.verified || e.challenges > 0 || e.identities >= 3 {
				return nil, errWiFiEAP
			}
			count := 0
			for _, kind := range []byte{atAnyID, atPermanentID, atFullID} {
				if attr, ok := attrs[kind]; ok {
					if len(attr.data) != 4 {
						return nil, errWiFiEAP
					}
					count++
				}
			}
			if count != 1 || !onlyAKAAttributes(attrs, atAnyID, atPermanentID, atFullID) {
				return nil, errWiFiEAP
			}
			e.identities++
			reply = akaPacket(id, akaIdentity, akaAttribute(atIdentity, uint16(len(e.identity)), []byte(e.identity)))
		case akaChallenge:
			reply, err = e.challenge(ctx, packet, attrs)
		case akaNotification:
			note, mac := attrs[atNotification], attrs[atMAC]
			if !e.verified || !e.resultPending || len(note.data) != 4 || binary.BigEndian.Uint16(note.data[2:]) != 32768 || !onlyAKAAttributes(attrs, atNotification, atMAC) || !validAKAMAC(packet, mac, e.keys[16:32]) {
				return nil, errWiFiEAP
			}
			e.resultPending = false
			reply = akaPacket(id, akaNotification, akaAttribute(atMAC, 0, make([]byte, 16)))
			signAKA(reply, e.keys[16:32])
		default:
			return nil, errors.New("WIFI_AUTH_UNSUPPORTED")
		}
		if err != nil {
			return nil, err
		}
	}
	binary.BigEndian.PutUint16(reply[2:4], uint16(len(reply)))
	if packet[4] == eapAKA && packet[5] == akaIdentity {
		e.transcript = append(e.transcript, packet...)
		e.transcript = append(e.transcript, reply...)
	}
	clear(e.request)
	clear(e.response)
	e.request, e.response = bytes.Clone(packet), bytes.Clone(reply)
	return reply, nil
}

type akaAttributeValue struct {
	data   []byte
	offset int
}

func parseAKAAttributes(packet []byte) (map[byte]akaAttributeValue, error) {
	if len(packet) < 8 || len(packet) > 4096 || int(binary.BigEndian.Uint16(packet[2:4])) != len(packet) {
		return nil, errWiFiEAP
	}
	attrs := make(map[byte]akaAttributeValue)
	for offset := 8; offset < len(packet); {
		if len(packet)-offset < 4 {
			return nil, errWiFiEAP
		}
		kind, size := packet[offset], int(packet[offset+1])*4
		if size < 4 || size > len(packet)-offset {
			return nil, errWiFiEAP
		}
		if _, exists := attrs[kind]; exists {
			return nil, errWiFiEAP
		}
		attrs[kind] = akaAttributeValue{packet[offset : offset+size], offset}
		offset += size
	}
	return attrs, nil
}

func onlyAKAAttributes(attrs map[byte]akaAttributeValue, allowed ...byte) bool {
	for kind := range attrs {
		if kind < 128 && bytes.IndexByte(allowed, kind) < 0 {
			return false
		}
	}
	return true
}

func (e *wifiEAP) challenge(ctx context.Context, packet []byte, attrs map[byte]akaAttributeValue) ([]byte, error) {
	rand, autn, mac := attrs[atRAND], attrs[atAUTN], attrs[atMAC]
	if e.verified || e.challenges >= 3 || len(rand.data) != 20 || len(autn.data) != 20 || len(mac.data) != 20 || !onlyAKAAttributes(attrs, atRAND, atAUTN, atMAC) {
		return nil, errWiFiEAP
	}
	if check, ok := attrs[atCheckcode]; ok {
		if len(e.transcript) == 0 {
			if len(check.data) != 4 {
				return nil, errWiFiEAP
			}
		} else {
			digest := sha1.Sum(e.transcript)
			if len(check.data) != 24 || !hmac.Equal(check.data[4:], digest[:]) {
				return nil, errWiFiEAP
			}
		}
	}
	result, wantsResult := attrs[atResult]
	if wantsResult && len(result.data) != 4 {
		return nil, errWiFiEAP
	}
	e.challenges++
	answer, err := e.authenticate(ctx, rand.data[4:], autn.data[4:])
	defer answer.clear()
	if err != nil || ctx.Err() != nil {
		return nil, errors.New("WIFI_SIM_AUTH_FAILED")
	}
	if len(answer.AUTS) != 0 {
		if len(answer.AUTS) != 14 || len(answer.RES)+len(answer.CK)+len(answer.IK) != 0 {
			return nil, errWiFiEAP
		}
		// AT_AUTS has no reserved field: 2-byte header followed by 14 bytes.
		return akaPacket(packet[1], 4, append([]byte{atAUTS, 4}, answer.AUTS...)), nil
	}
	if len(answer.RES) < 4 || len(answer.RES) > 16 || len(answer.CK) != 16 || len(answer.IK) != 16 {
		return nil, errWiFiEAP
	}
	hash := sha1.New()
	hash.Write([]byte(e.identity))
	hash.Write(answer.IK)
	hash.Write(answer.CK)
	seed := hash.Sum(nil)
	e.keys = akaPRF(seed)
	clear(seed)
	if !validAKAMAC(packet, mac, e.keys[16:32]) {
		return nil, errWiFiEAP
	}
	responseAttrs := akaAttribute(atRES, uint16(len(answer.RES)*8), answer.RES)
	if wantsResult {
		responseAttrs = append(responseAttrs, akaAttribute(atResult, 0, nil)...)
	}
	if check, ok := attrs[atCheckcode]; ok {
		responseAttrs = append(responseAttrs, akaAttribute(atCheckcode, 0, check.data[4:])...)
	}
	responseAttrs = append(responseAttrs, akaAttribute(atMAC, 0, make([]byte, 16))...)
	reply := akaPacket(packet[1], akaChallenge, responseAttrs)
	signAKA(reply, e.keys[16:32])
	e.verified, e.resultPending = true, wantsResult
	return reply, nil
}

func akaAttribute(kind byte, value uint16, data []byte) []byte {
	size := (4 + len(data) + 3) / 4 * 4
	attr := make([]byte, size)
	attr[0], attr[1] = kind, byte(size/4)
	binary.BigEndian.PutUint16(attr[2:4], value)
	copy(attr[4:], data)
	return attr
}

func akaPacket(id, subtype byte, attrs []byte) []byte {
	packet := append([]byte{2, id, 0, 0, eapAKA, subtype, 0, 0}, attrs...)
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	return packet
}

func akaMAC(packet []byte, attr akaAttributeValue, key []byte) []byte {
	h := hmac.New(sha1.New, key)
	h.Write(packet[:attr.offset+4])
	h.Write(make([]byte, 16))
	h.Write(packet[attr.offset+20:])
	return h.Sum(nil)[:16]
}

func validAKAMAC(packet []byte, attr akaAttributeValue, key []byte) bool {
	if len(attr.data) != 20 {
		return false
	}
	want := akaMAC(packet, attr, key)
	defer clear(want)
	return hmac.Equal(want, attr.data[4:])
}

func signAKA(packet, key []byte) {
	attrs, _ := parseAKAAttributes(packet)
	attr := attrs[atMAC]
	mac := akaMAC(packet, attr, key)
	copy(packet[attr.offset+4:], mac)
	clear(mac)
}
