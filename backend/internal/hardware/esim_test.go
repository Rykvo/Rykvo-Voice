package hardware

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/damonto/euicc-go/bertlv"
	"github.com/damonto/euicc-go/lpa"
	sgp22 "github.com/damonto/euicc-go/v2"
)

const testEID = "890490320010012345000123456789"
const testCardEID = "89049032001001234500012345678901"
const testICCID = "89123456789012345678"

type fakeESIM struct {
	eid        string
	profiles   []*sgp22.ProfileInfo
	writes     int
	fail       bool
	notifyFail bool
	removed    int
	changed    bool
}

func (f *fakeESIM) EID() ([]byte, error) { return hex.DecodeString(f.eid) }
func (f *fakeESIM) ListProfile(any, []bertlv.Tag) ([]*sgp22.ProfileInfo, error) {
	return f.profiles, nil
}
func (f *fakeESIM) EUICCInfo2() (*bertlv.TLV, error) { return nil, errors.New("unsupported") }
func (f *fakeESIM) ListNotification(...sgp22.NotificationEvent) ([]*sgp22.NotificationMetadata, error) {
	return []*sgp22.NotificationMetadata{}, nil
}
func (f *fakeESIM) EnableProfile(any, bool) error {
	f.writes++
	if f.fail {
		return errors.New("NO_EUICC")
	}
	f.profiles[0].ProfileState = sgp22.ProfileEnabled
	return nil
}
func (f *fakeESIM) DisableProfile(any, bool) error {
	f.writes++
	f.profiles[0].ProfileState = sgp22.ProfileDisabled
	return nil
}
func (f *fakeESIM) DeleteProfile(any) error { f.writes++; f.profiles = nil; return nil }
func (f *fakeESIM) SetNickname(_ sgp22.ICCID, label string) error {
	f.writes++
	if !f.fail {
		f.profiles[0].ProfileNickname = label
	}
	if f.changed {
		f.eid = "89049032001001234500012345678999"
	}
	return nil
}
func (f *fakeESIM) DownloadProfile(_ context.Context, _ *lpa.ActivationCode, options *lpa.DownloadOptions) (*sgp22.LoadBoundProfilePackageResponse, error) {
	f.writes++
	p := testProfile()
	if !options.OnConfirm(p) {
		return nil, errors.New("download declined")
	}
	options.OnProgress(lpa.DownloadStageInstall)
	if f.fail {
		return nil, errors.New("carrier rejected activation")
	}
	f.profiles = append(f.profiles, p)
	return &sgp22.LoadBoundProfilePackageResponse{}, nil
}
func (f *fakeESIM) RetrieveNotificationList(any) ([]*sgp22.PendingNotification, error) {
	return []*sgp22.PendingNotification{{Notification: &sgp22.NotificationMetadata{SequenceNumber: 1}}}, nil
}
func (f *fakeESIM) HandleNotification(*sgp22.PendingNotification) error {
	if f.notifyFail {
		return errors.New("network")
	}
	return nil
}
func (f *fakeESIM) RemoveNotificationFromList(sgp22.SequenceNumber) error { f.removed++; return nil }
func testProfile() *sgp22.ProfileInfo {
	id, _ := sgp22.NewICCID(testICCID)
	return &sgp22.ProfileInfo{ICCID: id, ProfileNickname: "主号"}
}
func fixtureESIM() *fakeESIM {
	return &fakeESIM{eid: testCardEID, profiles: []*sgp22.ProfileInfo{testProfile()}}
}
func perform(f *fakeESIM, action string) ESIMResult {
	return executeESIM(context.Background(), f, ESIMRequest{Action: action, EID: testCardEID, ICCID: testICCID, Label: "工作卡"}, func(string) {})
}

func TestESIMReadAndWriteVerification(t *testing.T) {
	for _, action := range []string{"read", "enable", "disable", "delete", "rename"} {
		t.Run(action, func(t *testing.T) {
			f := fixtureESIM()
			if action == "disable" {
				f.profiles[0].ProfileState = sgp22.ProfileEnabled
			}
			r := perform(f, action)
			if !r.Verified || r.Issue != "" {
				t.Fatalf("%+v", r)
			}
			if action == "read" && f.writes != 0 {
				t.Fatal("read mutated card")
			}
		})
	}
}
func TestESIMCardChangedBeforeOrAfterWrite(t *testing.T) {
	f := fixtureESIM()
	f.eid = "89049032001001234500012345678999"
	r := perform(f, "rename")
	if r.Issue != "DEVICE_CHANGED" || f.writes != 0 {
		t.Fatal(r)
	}
	f = fixtureESIM()
	f.changed = true
	r = perform(f, "rename")
	if r.Verified || r.Issue != "DEVICE_CHANGED" {
		t.Fatal(r)
	}
}
func TestESIMRejectsActiveDeletionAndPolicy(t *testing.T) {
	f := fixtureESIM()
	f.profiles[0].ProfileState = sgp22.ProfileEnabled
	if r := perform(f, "delete"); r.Issue != "PROFILE_DELETE_BLOCKED" || f.writes != 0 {
		t.Fatal(r)
	}
	f.profiles[0].ProfilePolicyRules.DisablingNotAllowed = true
	if r := perform(f, "disable"); r.Issue != "PROFILE_POLICY" || f.writes != 0 {
		t.Fatal(r)
	}
}
func TestESIMNoFakeSuccessOrRepeatedStateWrite(t *testing.T) {
	f := fixtureESIM()
	f.fail = true
	if r := perform(f, "rename"); r.Verified || r.Issue != "ESIM_RESULT_UNKNOWN" {
		t.Fatal(r)
	}
	f = fixtureESIM()
	if r := perform(f, "disable"); !r.Verified || f.writes != 0 {
		t.Fatal(r)
	}
}
func TestESIMPendingNotificationKeepsConfirmedResult(t *testing.T) {
	f := fixtureESIM()
	f.notifyFail = true
	r := perform(f, "rename")
	if !r.Verified || r.Warning != "ESIM_NOTIFICATION_PENDING" || f.removed != 0 {
		t.Fatal(r)
	}
}
func TestESIMDownloadAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := fixtureESIM()
		f.profiles = nil
		f.fail = fail
		r := executeESIM(context.Background(), f, ESIMRequest{Action: "download", EID: testCardEID, Activation: "LPA:1$carrier.example$test-only", IMEI: "123456789012345"}, func(string) {})
		if r.Verified == fail || r.TargetICCID != testICCID {
			t.Fatal(r)
		}
	}
}
func TestESIMExistingProfileIsNotNewDownload(t *testing.T) {
	f := fixtureESIM()
	request := ESIMRequest{Action: "download", EID: testCardEID, Activation: "LPA:1$carrier.example$test-only", IMEI: "123456789012345"}
	result := executeESIM(context.Background(), f, request, func(string) {})
	if result.Verified || result.TargetICCID != "" || len(f.profiles) != 1 {
		t.Fatal("existing profile reported as newly installed")
	}
	inventory, _ := cardInventory(f)
	if VerifyESIM(request, result, inventory) {
		t.Fatal("failed download reconciled as successful")
	}
}

func TestESIMMalformedInventory(t *testing.T) {
	f := fixtureESIM()
	f.eid = testEID
	if _, err := cardInventory(f); err == nil {
		t.Fatal("short EID accepted")
	}
	f = fixtureESIM()
	f.profiles = append(f.profiles, f.profiles[0])
	if _, err := cardInventory(f); err == nil {
		t.Fatal("duplicate ICCID accepted")
	}
}
func TestESIMRequestValidation(t *testing.T) {
	r := ESIMRequest{Action: "download", EID: testCardEID, Activation: "LPA:1$carrier.example$test-only", IMEI: "123456789012345"}
	if !ValidESIMRequest(r) {
		t.Fatal("valid activation rejected")
	}
	for _, bad := range []string{"LPA:10$host$code", "LPA:1$host/path$code", "LPA:1$user@host$code", "LPA:1$host:80$code", "LPA:1$host$code$$1", "LPA:1$host$has space", "LPA:1$host$code$x$2", "LPA:1$host$"} {
		q := r
		q.Activation = bad
		if ValidESIMRequest(q) {
			t.Fatal(bad)
		}
	}
	r.Activation += "$$1"
	r.Confirmation = "1234"
	if !ValidESIMRequest(r) {
		t.Fatal("confirmation rejected")
	}
	for _, label := range []string{"", "a\nb", strings.Repeat("中", 21), "\u200b"} {
		if ValidESIMRequest(ESIMRequest{Action: "rename", EID: testCardEID, ICCID: testICCID, Label: label}) {
			t.Fatal(label)
		}
	}
}
func TestESIMSSRFAndTLSValidation(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.194.129", "169.254.169.254", "::1", "fc00::1", "::ffff:127.0.0.1", "100.64.0.1"} {
		if publicAddress(net.ParseIP(s)) {
			t.Fatal(s)
		}
	}
	if !publicAddress(net.ParseIP("1.1.1.1")) {
		t.Fatal("public IP rejected")
	}
	for _, s := range []string{"http://carrier.example", "https://user@carrier.example", "https://carrier.example:8080", "https://carrier.example?secret=1"} {
		u, _ := url.Parse(s)
		if validESIMURL(u) {
			t.Fatal(s)
		}
	}
	client, close := esimHTTP(context.Background())
	defer close()
	tr := client.Transport.(*esimTransport).transport
	if tr.TLSClientConfig.InsecureSkipVerify || tr.Proxy != nil {
		t.Fatal("unsafe TLS or inherited proxy")
	}
}
func TestCSIMValidation(t *testing.T) {
	data, err := parseCSIM([]string{`+CSIM: 6,"019000"`})
	if err != nil || !reflect.DeepEqual(data, []byte{1, 0x90, 0}) {
		t.Fatal(data, err)
	}
	for _, s := range []string{`+CSIM: 4,"019000"`, `+CSIM: 4,"ZZZZ"`, `+CSIM: 0,""`} {
		if _, err := parseCSIM([]string{s}); err == nil {
			t.Fatal(s)
		}
	}
}
func TestLogicalChannelLifecycle(t *testing.T) {
	var commands []string
	responses := [][]byte{{1, 0x90, 0}, {0x61, 2}, {0x90, 0}, {0x90, 0}}
	c := &cardChannel{ctx: context.Background(), close: func() error { return nil }, send: func(_ context.Context, b []byte) ([]byte, error) {
		commands = append(commands, hex.EncodeToString(b))
		v := responses[0]
		responses = responses[1:]
		return v, nil
	}}
	channel, err := c.OpenLogicalChannel([]byte{0xa0, 0x00})
	if err != nil || channel != 1 {
		t.Fatal(err)
	}
	if err = c.CloseLogicalChannel(channel); err != nil {
		t.Fatal(err)
	}
	want := []string{"0070000001", "01a4040002a000", "81c0000002", "0070800100"}
	if !reflect.DeepEqual(commands, want) {
		t.Fatal(commands)
	}
}
