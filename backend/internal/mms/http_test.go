package mms

import (
	"context"
	"errors"
	"net"
	"testing"

	"rykvo.local/auth/internal/carrierconfig"
)

func TestRetrievalGatewayBoundToCarrier(t *testing.T) {
	p := carrierconfig.Profile{MCC: "310", MMSC: "http://mms.msg.eng.t-mobile.com/mms/wapenc"}
	for _, v := range []struct {
		url   string
		valid bool
	}{
		{"http://mpc.t-mobile.com/message", true},
		{"http://mms.msg.eng.t-mobile.com/message", true},
		{"http://mpc.t-mobile.com.evil.example/message", false},
		{"http://mpc.t-mobile.com@127.0.0.1/message", false},
		{"http://127.0.0.1/message", false},
		{"http://10.234.48.185/message", true},
		{"http://169.254.169.254/message", false},
		{"http://10.234.48.185:22/message", false},
		{"http://mpc.t-mobile.com:22/message", false},
		{"http://other.t-mobile.com/message", false},
	} {
		_, err := ReceiveURL(p, v.url)
		if (err == nil) != v.valid {
			t.Fatal(v, err)
		}
	}
	p.MCC = "460"
	if _, err := ReceiveURL(p, "http://mpc.t-mobile.com/message"); err == nil {
		t.Fatal("cross-carrier alias")
	}
}
func TestBoundHTTPNeverFallsBackToHost(t *testing.T) {
	count := 0
	p := carrierconfig.Profile{MCC: "310", MMSC: "http://mms.msg.eng.t-mobile.com/mms/wapenc"}
	c, err := NewClient(p, func(_ context.Context, network, address string) (net.Conn, error) {
		count++
		if network != "tcp" || address != "mpc.t-mobile.com:80" {
			t.Fatal(network, address)
		}
		return nil, errors.New("bearer offline")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Retrieve(context.Background(), "http://mpc.t-mobile.com/test"); !errors.Is(err, ErrNetwork) || count != 1 {
		t.Fatal(err, count)
	}
	if _, err = c.Retrieve(context.Background(), "http://untrusted.example/"); !errors.Is(err, ErrNetwork) || count != 1 {
		t.Fatal("untrusted host reached", err, count)
	}
}

func TestPrivateDownloadNeverUsesUnboundNetwork(t *testing.T) {
	p := carrierconfig.Profile{MMSC: "http://mmsc.vnet.mobi", MMSProxy: "10.0.0.200", MMSPort: "80"}
	calls := 0
	c, err := NewClient(p, func(_ context.Context, network, address string) (net.Conn, error) {
		calls++
		if address != "10.0.0.200:80" {
			t.Fatal(address)
		}
		return nil, errors.New("SIM bearer offline")
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Retrieve(context.Background(), "http://10.234.48.185/fixture")
	if !errors.Is(err, ErrNetwork) || calls != 1 {
		t.Fatal(err, calls)
	}
}
