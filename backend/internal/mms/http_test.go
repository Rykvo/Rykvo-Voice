package mms

import (
	"context"
	"errors"
	"net"
	"testing"

	"rykvo.local/auth/internal/carrierconfig"
)

func TestNotificationGatewayIsNotCarrierNameAllowlist(t *testing.T) {
	p := carrierconfig.Profile{MMSC: "http://submit.example:10021/mmsc"}
	for _, target := range []string{
		"http://download.example/message", "https://media.example:8514/message",
		"http://198.51.100.10:8088/message", "http://10.234.48.185/message", "http://[2001:db8::1]/message",
	} {
		if _, err := ReceiveURL(p, target); err != nil {
			t.Fatal(target, err)
		}
	}
	for _, target := range []string{
		"http://127.0.0.1/", "http://[::1]/", "http://0.0.0.0/", "http://[::]/",
		"http://169.254.169.254/", "http://[fe80::1]/", "http://224.0.0.1/", "http://[ff02::1]/",
		"http://localhost/", "http://LOCALHOST./", "http://device.local/", "http://router.home.arpa/",
		"http://user@download.example/", "http://2130706433/", "http://127.1/", "http://0x7f000001/",
		"http://download.example:0/", "http://download.example:65536/", "http://download.example:bad/",
		"file:///tmp/message", "http://download.example/\r\nx", "http://download.example/#fragment",
	} {
		if _, err := ReceiveURL(p, target); err == nil {
			t.Fatal("unsafe URL accepted", target)
		}
	}
}

func TestCarrierPortsAndProxyAreNotHardcoded(t *testing.T) {
	for _, tc := range []struct{ mmsc, proxy, port, target, address string }{
		{"http://mms.um.three.com.hk:10021/mmsc", "mms.three.com.hk", "8799", "http://media.example:10021/item", "mms.three.com.hk:8799"},
		{"http://mmsc.example:8514", "10.0.0.172", "8081", "http://198.51.100.10/item", "10.0.0.172:8081"},
		{"http://submit.example", "", "", "http://media.example:6672/item", "media.example:6672"},
		{"http://submit.example", "", "", "https://media.example/item", "media.example:443"},
	} {
		p := carrierconfig.Profile{MMSC: tc.mmsc, MMSProxy: tc.proxy, MMSPort: tc.port}
		calls := 0
		c, err := NewClient(p, func(_ context.Context, network, address string) (net.Conn, error) {
			calls++
			if network != "tcp" || address != tc.address {
				t.Fatal(network, address, tc)
			}
			return nil, errors.New("SIM bearer offline")
		})
		if err != nil {
			t.Fatal(tc, err)
		}
		if _, err = c.Retrieve(context.Background(), tc.target); !errors.Is(err, ErrNetwork) || calls != 1 {
			t.Fatal(tc, err, calls)
		}
		if _, err = NewClient(p, nil); err == nil {
			t.Fatal("unbound client allowed")
		}
	}
}

func TestChinaCarriersRetrieveThroughSelectedProxy(t *testing.T) {
	for _, mnc := range []string{"00", "01", "11"} {
		t.Run(mnc, func(t *testing.T) {
			s := carrierconfig.Match(carrierconfig.Identity{MCC: "460", MNC: mnc, IMSI: "460" + mnc + "0000000001"})
			if s.MMS.Status != "matched" || s.MMS.Profile == nil {
				t.Fatal(s)
			}
			p, calls := *s.MMS.Profile, 0
			c, err := NewClient(p, func(_ context.Context, _, address string) (net.Conn, error) {
				calls++
				if address != net.JoinHostPort(p.MMSProxy, p.MMSPort) {
					t.Fatal("wrong SIM proxy", address)
				}
				return nil, errors.New("SIM bearer offline")
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"http://198.51.100.10/fixture", "http://10.23.48.18/fixture"} {
				if _, err := c.Retrieve(context.Background(), target); !errors.Is(err, ErrNetwork) {
					t.Fatal(err)
				}
			}
			if calls != 2 {
				t.Fatal("notification IP never reached carrier proxy", calls)
			}
		})
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
	if _, err = c.Retrieve(context.Background(), "http://127.0.0.1/"); !errors.Is(err, ErrNetwork) || count != 1 {
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
