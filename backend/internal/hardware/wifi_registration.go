package hardware

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

type wifiRegistrarDelay struct{ until time.Time }

func (e *wifiRegistrarDelay) Error() string { return "WIFI_IMS_SERVICE_UNAVAILABLE" }

func wifiRetryAfter(values []string) time.Duration {
	if len(values) == 1 {
		fields := strings.FieldsFunc(values[0], func(r rune) bool { return r == ';' || r == '(' || r == ' ' })
		if len(fields) > 0 {
			if n, e := strconv.Atoi(fields[0]); e == nil && n >= 0 && n <= 86400 {
				if n < 5 {
					n = 5
				}
				return time.Duration(n) * time.Second
			}
		}
	}
	return 30 * time.Second
}

func wifiTransportFallback(observed bool, err error) bool {
	if observed || err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	switch err.Error() {
	case "WIFI_IMS_TIMEOUT", "WIFI_TCP_CONNECT_FAILED", "WIFI_TCP_CLOSED", "WIFI_TCP_WRITE_FAILED", "WIFI_IMS_ADDRESS_MISSING":
		return true
	}
	return false
}
func registerWiFiIMS(ctx context.Context, c *wifiChild, sim *wifiSIM, emit func(string)) (*wifiIMS, error) {
	transport := sim.profile.Transport
	if sim.preferredTransport != "" {
		transport = sim.preferredTransport
	}
	if transport == "" {
		transport = "tcp"
	}
	other := "udp"
	if transport == "udp" {
		other = "tcp"
	}
	var last error
	for _, peer := range c.pcscf {
		for _, mode := range []string{transport, other} {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s, e := newWiFiIMSAt(c, sim, mode, []net.IP{peer})
			if e != nil {
				last = e
				continue
			}
			emit("transport:" + mode)
			call, cancel := context.WithTimeout(ctx, 45*time.Second)
			_, e = s.register(call)
			cancel()
			if e == nil {
				sim.preferredTransport = mode
				return s, nil
			}
			emit("attempt:" + WiFiIssue(e))
			observed := s.sipObserved
			_ = s.close()
			last = e
			if !wifiTransportFallback(observed, e) {
				return nil, e
			}
		}
	}
	if last == nil {
		last = errors.New("WIFI_IMS_ADDRESS_MISSING")
	}
	return nil, last
}

// Only machine codes leave hardware boundaries; never log SIP/AKA material.
func wifiDetailedIssue(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.HasPrefix(s, "WIFI_IMS_DEREGISTER_UNCONFIRMED_STATUS_") {
		return "WIFI_IMS_DEREGISTER_UNCONFIRMED"
	}
	if strings.HasPrefix(s, "WIFI_IMS_PROTECTED:") {
		return "WIFI_IMS_PROTECTED_FAILED"
	}
	for _, v := range []string{"WIFI_IMS_CONTACT_UNCONFIRMED", "WIFI_IMS_SERVICE_UNAVAILABLE", "WIFI_IMS_TIMEOUT", "WIFI_TCP_CONNECT_FAILED", "WIFI_TCP_CLOSED", "WIFI_TCP_WRITE_FAILED", "WIFI_CARRIER_CONFIG_INVALID", "WIFI_CARRIER_UNSUPPORTED", "WIFI_IMS_PEER_AUTH_FAILED", "WIFI_IMS_ADDRESS_MISSING"} {
		if s == v {
			return v
		}
	}
	return ""
}
