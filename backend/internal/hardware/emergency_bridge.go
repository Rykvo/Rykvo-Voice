package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"rykvo.local/auth/internal/entitlement"
	"time"
)

type emergencyCommand struct {
	Op string `json:"op"`
	ID string `json:"id"`
}
type emergencyReply struct {
	ID   string            `json:"id"`
	Page *entitlement.Page `json:"page,omitempty"`
	Code string            `json:"code,omitempty"`
}
type emergencyHandler func(context.Context) (entitlement.Page, error)

func (s *smsClientSession) writeEmergency(c emergencyCommand) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return json.NewEncoder(s.conn).Encode(c)
}
func (client *VocatWorkerClient) EmergencyAddress(ctx context.Context, c Candidate, identity, card string) (entitlement.Page, error) {
	client.smsMu.Lock()
	s := client.smsSessions[smsSessionKey(c, card)]
	client.smsMu.Unlock()
	if s == nil {
		return entitlement.Page{}, entitlement.ErrUnavailable
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return entitlement.Page{}, ctx.Err()
	case <-s.closed:
		return entitlement.Page{}, entitlement.ErrUnavailable
	}
	id := Digest(identity + card + time.Now().String())
	ch := make(chan emergencyReply, 1)
	s.mu.Lock()
	if len(s.emergencyPending) > 0 {
		s.mu.Unlock()
		return entitlement.Page{}, errors.New("DEVICE_BUSY")
	}
	s.emergencyPending = map[string]chan emergencyReply{id: ch}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.emergencyPending, id); s.mu.Unlock() }()
	if s.writeEmergency(emergencyCommand{Op: "emergency-start", ID: id}) != nil {
		return entitlement.Page{}, entitlement.ErrUnavailable
	}
	select {
	case r := <-ch:
		if r.Code != "" {
			return entitlement.Page{}, errors.New(r.Code)
		}
		if r.Page == nil {
			return entitlement.Page{}, entitlement.ErrInvalid
		}
		return *r.Page, nil
	case <-ctx.Done():
		_ = s.writeEmergency(emergencyCommand{Op: "emergency-cancel", ID: id})
		return entitlement.Page{}, ctx.Err()
	case <-s.closed:
		return entitlement.Page{}, entitlement.ErrUnavailable
	}
}
func (s *smsClientSession) emergencyEvent(r emergencyReply) bool {
	if !smsRequestID.MatchString(r.ID) || (r.Page == nil) == (r.Code == "") {
		return false
	}
	if r.Page != nil && (len(r.Page.URL) > 2048 || len(r.Page.Token) > 16384) {
		return false
	}
	s.mu.Lock()
	ch := s.emergencyPending[r.ID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
			return false
		}
	}
	return true
}
func (w *smsWorker) setEmergencyHandler(handler emergencyHandler) {
	w.mu.Lock()
	w.emergencyHandler = handler
	if handler == nil && w.emergencyCancel != nil {
		w.emergencyCancel()
	}
	w.mu.Unlock()
}
func (w *smsWorker) emergencyCommand(data []byte) bool {
	var c emergencyCommand
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || !smsRequestID.MatchString(c.ID) {
		return false
	}
	w.mu.Lock()
	if c.Op == "emergency-cancel" {
		if w.emergencyID == c.ID && w.emergencyCancel != nil {
			w.emergencyCancel()
		}
		w.mu.Unlock()
		return true
	}
	if c.Op != "emergency-start" {
		w.mu.Unlock()
		return false
	}
	code := ""
	handler := w.emergencyHandler
	if handler == nil {
		code = "EMERGENCY_UNAVAILABLE"
	} else if w.emergencyCancel != nil {
		code = "DEVICE_BUSY"
	}
	if code != "" {
		w.mu.Unlock()
		w.emit(wifiWorkerEvent{EmergencyResult: &emergencyReply{ID: c.ID, Code: code}})
		return true
	}
	ctx, cancel := context.WithTimeout(w.ctx, 75*time.Second)
	w.emergencyID, w.emergencyCancel = c.ID, cancel
	w.emergencyWait.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.emergencyWait.Done()
		defer cancel()
		page, err := handler(ctx)
		r := emergencyReply{ID: c.ID, Code: EmergencyIssue(err)}
		if err == nil {
			r.Page = &page
		}
		w.mu.Lock()
		w.emergencyID, w.emergencyCancel = "", nil
		w.mu.Unlock()
		w.emit(wifiWorkerEvent{EmergencyResult: &r})
	}()
	return true
}
