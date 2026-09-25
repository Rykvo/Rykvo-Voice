package vowifi

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrISIMUnavailable = errors.New("vocat: ISIM identity application unavailable")

// ProvisionedIMSIdentity contains public subscription identifiers, not AKA keys.
// Keep it in session memory, never in public status, logs or cross-SIM caches.
type ProvisionedIMSIdentity struct {
	PrivateIdentity  string
	Domain           string
	PublicIdentities []string
}

func (id ProvisionedIMSIdentity) Validate() error {
	invalid := errors.New("vocat: invalid provisioned IMS identity set")
	if !validISIMDomain(id.Domain) || !validISIMNAI(id.PrivateIdentity) || len(id.PublicIdentities) == 0 || len(id.PublicIdentities) > 32 {
		return invalid
	}
	for _, uri := range id.PublicIdentities {
		lower := strings.ToLower(uri)
		if !strings.HasPrefix(lower, "sip:") && !strings.HasPrefix(lower, "sips:") {
			return invalid
		}
		if !validISIMNAI(uri[strings.IndexByte(uri, ':')+1:]) {
			return invalid
		}
	}
	return nil
}

func validISIMNAI(value string) bool {
	if len(value) > 1024 || strings.Count(value, "@") != 1 {
		return false
	}
	user, domain, _ := strings.Cut(value, "@")
	if user == "" || !validISIMDomain(domain) {
		return false
	}
	for _, c := range user {
		if c <= 0x20 || c >= 0x7f || strings.ContainsRune("<>\"\\;:,?", c) {
			return false
		}
	}
	return true
}

func validISIMDomain(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// TS 31.103 4.2.2–4.2.4: the value is UTF-8 in tag 80; FF pads the EF.
func decodeISIMString(data []byte) (string, error) {
	return decodeISIMValue(data, false)
}

func decodeISIMValue(data []byte, allowEmpty bool) (string, error) {
	var result string
	found := false
	for len(data) > 0 {
		if data[0] == 0xff {
			if len(bytes.Trim(data, "\xff")) != 0 {
				return "", errors.New("vocat: invalid ISIM file padding")
			}
			break
		}
		tag, _, value, n, err := decodeBERTLV(data)
		if err != nil {
			return "", errors.New("vocat: malformed ISIM TLV")
		}
		if len(tag) == 1 && tag[0] == 0x80 {
			if found || (len(value) == 0 && !allowEmpty) || len(value) > 1024 || !utf8.Valid(value) {
				return "", errors.New("vocat: invalid ISIM value")
			}
			found, result = true, string(value)
		}
		data = data[n:]
	}
	if !found {
		return "", errors.New("vocat: missing ISIM value")
	}
	return result, nil
}
