package ims

import (
	"strconv"
	"strings"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func callResponseFailure(response *sipResponse) vowifi.CallFailure {
	if response == nil || response.StatusCode < 300 {
		return ""
	}
	// RFC 3326 Q.850 causes distinguish remote release from SDP rejection.
	var result vowifi.CallFailure
	for _, value := range splitHeaderValues(response.values("Reason")) {
		protocol, params, ok := strings.Cut(value, ";")
		if !ok || !strings.EqualFold(strings.TrimSpace(protocol), "Q.850") {
			continue
		}
		cause, ok := reasonCause(params)
		if !ok {
			continue
		}
		var current vowifi.CallFailure
		switch cause {
		case 16, 31:
			current = "remote_cancelled"
		case 17:
			current = "busy"
		case 18, 19:
			current = "no_answer"
		case 21:
			current = "rejected"
		case 65, 88:
			current = "unsupported_audio"
		}
		if result != "" && current != result {
			return "" // Conflicting evidence is not a confirmed cause.
		}
		result = current
	}
	if result != "" {
		return result
	}
	for _, value := range splitHeaderValues(response.values("Warning")) {
		fields := strings.Fields(value)
		if len(fields) >= 3 && (fields[0] == "304" || fields[0] == "305") {
			return "unsupported_audio"
		}
	}
	return ""
}

func reasonCause(params string) (int, bool) {
	// Do not interpret a ;cause= fragment inside quoted free text.
	quoted, escaped, start, cause := false, false, 0, -1
	for i, c := range params + ";" {
		switch {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			key, value, ok := strings.Cut(params[start:i], "=")
			start = i + 1
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "cause") {
				continue
			}
			value = strings.TrimSpace(value)
			if cause >= 0 || value == "" || strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
				return 0, false
			}
			n, err := strconv.Atoi(value)
			if err != nil || n > 127 {
				return 0, false
			}
			cause = n
		}
	}
	return cause, !quoted && cause >= 0
}

func (session *Session) setCallFailure(id string, failure vowifi.CallFailure) {
	session.callMu.Lock()
	defer session.callMu.Unlock()
	if call := session.calls[id]; call != nil && call.public.EndedAt == nil {
		call.public.Failure = failure
	}
}
