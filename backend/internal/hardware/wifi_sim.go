package hardware

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Subscriber identities and AKA keys never enter the public status response.
type wifiIdentity struct {
	IMSI, ICCID     string
	MCC, MNC        string
	SPN, GID1, GID2 string
}

func (v wifiIdentity) epdg() string {
	return fmt.Sprintf("epdg.epc.mnc%03s.mcc%s.pub.3gppnetwork.org", v.MNC, v.MCC)
}

type akaResult struct {
	RES, CK, IK, AUTS []byte
}

func (v *akaResult) clear() {
	clear(v.RES)
	clear(v.CK)
	clear(v.IK)
	clear(v.AUTS)
	*v = akaResult{}
}

type wifiSIM struct {
	session            *atSession
	card               *cardChannel
	profile            wifiCarrier
	preferredTransport string
	id                 wifiIdentity
}

// Caller owns the module gate for the whole session, including cleanup.
func inspectWiFiSIM(ctx context.Context, session *atSession, expectedICCID string) (*wifiSIM, error) {
	if !decimal(expectedICCID, 18, 20) {
		return nil, errors.New("DEVICE_CHANGED")
	}
	lines, err := session.exchange(ctx, "AT+CPIN?", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("WIFI_SIM_PIN: %w", err)
	}
	ready := false
	for _, line := range lines {
		ready = ready || strings.TrimSpace(line) == "+CPIN: READY"
	}
	if !ready {
		return nil, errors.New("SIM_NOT_READY")
	}
	lines, err = session.exchange(ctx, "AT+QCCID", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("WIFI_SIM_ICCID: %w", err)
	}
	if digits(lines, 18, 20) != expectedICCID {
		return nil, errors.New("DEVICE_CHANGED")
	}
	s := &wifiSIM{session: session, card: atCard(ctx, session)}
	// Partial AID selection chooses the USIM in the currently enabled profile.
	aid, _ := hex.DecodeString("A0000000871002")
	if _, err = s.card.OpenLogicalChannel(aid); err != nil {
		return nil, fmt.Errorf("WIFI_SIM_CHANNEL: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.card.CloseLogicalChannel(s.card.channel)
		}
	}()
	imsi, err := s.readFile(ctx, 0x6f07, 9)
	if err != nil {
		return nil, fmt.Errorf("WIFI_SIM_IMSI: %w", err)
	}
	defer clear(imsi)
	ad, err := s.readFile(ctx, 0x6fad, 4)
	if err != nil {
		return nil, fmt.Errorf("WIFI_SIM_AD: %w", err)
	}
	s.id, err = wifiSIMIdentity(imsi, ad)
	if err != nil {
		return nil, err
	}
	s.id.ICCID = expectedICCID
	// Optional selectors are read through the same exclusive logical channel.
	if raw, e := s.readFile(ctx, 0x6f46, 17); e == nil {
		s.id.SPN = wifiSPN(raw)
		clear(raw)
	}
	if raw, e := s.readFile(ctx, 0x6f3e, 8); e == nil {
		s.id.GID1 = strings.ToUpper(hex.EncodeToString(raw))
		clear(raw)
	}
	if raw, e := s.readFile(ctx, 0x6f3f, 8); e == nil {
		s.id.GID2 = strings.ToUpper(hex.EncodeToString(raw))
		clear(raw)
	}
	carrierOnce.Do(loadWiFiCarriers)
	if carrierError != nil {
		return nil, carrierError
	}
	s.profile = resolveWiFiCarrier(s.id, carrierRules)
	if s.profile.Unsupported != "" {
		return nil, errors.New("WIFI_CARRIER_UNSUPPORTED")
	}

	// Catch a profile/card change during the read before publishing its identity.
	if err = s.verifyCard(ctx); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func (s *wifiSIM) verifyCard(ctx context.Context) error {
	lines, err := s.session.exchange(ctx, "AT+QCCID", 5*time.Second)
	if err != nil {
		return err
	}
	if digits(lines, 18, 20) != s.id.ICCID {
		return errors.New("DEVICE_CHANGED")
	}
	return nil
}

func (s *wifiSIM) close() error {
	return s.card.CloseLogicalChannel(s.card.channel)
}

func (s *wifiSIM) readFile(ctx context.Context, file uint16, size byte) ([]byte, error) {
	cla := channelClass(0, s.card.channel)
	if _, err := s.exchange(ctx, []byte{cla, 0xa4, 0, 0x0c, 2, byte(file >> 8), byte(file)}); err != nil {
		return nil, err
	}
	return s.exchange(ctx, []byte{cla, 0xb0, 0, 0, size})
}

func (s *wifiSIM) exchange(ctx context.Context, command []byte) ([]byte, error) {
	command = bytes.Clone(command)
	defer clear(command)
	var data []byte
	for i := 0; i < 8; i++ {
		response, err := s.card.send(ctx, command)
		if err != nil {
			clear(data)
			return nil, err
		}
		if len(response) < 2 || len(response) > 4096 {
			clear(data)
			return nil, errors.New("INVALID_RESPONSE")
		}
		sw1, sw2 := response[len(response)-2], response[len(response)-1]
		switch {
		case sw1 == 0x6c && len(command) == 5:
			command[4] = sw2
		case sw1 == 0x61:
			data = append(data, response[:len(response)-2]...)
			clear(command)
			command = []byte{channelClass(0, s.card.channel), 0xc0, 0, 0, sw2}
		case sw1 == 0x90 && sw2 == 0:
			data = append(data, response[:len(response)-2]...)
			clear(response)
			return data, nil
		default:
			clear(response)
			clear(data)
			if sw1 == 0x98 && sw2 == 0x62 {
				return nil, errors.New("AKA_REJECTED")
			}
			return nil, errors.New("SIM_APPLICATION_ERROR")
		}
		clear(response)
	}
	clear(data)
	return nil, errors.New("INVALID_RESPONSE")
}

func wifiSIMIdentity(imsi, ad []byte) (wifiIdentity, error) {
	invalid := errors.New("SIM_IDENTITY_INVALID")
	if len(imsi) != 9 || imsi[0] < 2 || imsi[0] > 8 || len(ad) < 4 {
		return wifiIdentity{}, invalid
	}
	var digits []byte
	// EF_IMSI stores the first digit above the parity/type nibble.
	digits = append(digits, imsi[1]>>4)
	for _, b := range imsi[2 : int(imsi[0])+1] {
		digits = append(digits, b&15, b>>4)
	}
	if imsi[1]&8 == 0 && digits[len(digits)-1] == 15 {
		digits = digits[:len(digits)-1]
	}
	for i, digit := range digits {
		if digit > 9 {
			return wifiIdentity{}, invalid
		}
		digits[i] += '0'
	}
	mncLength := int(ad[3] & 15)
	if len(digits) < 6 || len(digits) > 15 || (mncLength != 2 && mncLength != 3) {
		return wifiIdentity{}, invalid
	}
	value := string(digits)
	return wifiIdentity{IMSI: value, MCC: value[:3], MNC: value[3 : 3+mncLength]}, nil
}

// Only a verified network challenge should call this; inspection never authenticates.
func (s *wifiSIM) authenticate(ctx context.Context, rand, autn []byte) (akaResult, error) {
	if len(rand) != 16 || len(autn) != 16 {
		return akaResult{}, errors.New("AKA_CHALLENGE_INVALID")
	}
	if err := s.verifyCard(ctx); err != nil {
		return akaResult{}, err
	}
	command := []byte{channelClass(0, s.card.channel), 0x88, 0, 0x81, 34, 16}
	command = append(command, rand...)
	command = append(command, 16)
	command = append(command, autn...)
	command = append(command, 0)
	defer clear(command)
	data, err := s.exchange(ctx, command)
	if err != nil {
		return akaResult{}, err
	}
	defer clear(data)
	if err := s.verifyCard(ctx); err != nil {
		return akaResult{}, err
	}
	return parseAKA(data)
}

func parseAKA(data []byte) (akaResult, error) {
	invalid := errors.New("AKA_RESPONSE_INVALID")
	if len(data) < 2 {
		return akaResult{}, invalid
	}
	tag, rest := data[0], data[1:]
	read := func(min, max int) []byte {
		if len(rest) == 0 || int(rest[0]) < min || int(rest[0]) > max || len(rest) < int(rest[0])+1 {
			return nil
		}
		n := int(rest[0])
		value := bytes.Clone(rest[1 : n+1])
		rest = rest[n+1:]
		return value
	}
	var result akaResult
	if tag == 0xdc {
		result.AUTS = read(14, 14)
		if result.AUTS != nil && len(rest) == 0 {
			return result, nil
		}
	} else if tag == 0xdb {
		result.RES = read(4, 16)
		result.CK = read(16, 16)
		result.IK = read(16, 16)
		valid := result.RES != nil && result.CK != nil && result.IK != nil
		if len(rest) > 0 {
			kc := read(8, 8)
			valid = valid && kc != nil
			clear(kc)
		}
		if valid && len(rest) == 0 {
			return result, nil
		}
	}
	result.clear()
	return akaResult{}, invalid
}

// Local read-only probe. No RF, PDP, profile or authentication commands.
func WiFiSIMCheck() error { return wifiProbe(false) }
func WiFiSIMRun() error   { return wifiProbe(true) }
func wifiProbe(run bool) error {
	var request struct {
		Endpoint, Generation, HardwareKey, ICCID string
		HoldSeconds                              int
	}
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return errors.New("INVALID_REQUEST")
	}
	if !strings.HasPrefix(request.HardwareKey, "imei:") || len(request.HardwareKey) != 69 || !decimal(request.ICCID, 18, 20) {
		return errors.New("INVALID_REQUEST")
	}
	limit := 25 * time.Second
	if run {
		if request.HoldSeconds < 0 || request.HoldSeconds > 3600 {
			return errors.New("INVALID_REQUEST")
		}
		limit = time.Duration(120+request.HoldSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	devices, err := NewSystem().Discover(ctx)
	if err != nil {
		return err
	}
	for _, c := range devices {
		if c.Key != request.Endpoint || c.Generation != request.Generation || len(c.Ports) == 0 {
			continue
		}

		session, err := openWiFiSession(ctx, c, request.HardwareKey)
		if err != nil {
			return err
		}
		defer session.port.Close()

		if run {
			return runWiFiSIM(ctx, c, session, request.ICCID, time.Duration(request.HoldSeconds)*time.Second)
		}
		sim, err := inspectWiFiSIM(ctx, session, request.ICCID)
		if err != nil {
			return err
		}
		if err := sim.close(); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"simReadable": true, "mcc": sim.id.MCC, "mnc": sim.id.MNC, "epdg": sim.id.epdg(), "registered": false})
	}
	return errors.New("DEVICE_CHANGED")
}
