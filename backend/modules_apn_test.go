package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rykvo.local/auth/internal/hardware"
)

type moduleAPNFake struct {
	calls   int
	applied *hardware.APNConfig
}

func (*moduleAPNFake) Discover(context.Context) ([]hardware.Candidate, error) { return nil, nil }
func (*moduleAPNFake) Read(context.Context, hardware.Candidate) hardware.Reading {
	return hardware.Reading{}
}
func (f *moduleAPNFake) APN(_ context.Context, _ hardware.Candidate, _, _ string, c *hardware.APNConfig) ([]hardware.APNContext, error) {
	f.calls++
	f.applied = c
	return []hardware.APNContext{{CID: 1, APN: "internet", Protocol: "IPV4V6"}}, nil
}
func TestAPNOperationsRespectDeviceGateAndCard(t *testing.T) {
	f := &moduleAPNFake{}
	m := newModuleManager(nil, f)
	m.ready = true
	m.lastScan = time.Now()
	sample := wifiModuleFixture()
	v := moduleRecord{ID: 1, Endpoint: sample.Candidate.Key}
	m.values[1] = sample
	m.seen[v.Endpoint] = sample.Candidate
	gate := m.gate(v.Endpoint)
	gate <- struct{}{}
	if _, e := m.accessAPN(context.Background(), v, sample.Reading.ICCID, nil); e == nil || e.Error() != "DEVICE_BUSY" {
		t.Fatal(e)
	}
	<-gate
	if _, e := m.accessAPN(context.Background(), v, "89000000000000000000", nil); e == nil {
		t.Fatal("wrong card accepted")
	}
	m.wifi[1] = &moduleWiFi{running: true}
	if _, e := m.accessAPN(context.Background(), v, sample.Reading.ICCID, nil); e == nil {
		t.Fatal("concurrent Wi-Fi accepted")
	}
	delete(m.wifi, 1)
	if _, e := m.accessAPN(context.Background(), v, sample.Reading.ICCID, nil); e != nil || f.calls != 1 {
		t.Fatal(e, f.calls)
	}
}

func testAPNDatabase(t *testing.T, s *server, v moduleRecord, cookie, csrf string) {
	t.Helper()
	f := &moduleAPNFake{}
	m := newModuleManager(s.db, f)
	m.ready = true
	m.lastScan = time.Now()
	sample := wifiModuleFixture()
	sample.Candidate.Key = v.Endpoint
	m.values[v.ID] = sample
	m.seen[v.Endpoint] = sample.Candidate
	old := s.modules
	s.modules = m
	defer func() { s.modules = old }()
	base := "/api/modules/" + moduleID(v.ID) + "/lines/" + wifiLine(sample.Reading) + "/apns"
	request := func(method, path, body, token string, want int) string {
		r := localRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("X-CSRF-Token", token)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	id := "apn-test-profile-01"
	body := `{"apn":"internet","protocol":"IPV4V6","auth":"PAP","username":"user","password":"test-secret"}`
	request("PUT", base+"/"+id, body, "", 403)
	request("PUT", base+"/"+id, body, csrf, 200)
	list := request("GET", base, "", csrf, 200)
	if strings.Contains(list, "test-secret") || !strings.Contains(list, `"hasPassword":true`) {
		t.Fatal("password leak or missing presence flag")
	}
	request("PUT", base+"/"+id, `{"apn":"internet","protocol":"IPV4V6","auth":"PAP","username":"user","preservePassword":true}`, csrf, 200)
	request("POST", base+"/"+id+"/apply", "", csrf, 200)
	if f.applied == nil || f.applied.Password != "test-secret" {
		t.Fatal("credential did not reach device adapter")
	}
	request("PUT", base+"/"+id, `{"apn":"ims","protocol":"IP","auth":"NONE"}`, csrf, 400)
	request("DELETE", base+"/"+id, "", csrf, 200)
	request("POST", base+"/"+id+"/apply", "", csrf, 404)
	var response struct {
		Data struct {
			Profiles []any `json:"profiles"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(request("GET", base, "", csrf, 200)), &response) != nil || len(response.Data.Profiles) != 0 {
		t.Fatal("profile removal failed")
	}
}
