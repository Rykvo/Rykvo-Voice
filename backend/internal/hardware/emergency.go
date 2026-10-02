package hardware

import (
	"context"
	"errors"
	"fmt"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/entitlement"
	"rykvo.local/auth/internal/vocat/vowifi"
	"time"
)

type emergencyAKA struct{ *wifiEAP }

func (a emergencyAKA) Respond(ctx context.Context, packet []byte) ([]byte, error) {
	return a.handle(ctx, packet)
}
func (a emergencyAKA) Verified() bool { return !a.closed && a.verified && !a.resultPending }
func emergencyAddress(ctx context.Context, at *vocatAT, adapter *vowifi.EC20Adapter) (entitlement.Page, error) {
	id, err := adapter.ReadIdentity(ctx, at.device)
	if err != nil || id.ICCID != at.iccid {
		return entitlement.Page{}, errors.New("DEVICE_CHANGED")
	}
	provider, ok := carrierconfig.EmergencyByID(carrierconfig.EmergencyMatch(carrierconfig.Identity{MCC: id.HomeMCC, MNC: id.HomeMNC, IMSI: id.IMSI, ICCID: id.ICCID, SPN: id.SPN, GID1: id.GID1, GID2: id.GID2}))
	if !ok {
		return entitlement.Page{}, errors.New("EMERGENCY_UNSUPPORTED")
	}
	eap := &wifiEAP{identity: fmt.Sprintf("0%s@nai.epc.mnc%03s.mcc%s.3gppnetwork.org", id.IMSI, id.HomeMNC, id.HomeMCC), authenticate: func(call context.Context, rand, autn []byte) (akaResult, error) {
		if err := at.verify(call); err != nil {
			return akaResult{}, err
		}
		var challenge vowifi.AKAChallenge
		copy(challenge.RAND[:], rand)
		copy(challenge.AUTN[:], autn)
		defer clear(challenge.RAND[:])
		defer clear(challenge.AUTN[:])
		result, err := adapter.Authenticate(call, id, challenge)
		return akaResult{RES: result.RES, CK: result.CK, IK: result.IK, AUTS: result.AUTS}, err
	}}
	defer eap.close()
	page, err := entitlement.Discover(ctx, provider, eap.identity, id.IMEI, emergencyAKA{eap})
	if err == nil {
		err = at.verify(ctx)
	}
	if err != nil {
		return entitlement.Page{}, err
	}
	return page, nil
}

// Caller owns the gate. RF and Wi-Fi preferences remain unchanged.
func (s *System) EmergencyAddress(ctx context.Context, c Candidate, identity, card string) (entitlement.Page, error) {
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	devices, err := s.Discover(ctx)
	if err != nil {
		return entitlement.Page{}, errors.New("DEVICE_UNAVAILABLE")
	}
	found := false
	for _, current := range devices {
		if current.Key == c.Key && current.Generation == c.Generation {
			c, found = current, true
			break
		}
	}
	if !found {
		return entitlement.Page{}, errors.New("DEVICE_CHANGED")
	}
	session, err := openWiFiSession(ctx, c, identity)
	if err != nil {
		return entitlement.Page{}, err
	}
	defer session.port.Close()
	at := &vocatAT{session: session, device: c.Key, iccid: card}
	adapter, err := vowifi.NewEC20Adapter(at, vowifi.EC20AdapterOptions{})
	if err != nil {
		return entitlement.Page{}, entitlement.ErrUnavailable
	}
	return emergencyAddress(ctx, at, adapter)
}
func EmergencyIssue(err error) string {
	if err == nil {
		return ""
	}
	for _, code := range []string{"DEVICE_CHANGED", "DEVICE_BUSY", "DEVICE_UNAVAILABLE", "EMERGENCY_UNSUPPORTED", "EMERGENCY_UNAVAILABLE", "EMERGENCY_REJECTED", "EMERGENCY_INVALID_RESPONSE"} {
		if err.Error() == code {
			return code
		}
	}
	return "EMERGENCY_UNAVAILABLE"
}
