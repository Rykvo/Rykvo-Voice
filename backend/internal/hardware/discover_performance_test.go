package hardware

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Explicit opt-in: sysfs reads only, no AT, PC/SC, SIM or network commands.
func BenchmarkLiveDiscovery(b *testing.B) {
	if os.Getenv("RYKVO_DISCOVERY_BENCH") != "1" {
		b.Skip("read-only host diagnostic")
	}
	s := NewSystem()
	s.Readers = func(context.Context) ([]string, error) { return nil, nil }
	var result []Candidate
	for b.Loop() {
		var err error
		result, err = s.Discover(context.Background())
		if err != nil {
			b.Fatal(err)
		}
	}
	body, _ := json.Marshal(result)
	b.Logf("candidates=%d fingerprint=%s", len(result), Digest(string(body)))
}

func TestDiscoverySkipsNonPortSubtrees(t *testing.T) {
	s := fixture(t)
	usb(t, s, "1-1", "fixture", 0, 0)
	p := filepath.Join(s.Sys, "bus/usb/devices/1-1:1.2")
	put(t, filepath.Join(p, "usbmisc/cdc-wdm0/uevent"), "")
	put(t, filepath.Join(p, "net/wwan0/statistics/rx_bytes"), "0")
	put(t, filepath.Join(p, "power/deep/ttyUSB999/uevent"), "")
	put(t, filepath.Join(p, "net/wwan0/queues/rx-0/deep/ttyUSB998/uevent"), "")
	items, err := s.Discover(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatal(len(items), err)
	}
	if len(items[0].Ports) != 2 || items[0].Network != "wwan0" || filepath.Base(items[0].Control) != "cdc-wdm0" {
		t.Fatal("port grouping changed", items[0])
	}
}
