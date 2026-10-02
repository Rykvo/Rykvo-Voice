package hardware

import (
	"context"
	"errors"
	"testing"
	"time"

	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/vocat/vowifi"
)

type carrierReaderFake struct {
	device string
	err    error
}

func TestCarrierIdentityFailureRetriesWithoutLongNegativeCache(t *testing.T) {
	now := time.Now()
	failed := carrierCacheEntry{at: now}
	if !failed.fresh(now.Add(29*time.Second)) || failed.fresh(now.Add(30*time.Second)) {
		t.Fatal("temporary SIM read failure must retry after 30 seconds")
	}
	ready := carrierCacheEntry{at: now, value: &carrierconfig.Selection{}}
	if !ready.fresh(now.Add(14*time.Minute)) || ready.fresh(now.Add(15*time.Minute)) || ready.fresh(now.Add(-time.Second)) {
		t.Fatal("successful identity cache lifetime changed")
	}
}

func TestCarrierCacheIsBoundToCardDeviceAndGeneration(t *testing.T) {
	c := Candidate{Key: "fixture-a", Kind: "usb", Vendor: "2c7c", Product: "0125", Generation: "generation-a", Ports: []Port{{Interface: 3}}}
	r := Reading{SIM: "READY", ICCID: "8900000000000000001", IMEI: "123456789012345", Responsive: true}
	selection := carrierconfig.Match(carrierconfig.Identity{MCC: "310", MNC: "280", IMSI: "310280000000001", GID1: "20FF"})
	key := Digest(c.Identity(r) + "\x00" + r.ICCID + "\x00" + c.Generation)
	s := NewSystem()
	s.carriers.entries = map[string]carrierCacheEntry{key: {at: time.Now(), value: &selection}}
	// A short deadline prevents a hardware read on a cache miss.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := s.carrierConfiguration(ctx, c, r); got != &selection {
		t.Fatal("current SIM lost its cached configuration")
	}
	for _, change := range []func(*Candidate, *Reading){
		func(c *Candidate, _ *Reading) { c.Generation = "generation-b" },
		func(_ *Candidate, r *Reading) { r.ICCID = "8900000000000000002" },
		func(_ *Candidate, r *Reading) { r.IMEI = "123456789012346" },
		func(_ *Candidate, r *Reading) { r.SIM = "absent" },
		func(_ *Candidate, r *Reading) { r.Issue = "READ_TIMEOUT" },
	} {
		device, reading := c, r
		change(&device, &reading)
		if s.carrierConfiguration(ctx, device, reading) != nil {
			t.Fatal("cached carrier leaked across an identity or availability change")
		}
	}
	if s.carrierConfiguration(ctx, c, r) != &selection {
		t.Fatal("another device invalidated the original SIM cache")
	}
}

func (*carrierReaderFake) ReadIdentity(context.Context, string) (vowifi.SIMIdentity, error) {
	return vowifi.SIMIdentity{}, nil
}
func (r *carrierReaderFake) ReadSMSCenter(ctx context.Context, device string) (string, error) {
	r.device = device
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "+12025550101", r.err
}

func TestCarrierSIMPreservesSMSCenterReader(t *testing.T) {
	reader := &carrierReaderFake{}
	wrapped := carrierSIM{reader, func(string) {}}
	smsc, ok := any(wrapped).(vowifi.SMSCenterReader)
	if !ok {
		t.Fatal("carrier wrapper dropped the SIM SMS-centre reader")
	}
	value, err := smsc.ReadSMSCenter(context.Background(), "device-a")
	if err != nil || value != "+12025550101" || reader.device != "device-a" {
		t.Fatal("SMS centre was not read from the bound device")
	}
	reader.err = errors.New("read failed")
	if _, err = smsc.ReadSMSCenter(context.Background(), "device-a"); err != reader.err {
		t.Fatal("read error lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = smsc.ReadSMSCenter(ctx, "device-a"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
