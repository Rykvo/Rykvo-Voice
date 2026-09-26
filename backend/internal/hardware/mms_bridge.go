package hardware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/mms"
)

const mmsIPCMax = 3 * 1024 * 1024

type mmsCommand struct {
	Op          string                `json:"op"`
	ID          string                `json:"id"`
	Profile     carrierconfig.Profile `json:"profile,omitempty"`
	Location    string                `json:"location,omitempty"`
	Transaction string                `json:"transaction,omitempty"`
	To          string                `json:"to,omitempty"`
	Text        string                `json:"text,omitempty"`
	Image       *mms.Part             `json:"image,omitempty"`
	Stored      bool                  `json:"stored,omitempty"`
}
type mmsReply struct {
	ID        string   `json:"id"`
	Content   *mms.PDU `json:"content,omitempty"`
	MessageID string   `json:"messageId,omitempty"`
	Code      string   `json:"code,omitempty"`
}
type mmsHandler func(context.Context, mmsCommand, func(mms.PDU) error) (string, error)

func (s *smsClientSession) writeMMS(c mmsCommand) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return json.NewEncoder(s.conn).Encode(c)
}
func (client *VocatWorkerClient) exchangeMMS(ctx context.Context, c Candidate, card string, request mmsCommand, store func(mms.PDU) error) (string, error) {
	if !smsRequestID.MatchString(request.ID) {
		return "", mms.ErrPDU
	}
	if request.Op == "mms-receive" {
		if _, err := mms.ReceiveURL(request.Profile, request.Location); err != nil {
			return "", err
		}
		if store == nil || len(request.Transaction) < 1 || len(request.Transaction) > 80 || strings.ContainsAny(request.Transaction, "\x00\r\n") {
			return "", mms.ErrPDU
		}
	} else if _, err := mms.SendRequest(request.ID, request.To, request.Text, request.Image); err != nil {
		return "", err
	}
	client.smsMu.Lock()
	s := client.smsSessions[smsSessionKey(c, card)]
	client.smsMu.Unlock()
	if s == nil {
		return "", mms.ErrNetwork
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return "", mms.ErrNetwork
	case <-s.closed:
		return "", mms.ErrNetwork
	}
	ch := make(chan mmsReply, 2)
	s.mu.Lock()
	if len(s.mmsPending) != 0 {
		s.mu.Unlock()
		return "", errors.New("MMS_BUSY")
	}
	if s.mmsPending == nil {
		s.mmsPending = map[string]chan mmsReply{}
	}
	s.mmsPending[request.ID] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.mmsPending, request.ID); s.mu.Unlock() }()
	if s.writeMMS(request) != nil {
		return "", mms.ErrUnknown
	}
	stored := false
	for {
		select {
		case r := <-ch:
			if r.Content != nil {
				if stored || store == nil || r.Content.Type != 0x84 {
					return "", mms.ErrPDU
				}
				err := store(*r.Content)
				_ = s.writeMMS(mmsCommand{Op: "mms-stored", ID: request.ID, Stored: err == nil})
				if err != nil {
					return "", err
				}
				stored = true
			} else {
				if stored {
					return r.MessageID, nil
				} // Durable content wins over a lost MMSC acknowledgement.
				if r.Code != "" {
					if r.Code == mms.ErrNetwork.Error() {
						return "", mms.ErrNetwork
					}
					return "", errors.New(r.Code)
				}
				if request.Op == "mms-receive" {
					return "", mms.ErrPDU
				}
				return r.MessageID, nil
			}
		case <-ctx.Done():
			if stored {
				return "", nil
			}
			return "", mms.ErrUnknown
		case <-s.closed:
			if stored {
				return "", nil
			}
			return "", mms.ErrUnknown
		}
	}
}
func (client *VocatWorkerClient) ReceiveWiFiMMS(ctx context.Context, c Candidate, card, id string, p carrierconfig.Profile, location, transaction string, store func(mms.PDU) error) error {
	_, err := client.exchangeMMS(ctx, c, card, mmsCommand{Op: "mms-receive", ID: id, Profile: p, Location: location, Transaction: transaction}, store)
	return err
}
func (client *VocatWorkerClient) SendWiFiMMS(ctx context.Context, c Candidate, card string, p carrierconfig.Profile, id, to, text string, image *mms.Part) (string, error) {
	return client.exchangeMMS(ctx, c, card, mmsCommand{Op: "mms-send", ID: id, Profile: p, To: to, Text: text, Image: image}, nil)
}
func (w *smsWorker) setMMSHandler(f mmsHandler) { w.mu.Lock(); w.mmsHandler = f; w.mu.Unlock() }
func (w *smsWorker) mmsCommand(raw []byte) bool {
	var c mmsCommand
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || !smsRequestID.MatchString(c.ID) {
		return false
	}
	if c.Op == "mms-stored" {
		if c.Profile != (carrierconfig.Profile{}) || c.Location != "" || c.Transaction != "" || c.To != "" || c.Text != "" || c.Image != nil {
			return false
		}
		w.mu.Lock()
		ch := w.mmsAcks[c.ID]
		w.mu.Unlock()
		if ch != nil {
			select {
			case ch <- c.Stored:
			default:
			}
		}
		return true
	}
	if c.Stored {
		return false
	}
	switch c.Op {
	case "mms-receive":
		if c.To != "" || c.Text != "" || c.Image != nil || len(c.Transaction) < 1 || len(c.Transaction) > 80 || strings.ContainsAny(c.Transaction, "\x00\r\n") {
			return false
		}
		if _, err := mms.ReceiveURL(c.Profile, c.Location); err != nil {
			return false
		}
	case "mms-send":
		if c.Location != "" || c.Transaction != "" {
			return false
		}
		if _, err := mms.SendRequest(c.ID, c.To, c.Text, c.Image); err != nil {
			return false
		}
	default:
		return false
	}
	w.mu.Lock()
	f := w.mmsHandler
	code := ""
	if w.mmsBusy {
		code = "MMS_BUSY"
	} else if f == nil {
		code = "MMS_NETWORK_REQUIRED"
	} else if c.Op == "mms-send" && (w.used[c.ID] || len(w.used) >= 10000) {
		code = "MMS_OUTCOME_UNKNOWN"
	}
	if code != "" {
		w.mu.Unlock()
		w.emit(wifiWorkerEvent{MMSResult: &mmsReply{ID: c.ID, Code: code}})
		return true
	}
	w.mmsBusy = true
	w.mmsWait.Add(1)
	if c.Op == "mms-send" {
		w.used[c.ID] = true
	}
	ack := make(chan bool, 1)
	if w.mmsAcks == nil {
		w.mmsAcks = map[string]chan bool{}
	}
	w.mmsAcks[c.ID] = ack
	w.mu.Unlock()
	go func() {
		defer w.mmsWait.Done()
		defer func() { w.mu.Lock(); w.mmsBusy = false; delete(w.mmsAcks, c.ID); w.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(w.ctx, 200*time.Second)
		defer cancel()
		stored := false
		id, err := f(ctx, c, func(p mms.PDU) error {
			w.emit(wifiWorkerEvent{MMSResult: &mmsReply{ID: c.ID, Content: &p}})
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			select {
			case ok := <-ack:
				if ok {
					stored = true
					return nil
				}
			case <-ctx.Done():
			case <-timer.C:
			}
			return errors.New("MMS_STORAGE_UNCONFIRMED")
		})
		if stored {
			err = nil
		}
		// Retry only failures proven to precede HTTP submission.
		if c.Op == "mms-send" && (errors.Is(err, mms.ErrNetwork) || mmsBridgeCode(err) == "MMS_IWLAN_UNAVAILABLE") {
			w.mu.Lock()
			delete(w.used, c.ID)
			w.mu.Unlock()
		}
		w.emit(wifiWorkerEvent{MMSResult: &mmsReply{ID: c.ID, MessageID: id, Code: mmsBridgeCode(err)}})
	}()
	return true
}
