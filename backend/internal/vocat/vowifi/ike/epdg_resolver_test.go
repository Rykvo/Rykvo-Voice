package ike

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveEPDGWithECS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("name"); got != "epdg.epc.mnc002.mcc262.pub.3gppnetwork.org" {
			t.Errorf("name = %q", got)
		}
		if got := r.URL.Query().Get("edns_client_subnet"); got != "109.192.0.0/24" {
			t.Errorf("edns_client_subnet = %q", got)
		}
		w.Header().Set("Content-Type", "application/dns-json")
		_, _ = fmt.Fprint(w, `{
          "Status": 0,
          "Question": [{"name":"epdg.example.","type":1}],
          "Answer": [
            {"name":"epdg.example.","type":5,"TTL":60,"data":"gateway.example."},
            {"name":"gateway.example.","type":1,"TTL":60,"data":"139.7.117.168"},
            {"name":"gateway.example.","type":1,"TTL":60,"data":"139.7.117.169"},
            {"name":"gateway.example.","type":1,"TTL":60,"data":"139.7.117.168"}
          ]
        }`)
	}))
	defer server.Close()

	addresses, err := resolveEPDGWithECS(
		context.Background(),
		server.Client(),
		server.URL,
		"epdg.epc.mnc002.mcc262.pub.3gppnetwork.org",
		"109.192.0.0/24",
	)
	if err != nil {
		t.Fatalf("resolveEPDGWithECS: %v", err)
	}
	if len(addresses) != 2 || addresses[0].IP.String() != "139.7.117.168" || addresses[1].IP.String() != "139.7.117.169" {
		t.Fatalf("addresses = %#v", addresses)
	}
}

func TestResolveEPDGWithECSRejectsEmptyAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"Status":0,"Answer":[{"type":5,"data":"gateway.example."}]}`)
	}))
	defer server.Close()
	if _, err := resolveEPDGWithECS(context.Background(), server.Client(), server.URL, "epdg.example", "192.0.2.0/24"); err == nil {
		t.Fatal("empty address answer was accepted")
	}
}
