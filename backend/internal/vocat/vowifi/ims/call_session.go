package ims

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func headerHasToken(values []string, token string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// RFC 4028: the answer must choose a refresher, even when the offer omits it.
func sessionTimerAnswer(request *sipRequest) string {
	value := strings.TrimSpace(request.value("Session-Expires"))
	if value == "" {
		return ""
	}
	if headerParameter(value, "refresher") == "" {
		value += ";refresher=uas"
	}
	return value
}

func sessionTimerSchedule(header string, localUAC bool) (int, bool, time.Duration) {
	seconds, err := strconv.Atoi(strings.TrimSpace(strings.Split(header, ";")[0]))
	if err != nil || seconds < 90 || seconds > 86400 {
		return 0, false, 0
	}
	role := strings.ToLower(headerParameter(header, "refresher"))
	if role == "" {
		// Older peers omit the parameter in their answer.
		role = "uac"
	}
	if role != "uac" && role != "uas" {
		return 0, false, 0
	}
	refresh := (role == "uac") == localUAC
	interval := time.Duration(seconds) * time.Second
	if refresh {
		return seconds, true, interval / 2
	}
	return seconds, false, interval - min(32*time.Second, interval/3)
}

func (session *Session) startSessionTimer(call *imsCall, header string, localUAC bool) {
	if call == nil {
		return
	}
	seconds, refresh, delay := sessionTimerSchedule(header, localUAC)
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if call.terminated || call.public.EndedAt != nil {
		return
	}
	if call.sessionCancel != nil {
		call.sessionCancel()
		call.sessionCancel = nil
	}
	call.sessionGeneration++
	call.sessionExpires = seconds
	if seconds == 0 {
		return
	}
	ctx, cancel := context.WithCancel(session.refreshContext)
	call.sessionCancel = cancel
	go session.runSessionTimer(ctx, call, call.sessionGeneration, delay, refresh)
}

func (session *Session) runSessionTimer(ctx context.Context, call *imsCall, generation uint64, delay time.Duration, refresh bool) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return
	}
	if refresh {
		refreshContext, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := session.sendDialogRequest(refreshContext, call, "UPDATE")
		cancel()
		if err == nil || ctx.Err() != nil {
			// The response installs the next timer.
			return
		}
	}
	endContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A newer refresh invalidates this expiry; unconfirmed BYE keeps the call isolated.
	_ = session.hangupCall(endContext, call.callID, generation)
}
