package ims

import (
	"net/url"
	"strings"
)

// Only for requests received through the registered operator IMS session.
// Never use these identities to authorize APP requests or choose a module.
func incomingCallerNumber(request *sipRequest) string {
	for _, value := range request.values("Privacy") {
		for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return r == ';' || r == ',' || r == ' ' || r == '\t'
		}) {
			if token == "id" || token == "user" {
				return ""
			}
		}
	}
	for _, value := range splitHeaderValues(request.values("P-Asserted-Identity")) {
		if number := callerIdentityNumber(value); number != "" {
			return number
		}
	}
	return callerIdentityNumber(request.value("From"))
}

func callerIdentityNumber(value string) string {
	scheme, rest, ok := strings.Cut(publicIdentityURI(value), ":")
	if !ok {
		return ""
	}
	switch strings.ToLower(scheme) {
	case "sip", "sips":
		user, host, ok := strings.Cut(rest, "@")
		if !ok || host == "" || strings.Contains(host, "@") {
			return ""
		}
		rest = user
	case "tel":
	default:
		return ""
	}
	// Strip URI parameters, not telephone digits; never guess a country code.
	number, _, _ := strings.Cut(rest, ";")
	number, err := url.PathUnescape(number)
	if err != nil {
		return ""
	}
	number = strings.Map(func(r rune) rune {
		if r == '-' || r == '.' || r == '(' || r == ')' {
			return -1
		}
		return r
	}, number)
	if !validCallNumber(number) || !strings.ContainsAny(number, "0123456789") {
		return ""
	}
	return number
}
