package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSIPAccountValidation(t *testing.T) {
	network := sipAccountNetwork{Start: 20000, End: 30000}
	valid := sipAccountInput{Username: "1001", Password: "test-only-password", Port: 20001, Allocation: "fixed", ModuleIDs: []string{"module-02", "module-01", "module-02"}}
	ids, code := validateSIPAccount(&valid, false, network)
	if code != "" || len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatal(ids, code)
	}
	for _, change := range []func(*sipAccountInput){
		func(v *sipAccountInput) { v.Username = "x:y" },
		func(v *sipAccountInput) { v.Username = "<img>" },
		func(v *sipAccountInput) { v.Password = "" },
		func(v *sipAccountInput) { v.Password = "hello\r\nworld" },
		func(v *sipAccountInput) { v.Port = 19999 },
		func(v *sipAccountInput) { v.Port = 30001 },
		func(v *sipAccountInput) { v.Allocation = "random" },
		func(v *sipAccountInput) { v.ModuleIDs = nil },
		func(v *sipAccountInput) { v.ModuleIDs = []string{"module-1"} },
		func(v *sipAccountInput) { v.ModuleIDs = []string{"01"} },
	} {
		v := valid
		change(&v)
		if _, code := validateSIPAccount(&v, false, network); code == "" {
			t.Fatal(v)
		}
	}
	valid.Password = ""
	valid.Revision = 1
	if _, code := validateSIPAccount(&valid, true, network); code != "" {
		t.Fatal(code)
	}
	valid.Allocation = "all"
	if ids, code := validateSIPAccount(&valid, true, network); code != "" || len(ids) != 0 {
		t.Fatal(ids, code)
	}
}

func TestSIPDigestsNeverStorePlaintext(t *testing.T) {
	a, b := sipDigests("1001", "test-secret")
	if len(a) != 16 || len(b) != 32 || bytes.Contains(a, []byte("test-secret")) || bytes.Contains(b, []byte("test-secret")) {
		t.Fatal("invalid digest")
	}
	x, y := sipDigests("1002", "test-secret")
	if bytes.Equal(a, x) || bytes.Equal(b, y) {
		t.Fatal("digest not bound to username")
	}
}

func testSIPAccountsDatabase(t *testing.T, s *server, cookie, csrf string) {
	t.Helper()
	ctx := context.Background()
	oldNetwork := s.sipNetwork
	s.sipNetwork = &sipNetworkManager{call: func(context.Context, map[string]string) (sipNetworkStatus, error) {
		return sipNetworkStatus{State: "connected", Network: &sipNetworkBinding{Server: "sip.example.test", Start: 20000, End: 30000}}, nil
	}}
	defer func() { s.sipNetwork = oldNetwork }()
	request := func(server *server, method, path string, body any, auth, csrfOK bool) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		r := localRequest(method, path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", s.origin)
		if auth {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		if csrfOK {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	check := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		w := request(s, method, path, body, true, true)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var response map[string]any
		if w.Body.Len() > 0 {
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return response
	}
	const base = "/api/sip/accounts"
	input := map[string]any{"username": "sip-test-1001", "password": "test-only-secret-01", "port": 20001, "allocation": "fixed", "moduleIds": []string{"module-01", "module-02", "module-01"}, "receiveCalls": true}
	if w := request(s, "POST", base, input, false, true); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(s, "POST", base, input, true, false); w.Code != 403 {
		t.Fatal(w.Code)
	}
	input["port"] = 5060
	check("POST", base, input, 400)
	input["port"] = 20001
	result := check("POST", base, input, 201)["data"].(map[string]any)
	id := result["id"].(string)
	check("POST", base, input, 409)
	data := check("GET", base, nil, 200)["data"].(map[string]any)
	if data["callsReady"] != false || data["network"].(map[string]any)["start"] != float64(20000) {
		t.Fatal(data)
	}
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatal(items)
	}
	account := items[0].(map[string]any)
	if account["status"] != "offline" || account["ip"] != "sip.example.test" || len(account["moduleIds"].([]any)) != 2 {
		t.Fatal(account)
	}
	var storedMD5, storedSHA []byte
	var version int64
	if err := s.db.QueryRow(ctx, `SELECT digest_md5,digest_sha256,credential_revision FROM sip_accounts WHERE id=$1`, id).Scan(&storedMD5, &storedSHA, &version); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"test-only-secret-01", "digest_md5", "digest_sha256", hex.EncodeToString(storedMD5), hex.EncodeToString(storedSHA)} {
		if strings.Contains(request(s, "GET", base, nil, true, true).Body.String(), secret) {
			t.Fatal("credential exposed")
		}
	}
	input["revision"] = 1
	input["password"] = ""
	input["receiveCalls"] = false
	check("PATCH", base+"/"+id, input, 200)
	var hash []byte
	if err := s.db.QueryRow(ctx, `SELECT digest_md5,credential_revision FROM sip_accounts WHERE id=$1`, id).Scan(&hash, &version); err != nil || version != 1 || !bytes.Equal(hash, storedMD5) {
		t.Fatal("unchanged password lost", err, version)
	}
	check("PATCH", base+"/"+id, input, 409)
	input["revision"] = 2
	input["username"] = "sip-test-renamed"
	check("PATCH", base+"/"+id, input, 400)
	input["password"] = "test-only-secret-02"
	check("PATCH", base+"/"+id, input, 200)
	if err := s.db.QueryRow(ctx, `SELECT digest_md5,credential_revision FROM sip_accounts WHERE id=$1`, id).Scan(&hash, &version); err != nil || version != 2 || bytes.Equal(hash, storedMD5) {
		t.Fatal("credentials not rotated", err, version)
	}
	// A fresh server instance reads durable accounts, not an in-memory preview.
	restarted := &server{db: s.db, origin: s.origin, sipNetwork: s.sipNetwork}
	if w := request(restarted, "GET", base+"/"+id, nil, true, true); w.Code != 200 || !strings.Contains(w.Body.String(), "sip-test-renamed") {
		t.Fatal(w.Code, w.Body.String())
	}
	input["revision"] = 3
	input["password"] = ""
	input["allocation"] = "all"
	input["moduleIds"] = []string{}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- request(s, "PATCH", base+"/"+id, input, true, true).Code }()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for code := range results {
		if code == 200 {
			wins++
		} else if code == 409 {
			conflicts++
		} else {
			t.Fatal(code)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	check("DELETE", base+"/"+id, map[string]any{"revision": 3}, 409)
	check("DELETE", base+"/"+id, map[string]any{"revision": 4}, 204)
	check("GET", base+"/"+id, nil, 404)
	var associated int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM sip_account_modules WHERE account_id=$1`, id).Scan(&associated); err != nil || associated != 0 {
		t.Fatal("orphan module assignment", err)
	}
}

func TestSIPAccountNetworkSelection(t *testing.T) {
	binding := &sipNetworkBinding{Server: "sip.example.test", Start: 20000, End: 30000}
	for _, tc := range []struct {
		name     string
		status   sipNetworkStatus
		failure  bool
		wantMode string
	}{
		{name: "not configured", status: sipNetworkStatus{State: "disconnected"}, wantMode: "lan"},
		{name: "logged out with cached range", status: sipNetworkStatus{State: "disconnected", Network: binding}, wantMode: "lan"},
		{name: "connected", status: sipNetworkStatus{State: "connected", Enabled: true, Network: binding}, wantMode: "cloud"},
		{name: "connecting", status: sipNetworkStatus{State: "connecting", Enabled: true, Network: binding}},
		{name: "failed tunnel", status: sipNetworkStatus{State: "failed", Enabled: true, Network: binding}},
		{name: "configuring", status: sipNetworkStatus{State: "configuring"}},
		{name: "missing binding", status: sipNetworkStatus{State: "connected", Enabled: true}},
		{name: "helper unavailable", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{sipNetwork: &sipNetworkManager{call: func(context.Context, map[string]string) (sipNetworkStatus, error) {
				if tc.failure {
					return sipNetworkStatus{}, errors.New("unavailable")
				}
				return tc.status, nil
			}}}
			r := httptest.NewRequest("GET", "http://192.168.8.130/api/sip/accounts", nil)
			network, err := s.sipAccountNetwork(context.Background(), r)
			if tc.wantMode == "" {
				if err == nil {
					t.Fatal("unknown network accepted")
				}
				return
			}
			if err != nil || network.Mode != tc.wantMode {
				t.Fatal(network, err)
			}
			if tc.wantMode == "lan" {
				if network.Host != "192.168.8.130" || network.Start != 1024 || network.End != 65535 {
					t.Fatal(network)
				}
				for _, port := range []int{5060, 40000, 65535} {
					input := sipAccountInput{Username: "1001", Password: "test-only", Port: port, Allocation: "all"}
					if _, code := validateSIPAccount(&input, false, network); code != "" {
						t.Fatal(port, code)
					}
				}
			} else if network.Host != "sip.example.test" || network.Start != 20000 || network.End != 30000 {
				t.Fatal(network)
			}
		})
	}
}
