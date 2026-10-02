package vowifi

import (
	"net/url"
	"strings"
	"unicode"
)

// ExtractAssociatedMSISDN accepts only identities explicitly associated by
// IMS. It never examines SIMIdentity.IMSI and deliberately rejects a bare SIP
// user that could be an IMSI.
func ExtractAssociatedMSISDN(evidence IMSEvidence) (number string, source string, ok bool) {
	if number, ok := normalizeAssociatedIdentity(evidence.AssociatedMSISDN, true); ok {
		return number, PhoneSourceAssociatedMSISDN, true
	}
	for _, associatedURI := range evidence.PAssociatedURI {
		if number, ok := normalizeAssociatedIdentity(associatedURI, false); ok {
			return number, PhoneSourcePAssociatedURI, true
		}
	}
	return "", "", false
}

func normalizeAssociatedIdentity(raw string, explicit bool) (string, bool) {
	value := strings.TrimSpace(raw)
	value = strings.Trim(value, "<>\"'")
	if value == "" {
		return "", false
	}

	lower := strings.ToLower(value)
	typedURI := false
	switch {
	case strings.HasPrefix(lower, "tel:"):
		typedURI = true
		value = value[len("tel:"):]
	case strings.HasPrefix(lower, "sip:"):
		typedURI = true
		value = value[len("sip:"):]
		if at := strings.IndexByte(value, '@'); at >= 0 {
			value = value[:at]
		}
		// A bare, non-E.164 SIP user is commonly an IMSI-derived IMPU.
		if !strings.HasPrefix(strings.TrimSpace(value), "+") {
			return "", false
		}
	case strings.HasPrefix(lower, "sips:"):
		typedURI = true
		value = value[len("sips:"):]
		if at := strings.IndexByte(value, '@'); at >= 0 {
			value = value[:at]
		}
		if !strings.HasPrefix(strings.TrimSpace(value), "+") {
			return "", false
		}
	default:
		if at := strings.IndexByte(value, '@'); at >= 0 {
			value = value[:at]
		} else if !explicit {
			// P-Associated-URI entries must be typed URIs; accepting arbitrary
			// digit strings here risks treating an IMSI as an MSISDN.
			return "", false
		}
	}

	if semicolon := strings.IndexByte(value, ';'); semicolon >= 0 {
		if !typedURI {
			return "", false
		}
		value = value[:semicolon]
	}
	if question := strings.IndexByte(value, '?'); question >= 0 {
		if !typedURI {
			return "", false
		}
		value = value[:question]
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}

	var normalized strings.Builder
	for _, character := range strings.TrimSpace(value) {
		switch {
		case character == '+' && normalized.Len() == 0:
			normalized.WriteRune(character)
		case character >= '0' && character <= '9':
			normalized.WriteRune(character)
		case unicode.IsSpace(character), character == '-', character == '(', character == ')':
			// Human formatting is harmless once the identity source is trusted.
		default:
			return "", false
		}
	}

	number := normalized.String()
	if !strings.HasPrefix(number, "+") {
		return "", false
	}
	digits := strings.TrimPrefix(number, "+")
	if !isNDigits(digits, 5, 15) {
		return "", false
	}
	return number, true
}
