package ims

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SIP 2xx only accepts transport. RP-ACK/RP-ERROR answers the SC submission;
// only a separate TP-STATUS-REPORT proves recipient delivery.
type rpSubmitResult struct {
	accepted bool
	cause    *int
}
type rpPending struct {
	callID string
	result chan rpSubmitResult
}

func (s *Session) beginRP() (byte, *rpPending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingRP == nil {
		s.pendingRP = make(map[byte]*rpPending)
	}
	now := time.Now()
	for i := 0; i < 256; i++ {
		ref := s.nextRPReference
		s.nextRPReference++
		if s.pendingRP[ref] != nil || now.Before(s.rpReuseAfter[ref]) {
			continue
		}
		pending := &rpPending{result: make(chan rpSubmitResult, 1)}
		s.pendingRP[ref] = pending
		s.rpReuseAfter[ref] = now.Add(5 * time.Minute)
		return ref, pending, nil
	}
	return 0, nil, errors.New("SMS_REFERENCE_BUSY")
}
func (s *Session) endRP(ref byte) {
	s.mu.Lock()
	delete(s.pendingRP, ref)
	s.mu.Unlock()
}
func (s *Session) awaitRP(ctx context.Context, pending *rpPending) (rpSubmitResult, error) {
	timer := time.NewTimer(40 * time.Second)
	defer timer.Stop()
	var closed <-chan struct{}
	if s.refreshContext != nil {
		closed = s.refreshContext.Done()
	}
	select {
	case r := <-pending.result:
		if r.cause != nil {
			return r, fmt.Errorf("%w: RP cause %d", ErrSMSRejected, *r.cause)
		}
		return r, nil
	case <-ctx.Done():
		return rpSubmitResult{}, ctx.Err()
	case <-closed:
		return rpSubmitResult{}, errors.New("SMS_SESSION_CLOSED")
	case <-timer.C:
		return rpSubmitResult{}, errors.New("SMS_RP_TIMEOUT")
	}
}
func parseRPResult(data []byte) (rpSubmitResult, error) {
	fail := errors.New("SMS_INVALID_RP_RESULT")
	if len(data) < 2 || (data[0] != 3 && data[0] != 5) {
		return rpSubmitResult{}, fail
	}
	r := rpSubmitResult{accepted: data[0] == 3}
	offset := 2
	if data[0] == 5 {
		if len(data) < 4 || data[2] < 1 || int(data[2]) > len(data)-3 {
			return rpSubmitResult{}, fail
		}
		cause := int(data[3] & 0x7f)
		r.cause = &cause
		offset = 3 + int(data[2])
	}
	// Optional RP user data contains SMS-SUBMIT-REPORT.
	if offset < len(data) && (len(data)-offset < 2 || data[offset] != 0x41 || int(data[offset+1]) != len(data)-offset-2) {
		return rpSubmitResult{}, fail
	}
	return r, nil
}
func (s *Session) receiveRPResult(request *sipRequest, data []byte) {
	r, err := parseRPResult(data)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pendingRP[data[1]]
	if pending == nil {
		return
	}
	reply := strings.TrimSpace(request.value("In-Reply-To"))
	if reply != "" && reply != pending.callID {
		return
	}
	select {
	case pending.result <- r:
	default:
	}
}
