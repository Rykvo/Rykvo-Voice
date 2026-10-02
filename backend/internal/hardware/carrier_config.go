package hardware

import (
	"context"
	"encoding/json"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/vocat/vowifi"
	"sync"
	"time"
)

type carrierIdentityReader interface {
	vowifi.SIMIdentityReader
	vowifi.SMSCenterReader
}

type carrierSIM struct {
	carrierIdentityReader
	emit func(string)
}

func (s carrierSIM) ReadIdentity(ctx context.Context, id string) (vowifi.SIMIdentity, error) {
	identity, err := s.carrierIdentityReader.ReadIdentity(ctx, id)
	if err == nil {
		selection := matchCarrier(identity)
		if selection.Valid() {
			b, _ := json.Marshal(selection)
			s.emit("config:" + string(b))
		}
	}
	return identity, err
}
func matchCarrier(id vowifi.SIMIdentity) carrierconfig.Selection {
	return carrierconfig.Match(carrierconfig.Identity{MCC: id.HomeMCC, MNC: id.HomeMNC, SPN: id.SPN, GID1: id.GID1, GID2: id.GID2, IMSI: id.IMSI, ICCID: id.ICCID})
}

type carrierCacheEntry struct {
	at    time.Time
	value *carrierconfig.Selection
}

func (e carrierCacheEntry) fresh(now time.Time) bool {
	validFor := 15 * time.Minute
	if e.value == nil {
		validFor = 30 * time.Second
	}
	age := now.Sub(e.at)
	return age >= 0 && age < validFor
}

type carrierCache struct {
	sync.Mutex
	entries map[string]carrierCacheEntry
}

func (s *System) carrierConfiguration(ctx context.Context, c Candidate, r Reading) *carrierconfig.Selection {
	if r.SIM != "READY" || r.ICCID == "" || !CommunicationHealthy(r) || !WiFiSupported(c) {
		return nil
	}
	key := Digest(c.Identity(r) + "\x00" + r.ICCID + "\x00" + c.Generation)
	s.carriers.Lock()
	if entry, ok := s.carriers.entries[key]; ok && entry.fresh(time.Now()) {
		s.carriers.Unlock()
		return entry.value
	}
	s.carriers.Unlock()
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 8*time.Second {
		return nil
	}
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var value *carrierconfig.Selection
	session, err := openWiFiSession(call, c, c.Identity(r))
	if err == nil {
		defer session.port.Close()
		at := &vocatAT{session: session, device: c.Key, iccid: r.ICCID}
		adapter, e := vowifi.NewEC20Adapter(at, vowifi.EC20AdapterOptions{})
		if e == nil {
			identity, e := adapter.ReadIdentity(call, c.Key)
			if e == nil && identity.ICCID == r.ICCID {
				selection := matchCarrier(identity)
				if selection.Valid() {
					value = &selection
				}
			}
		}
	}
	s.carriers.Lock()
	defer s.carriers.Unlock()
	if len(s.carriers.entries) > 128 {
		s.carriers.entries = nil
	}
	if s.carriers.entries == nil {
		s.carriers.entries = map[string]carrierCacheEntry{}
	}
	s.carriers.entries[key] = carrierCacheEntry{at: time.Now(), value: value}
	return value
}
