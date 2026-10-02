package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/entitlement"
	"rykvo.local/auth/internal/hardware"
	"sync"
	"time"
)

type emergencySource interface {
	EmergencyAddress(context.Context, hardware.Candidate, string, string) (entitlement.Page, error)
}
type emergencySessions struct {
	sync.Mutex
	items map[string]*emergencySession
}
type emergencySession struct {
	id, owner, line, card, generation, provider string
	module, epoch                               int64
	expires                                     time.Time
	state, issue                                string
	page                                        entitlement.Page
	cancel                                      context.CancelFunc
	timer                                       *time.Timer
}

func (s *emergencySessions) remove(item *emergencySession) {
	item.cancel()
	if item.timer != nil {
		item.timer.Stop()
	}
	item.page = entitlement.Page{}
	delete(s.items, item.id)
}

func (m *moduleManager) openEmergency(ctx context.Context, v moduleRecord, card string) (entitlement.Page, error) {
	m.mu.RLock()
	sample, exists := m.values[v.ID]
	wifi := m.wifi[v.ID]
	running := wifi != nil && wifi.running && wifi.ICCID == card
	var source emergencySource
	if running {
		source, _ = m.wifiEngine.(emergencySource)
	} else {
		source, _ = m.source.(emergencySource)
	}
	valid := exists && sample.Candidate.Key == v.Endpoint && sample.Reading.ICCID == card && sample.Reading.SIM == "READY"
	m.mu.RUnlock()
	if !valid {
		return entitlement.Page{}, errors.New("DEVICE_CHANGED")
	}
	if source == nil {
		return entitlement.Page{}, entitlement.ErrUnavailable
	}
	release := m.reserveModuleWork(sample)
	if release == nil {
		return entitlement.Page{}, errors.New("DEVICE_BUSY")
	}
	defer release()
	if !running {
		gate := m.gate(v.Endpoint)
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-ctx.Done():
			return entitlement.Page{}, ctx.Err()
		}
	}
	page, err := source.EmergencyAddress(ctx, sample.Candidate, v.Identity, card)
	if err != nil {
		return entitlement.Page{}, err
	}
	m.mu.RLock()
	valid = m.moduleWorkAllowedLocked(sample)
	m.mu.RUnlock()
	if !valid {
		return entitlement.Page{}, errors.New("DEVICE_CHANGED")
	}
	return page, nil
}

func (s *server) emergencyBinding(ctx context.Context, id int64) (card string, epoch int64, generation string, err error) {
	err = s.db.QueryRow(ctx, "SELECT active_card,card_epoch,endpoint_generation FROM modules WHERE id=$1", id).Scan(&card, &epoch, &generation)
	return
}

func (s *server) moduleEmergency(ctx context.Context, w http.ResponseWriter, r *http.Request, v moduleRecord, parts []string) {
	if len(parts) != 5 && len(parts) != 6 || parts[4] != "session" {
		fail(w, 404, "NOT_FOUND")
		return
	}
	if s.modules == nil {
		fail(w, 503, "EMERGENCY_UNAVAILABLE")
		return
	}
	owner := base64.RawURLEncoding.EncodeToString(tokenHash(requestToken(r)))
	card, epoch, generation, err := s.emergencyBinding(ctx, v.ID)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	lineValid := false
	for _, entry := range s.moduleView(v)["sims"].([]any) {
		line, ok := entry.(map[string]any)
		if ok && line["id"] == parts[2] && line["iccid"] == card && line["enabled"] == true {
			lineValid = true
			break
		}
	}
	if len(parts) == 5 {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			fail(w, 405, "METHOD_NOT_ALLOWED")
			return
		}
		if !lineValid || card == "" {
			fail(w, 409, "DEVICE_CHANGED")
			return
		}
		s.modules.mu.RLock()
		selection := s.modules.carrierConfigs[card]
		parent := s.modules.ctx
		s.modules.mu.RUnlock()
		provider, ok := carrierconfig.EmergencyByID(selection.Emergency)
		if !ok || !selection.Valid() {
			fail(w, 409, "EMERGENCY_UNSUPPORTED")
			return
		}
		if parent == nil || parent.Err() != nil {
			fail(w, 503, "EMERGENCY_UNAVAILABLE")
			return
		}
		s.emergency.Lock()
		if s.emergency.items == nil {
			s.emergency.items = map[string]*emergencySession{}
		}
		for _, item := range s.emergency.items {
			if time.Now().After(item.expires) {
				s.emergency.remove(item)
				continue
			}
			if item.module == v.ID {
				if item.owner != owner {
					s.emergency.Unlock()
					fail(w, 409, "DEVICE_BUSY")
					return
				}
				if item.owner == owner && item.card == card && item.epoch == epoch && item.generation == generation && item.line == parts[2] {
					result := emergencyView(item, false)
					s.emergency.Unlock()
					reply(w, 202, map[string]any{"data": result})
					return
				}
				s.emergency.remove(item)
			}
		}
		if len(s.emergency.items) >= 64 {
			s.emergency.Unlock()
			fail(w, 429, "EMERGENCY_BUSY")
			return
		}
		call, cancel := context.WithTimeout(parent, 75*time.Second)
		item := &emergencySession{id: token(), owner: owner, module: v.ID, line: parts[2], card: card, epoch: epoch, generation: generation, provider: provider.ID, expires: time.Now().Add(5 * time.Minute), state: "pending", cancel: cancel}
		s.emergency.items[item.id] = item
		result := emergencyView(item, false)
		item.timer = time.AfterFunc(time.Until(item.expires), func() {
			s.emergency.Lock()
			defer s.emergency.Unlock()
			if s.emergency.items[item.id] == item {
				s.emergency.remove(item)
			}
		})
		s.emergency.Unlock()
		go func() {
			defer cancel()
			page, err := s.modules.openEmergency(call, v, card)
			if err == nil && !page.Valid(provider) {
				err = entitlement.ErrInvalid
			}
			s.emergency.Lock()
			defer s.emergency.Unlock()
			if s.emergency.items[item.id] != item || call.Err() != nil {
				if s.emergency.items[item.id] == item {
					item.state, item.issue = "failed", "EMERGENCY_UNAVAILABLE"
				}
				return
			}
			if err != nil {
				item.state, item.issue = "failed", hardware.EmergencyIssue(err)
			} else {
				item.state, item.page = "ready", page
			}
		}()
		reply(w, 202, map[string]any{"data": result})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, DELETE")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	s.emergency.Lock()
	defer s.emergency.Unlock()
	item := s.emergency.items[parts[5]]
	if item == nil || item.owner != owner || item.module != v.ID || item.line != parts[2] {
		fail(w, 404, "EMERGENCY_SESSION_EXPIRED")
		return
	}
	valid := lineValid && card == item.card && epoch == item.epoch && generation == item.generation && time.Now().Before(item.expires)
	if r.Method == http.MethodDelete || !valid {
		s.emergency.remove(item)
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
		} else {
			fail(w, 409, "EMERGENCY_SESSION_EXPIRED")
		}
		return
	}
	reply(w, 200, map[string]any{"data": emergencyView(item, true)})
}
func emergencyView(item *emergencySession, includePage bool) map[string]any {
	out := map[string]any{"id": item.id, "state": item.state, "expiresAt": item.expires}
	if item.issue != "" {
		out["issue"] = item.issue
	}
	if includePage && item.state == "ready" {
		out["page"] = item.page
	}
	return out
}
