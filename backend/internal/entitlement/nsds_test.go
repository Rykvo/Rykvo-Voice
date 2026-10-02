package entitlement

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"rykvo.local/auth/internal/carrierconfig"
	"strings"
	"testing"
)

type fakeAKA struct {
	verified bool
	rejected bool
}

func (a *fakeAKA) Respond(context.Context, []byte) ([]byte, error) {
	if a.rejected {
		return nil, errors.New("private-auth-detail")
	}
	a.verified = true
	return []byte("fixture-response"), nil
}
func (a *fakeAKA) Verified() bool { return a.verified }

func TestNSDSCarrierSession(t *testing.T) {
	for _, mode := range []string{"success", "rejected", "unverified", "bad-page", "wrong-id", "duplicate", "auth-pair-rejected", "sim-rejected", "http-error", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			provider, _ := carrierconfig.EmergencyByID("att-mvno-us")
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.Header.Get("x-generic-protocol-version") != "1.0" || r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("protocol headers")
				}
				gz, err := gzip.NewReader(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer gz.Close()
				var in []request
				if json.NewDecoder(gz).Decode(&in) != nil {
					t.Error("invalid request")
				}
				if strings.Contains(r.URL.String(), "fixture-secret") {
					t.Error("URL credential leak")
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "http-error" {
					w.WriteHeader(503)
					return
				}
				if mode == "oversize" {
					w.Write([]byte(strings.Repeat("x", 256*1024+1)))
					return
				}
				auth := response{ID: 1, Code: 1000}
				if mode == "unverified" {
					json.NewEncoder(w).Encode([]response{auth})
					return
				}
				if calls == 1 {
					auth.Code = 1003
					auth.Challenge = "Zml4dHVyZQ=="
					if mode == "rejected" {
						auth.Code = 1006
					}
					json.NewEncoder(w).Encode([]response{auth})
					return
				}
				if calls == 2 {
					if in[0]["aka-challenge-rsp"] == nil {
						t.Error("missing challenge reply")
					}
					auth.Token = "fixture-secret"
					json.NewEncoder(w).Encode([]response{auth})
					return
				}
				if in[0]["aka-token"] != "fixture-secret" || in[0]["aka-challenge-rsp"] != nil {
					t.Error("wrong token flow")
				}
				if mode == "auth-pair-rejected" {
					auth.Code = 1006
				}
				if calls == 3 {
					if len(in) != 2 || in[1]["method"] != "getMSISDN" {
						t.Error("wrong identity request")
					}
					line := response{ID: 2, Code: 1000, Fingerprint: "fixture-line"}
					if mode == "wrong-id" {
						line.ID = 9
					}
					if mode == "duplicate" {
						line.ID = 1
					}
					json.NewEncoder(w).Encode([]response{line, auth})
					return
				}
				if len(in) != 2 || in[1]["method"] != "manageLocationAndTC" || in[1]["service-fingerprint"] != "fixture-line" {
					t.Error("wrong address lookup")
				}
				pageURL := "https://attdashboard.wireless.att.com/softphone/primary/reseller/fixture"
				if mode == "bad-page" {
					pageURL = "https://attdashboard.wireless.att.com.evil.example/softphone/primary/"
				}
				w.Header().Set("Content-Encoding", "gzip")
				out := gzip.NewWriter(w)
				defer out.Close()
				json.NewEncoder(out).Encode([]response{auth, {ID: 3, Code: 1000, URL: pageURL, Data: "fixture-page-token"}})
			}))
			defer ts.Close()
			provider.Endpoint = ts.URL
			aka := &fakeAKA{rejected: mode == "sim-rejected"}
			page, err := discover(context.Background(), ts.Client(), provider, "0310280000000000@nai.epc.mnc280.mcc310.3gppnetwork.org", "123456789012345", aka)
			if mode == "success" {
				if err != nil || page.Token != "fixture-page-token" || calls != 4 {
					t.Fatal(page, err, calls)
				}
			} else {
				if err == nil || page.Token != "" || strings.Contains(err.Error(), "private-") {
					t.Fatal("unsafe failure", err)
				}
			}
			if calls > 4 {
				t.Fatal("unbounded retries")
			}
		})
	}
}
func TestEmergencyPageURLBoundaries(t *testing.T) {
	p, _ := carrierconfig.EmergencyByID("att-mvno-us")
	for _, raw := range []string{"http://attdashboard.wireless.att.com/softphone/primary/", "https://attdashboard.wireless.att.com.evil.example/softphone/primary/", "https://evil@attdashboard.wireless.att.com/softphone/primary/", "https://attdashboard.wireless.att.com:443/softphone/primary/", "https://attdashboard.wireless.att.com/softphone/primary/../other", "https://attdashboard.wireless.att.com/softphone/primary/%2e%2e/other", "https://attdashboard.wireless.att.com/softphone/primary/?token=secret", "javascript:alert(1)"} {
		if (Page{URL: raw, Token: "fixture"}).Valid(p) {
			t.Fatal(raw)
		}
	}
	if !(Page{URL: "https://attdashboard.wireless.att.com/softphone/primary/reseller/fixture", Token: "fixture"}).Valid(p) {
		t.Fatal("valid page rejected")
	}
}

func TestEmergencyNetworkBoundaries(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "192.168.1.1", "10.1.1.1", "169.254.1.1", "100.64.1.1", "198.18.0.1", "2001:db8::1", "::ffff:127.0.0.1", "192.0.2.1"} {
		if publicIP(net.ParseIP(s)) {
			t.Fatal("private/reserved accepted", s)
		}
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public rejected")
	}
	p, _ := carrierconfig.EmergencyByID("att-mvno-us")
	p.Endpoint = "https://untrusted.example/"
	if _, err := Discover(context.Background(), p, "fixture", "000000000000000", &fakeAKA{}); err != ErrInvalid {
		t.Fatal("unregistered provider accepted", err)
	}
}
