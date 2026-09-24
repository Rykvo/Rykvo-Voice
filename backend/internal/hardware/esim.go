package hardware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/damonto/euicc-go/bertlv"
	"github.com/damonto/euicc-go/driver"
	"github.com/damonto/euicc-go/lpa"
	sgp22 "github.com/damonto/euicc-go/v2"
)

type ESIMProfile struct {
	ICCID      string `json:"iccid"`
	Label      string `json:"label"`
	Provider   string `json:"provider"`
	Enabled    bool   `json:"enabled"`
	CanDisable bool   `json:"canDisable"`
	CanDelete  bool   `json:"canDelete"`
}
type ESIMInfo struct {
	EID        string        `json:"eid"`
	Profiles   []ESIMProfile `json:"profiles"`
	Pending    int           `json:"pending"`
	FreeMemory *int          `json:"freeMemory,omitempty"`
	Issue      string        `json:"issue,omitempty"`
}
type ESIMRequest struct {
	Candidate    Candidate `json:"candidate"`
	Action       string    `json:"action"`
	EID          string    `json:"eid"`
	ICCID        string    `json:"iccid,omitempty"`
	Label        string    `json:"label,omitempty"`
	Activation   string    `json:"activation,omitempty"`
	Confirmation string    `json:"confirmation,omitempty"`
	IMEI         string    `json:"imei,omitempty"`
	ExpectedIMEI string    `json:"expectedIMEI,omitempty"`
}
type ESIMResult struct {
	Info        *ESIMInfo `json:"info,omitempty"`
	Issue       string    `json:"issue,omitempty"`
	Warning     string    `json:"warning,omitempty"`
	Changed     bool      `json:"changed"`
	Verified    bool      `json:"verified"`
	TargetICCID string    `json:"targetICCID,omitempty"`
}
type esimEvent struct {
	Stage  string      `json:"stage,omitempty"`
	Result *ESIMResult `json:"result,omitempty"`
}

func decimal(value string, min, max int) bool {
	return len(value) >= min && len(value) <= max && strings.Trim(value, "0123456789") == ""
}
func ProfileID(eid, iccid string) string { return "line-" + Digest(eid + ":" + iccid)[:24] }
func ValidESIMRequest(r ESIMRequest) bool {
	if r.Action == "read" {
		return true
	}
	if !decimal(r.EID, 32, 32) {
		return false
	}
	switch r.Action {
	case "download":
		_, err := activationCode(r)
		return err == nil
	case "notifications":
		return true
	case "enable", "disable", "delete":
		return decimal(r.ICCID, 18, 20)
	case "rename":
		if !decimal(r.ICCID, 18, 20) || !utf8.ValidString(r.Label) || utf8.RuneCountInString(r.Label) > 20 || strings.TrimSpace(r.Label) == "" {
			return false
		}
		for _, ch := range r.Label {
			if unicode.IsControl(ch) || unicode.In(ch, unicode.Cf) {
				return false
			}
		}
		return true
	}
	return false
}
func activationCode(r ESIMRequest) (*lpa.ActivationCode, error) {
	parts := strings.Split(r.Activation, "$")
	if len(r.Activation) > 2048 || len(parts) < 3 || len(parts) > 5 || parts[0] != "LPA:1" || parts[2] == "" || strings.ContainsAny(r.Activation, "\r\n\t ") || len(r.Confirmation) > 128 || !decimal(r.IMEI, 15, 15) {
		return nil, errors.New("INVALID_ACTIVATION")
	}
	var ac lpa.ActivationCode
	if ac.UnmarshalText([]byte(r.Activation)) != nil || !validESIMURL(ac.SMDP) || ac.SMDP.Path != "" {
		return nil, errors.New("INVALID_ACTIVATION")
	}
	if len(parts) == 5 && parts[4] != "" && parts[4] != "1" {
		return nil, errors.New("INVALID_ACTIVATION")
	}
	if len(parts) == 5 && parts[4] == "1" && r.Confirmation == "" {
		return nil, errors.New("CONFIRMATION_REQUIRED")
	}
	ac.ConfirmationCode = r.Confirmation
	ac.IMEI = r.IMEI
	return &ac, nil
}

// Secrets travel only through stdin. Driver crashes and hangs stay outside the server.
func ESIMCall(ctx context.Context, request ESIMRequest, progress func(string)) ESIMResult {
	exe, err := os.Executable()
	if err != nil {
		return ESIMResult{Issue: "ESIM_UNAVAILABLE"}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return ESIMResult{Issue: "ESIM_UNAVAILABLE"}
	}
	defer clear(data)
	cmd := exec.CommandContext(ctx, exe, "-hardware-esim")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return ESIMResult{Issue: "ESIM_UNAVAILABLE"}
	}
	result := ESIMResult{Issue: "ESIM_INTERRUPTED"}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	valid := true
	for scanner.Scan() {
		var event esimEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			valid = false
			break
		}
		if event.Stage != "" && progress != nil {
			progress(event.Stage)
		}
		if event.Result != nil {
			result = *event.Result
		}
	}
	if scanner.Err() != nil || !valid {
		_ = cmd.Process.Kill()
	}
	if err = cmd.Wait(); err != nil {
		result.Issue = "ESIM_INTERRUPTED"
		result.Verified = false
	}
	return result
}
func ESIMHelper() error {
	var request ESIMRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || !ValidESIMRequest(request) {
		return errors.New("INVALID_ESIM_REQUEST")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	enc := json.NewEncoder(os.Stdout)
	progress := func(stage string) { _ = enc.Encode(esimEvent{Stage: stage}) }
	var result ESIMResult
	use := func(ch driver.SmartCardChannel) ESIMResult {
		client, err := lpa.New(&lpa.Options{Channel: ch, MSS: 120, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			return ESIMResult{Issue: esimError(err)}
		}
		defer client.Close()
		httpClient, closeHTTP := esimHTTP(ctx)
		defer closeHTTP()
		client.HTTP.Client = httpClient
		return executeESIM(ctx, client, request, progress)
	}
	c := request.Candidate
	if c.Kind == "reader" {
		_, err := withPCSC(c.Reader, func(ch *cardChannel, _ string) (any, error) { ch.ctx = ctx; result = use(ch); return nil, nil })
		if err != nil {
			result.Issue = esimError(err)
		}
	} else if len(c.Ports) > 0 {
		ch, err := openATCard(ctx, c, request.ExpectedIMEI)
		if err != nil {
			result.Issue = esimError(err)
		} else {
			result = use(ch)
		}
	} else if c.Control != "" {
		ch, err := openQMICard(ctx, c.Control)
		if err != nil {
			result.Issue = esimError(err)
		} else {
			result = use(ch)
		}
	} else {
		result.Issue = "ESIM_UNAVAILABLE"
	}
	return enc.Encode(esimEvent{Result: &result})
}

type esimClient interface {
	EID() ([]byte, error)
	ListProfile(any, []bertlv.Tag) ([]*sgp22.ProfileInfo, error)
	EUICCInfo2() (*bertlv.TLV, error)
	ListNotification(...sgp22.NotificationEvent) ([]*sgp22.NotificationMetadata, error)
	EnableProfile(any, bool) error
	DisableProfile(any, bool) error
	DeleteProfile(any) error
	SetNickname(sgp22.ICCID, string) error
	DownloadProfile(context.Context, *lpa.ActivationCode, *lpa.DownloadOptions) (*sgp22.LoadBoundProfilePackageResponse, error)
	RetrieveNotificationList(any) ([]*sgp22.PendingNotification, error)
	HandleNotification(*sgp22.PendingNotification) error
	RemoveNotificationFromList(sgp22.SequenceNumber) error
}

func cardInventory(client esimClient) (*ESIMInfo, error) {
	eid, err := client.EID()
	if err != nil {
		return nil, err
	}
	info := &ESIMInfo{EID: hex.EncodeToString(eid), Profiles: []ESIMProfile{}, Pending: -1}
	if !decimal(info.EID, 32, 32) {
		return nil, errors.New("INVALID_RESPONSE")
	}
	profiles, err := client.ListProfile(nil, []bertlv.Tag{sgp22.TagProfilePolicyRules})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		iccid := p.ICCID.String()
		if !decimal(iccid, 18, 20) || seen[iccid] {
			return nil, errors.New("INVALID_RESPONSE")
		}
		seen[iccid] = true
		label := p.ProfileNickname
		if label == "" {
			label = p.ProfileName
		}
		if label == "" {
			label = p.ServiceProviderName
		}
		if label == "" {
			label = iccid
		}
		info.Profiles = append(info.Profiles, ESIMProfile{ICCID: iccid, Label: label, Provider: p.ServiceProviderName, Enabled: p.ProfileState == sgp22.ProfileEnabled, CanDisable: !p.ProfilePolicyRules.DisablingNotAllowed, CanDelete: !p.ProfilePolicyRules.DeletionNotAllowed})
	}
	if data, err := client.EUICCInfo2(); err == nil && data != nil {
		if resource := data.First(bertlv.ContextSpecific.Constructed(4)); resource != nil {
			if free := resource.First(bertlv.ContextSpecific.Primitive(2)); free != nil && len(free.Value) > 0 && len(free.Value) <= 4 {
				count := 0
				for _, b := range free.Value {
					count = count*256 + int(b)
				}
				info.FreeMemory = &count
			}
		}
	}
	if notifications, err := client.ListNotification(); err == nil {
		info.Pending = len(notifications)
	}
	return info, nil
}
func profileIn(info *ESIMInfo, iccid string) *ESIMProfile {
	if info != nil {
		for i := range info.Profiles {
			if info.Profiles[i].ICCID == iccid {
				return &info.Profiles[i]
			}
		}
	}
	return nil
}
func executeESIM(ctx context.Context, client esimClient, r ESIMRequest, progress func(string)) ESIMResult {
	info, err := cardInventory(client)
	if err != nil {
		return ESIMResult{Issue: esimError(err)}
	}
	result := ESIMResult{Info: info}
	if r.Action == "read" {
		result.Verified = true
		return result
	}
	if info.EID != r.EID {
		result.Issue = "DEVICE_CHANGED"
		return result
	}
	profile := profileIn(info, r.ICCID)
	if r.Action != "download" && r.Action != "notifications" && profile == nil {
		result.Issue = "PROFILE_NOT_FOUND"
		return result
	}
	if r.Action == "delete" && (profile.Enabled || !profile.CanDelete) {
		result.Issue = "PROFILE_DELETE_BLOCKED"
		return result
	}
	if r.Action == "disable" && !profile.CanDisable {
		result.Issue = "PROFILE_POLICY"
		return result
	}
	if r.Action == "enable" && profile.Enabled || r.Action == "disable" && !profile.Enabled || r.Action == "rename" && profile.Label == r.Label {
		result.Verified = true
		return result
	}
	iccid, _ := sgp22.NewICCID(r.ICCID)
	for len(iccid) < 10 {
		iccid = append(iccid, 0xff)
	}
	progress("writing")
	result.Changed = true
	wantICCID := r.ICCID
	switch r.Action {
	case "enable":
		err = client.EnableProfile(iccid, true)
	case "disable":
		err = client.DisableProfile(iccid, true)
	case "delete":
		err = client.DeleteProfile(iccid)
	case "rename":
		err = client.SetNickname(iccid, r.Label)
	case "download":
		ac, validationErr := activationCode(r)
		if validationErr != nil {
			result.Issue = validationErr.Error()
			return result
		}
		var downloaded *sgp22.LoadBoundProfilePackageResponse
		downloaded, err = client.DownloadProfile(ctx, ac, &lpa.DownloadOptions{
			OnProgress: func(stage lpa.DownloadStage) {
				progress(map[lpa.DownloadStage]string{lpa.DownloadStageAuthenticateClient: "authenticating", lpa.DownloadStageAuthenticateServer: "downloading", lpa.DownloadStageInstall: "installing"}[stage])
			},
			OnConfirm: func(p *sgp22.ProfileInfo) bool {
				candidate := p.ICCID.String()
				if !decimal(candidate, 18, 20) || profileIn(info, candidate) != nil {
					wantICCID = ""
					return false
				}
				wantICCID = candidate
				return true
			},
		})
		if err == nil && downloaded == nil {
			err = errors.New("ESIM_RESULT_UNKNOWN")
		}
	case "notifications":
	default:
		result.Issue = "INVALID_ESIM_REQUEST"
		return result
	}
	if err != nil {
		result.TargetICCID = wantICCID
		result.Issue = esimError(err)
		result.Info = nil
		return result
	}
	progress("verifying")
	result.TargetICCID = wantICCID
	next, err := cardInventory(client)
	if err != nil {
		result.Info = nil
		result.Issue = "ESIM_RESULT_UNKNOWN"
		return result
	}
	if next.EID != r.EID {
		result.Info = nil
		result.Issue = "DEVICE_CHANGED"
		return result
	}
	result.Info = next
	result.Verified = VerifyESIM(r, result, next)
	if !result.Verified {
		result.Issue = "ESIM_RESULT_UNKNOWN"
		return result
	}
	progress("notifying")
	pending, err := client.RetrieveNotificationList(nil)
	if err == nil {
		for _, notification := range pending {
			if err = client.HandleNotification(notification); err != nil {
				break
			}
			if err = client.RemoveNotificationFromList(notification.Notification.SequenceNumber); err != nil {
				break
			}
		}
	}
	if err != nil {
		result.Warning = "ESIM_NOTIFICATION_PENDING"
	}
	if notices, err := client.ListNotification(); err == nil {
		result.Info.Pending = len(notices)
	}
	return result
}
func VerifyESIM(r ESIMRequest, result ESIMResult, info *ESIMInfo) bool {
	if info == nil || info.EID != r.EID || info.Issue != "" {
		return false
	}
	iccid := r.ICCID
	if r.Action == "download" {
		iccid = result.TargetICCID
	}
	p := profileIn(info, iccid)
	switch r.Action {
	case "enable":
		return p != nil && p.Enabled
	case "disable":
		return p != nil && !p.Enabled
	case "delete":
		return p == nil
	case "rename":
		return p != nil && p.Label == r.Label
	case "download":
		return iccid != "" && p != nil
	case "notifications":
		return true
	}
	return false
}
func esimError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errTimeout) {
		return "READ_TIMEOUT"
	}
	if errors.Is(err, sgp22.ErrICCIDNotFound) {
		return "PROFILE_NOT_FOUND"
	}
	var operation *sgp22.ProfileOperationError
	if errors.As(err, &operation) {
		return "ESIM_CARD_REJECTED"
	}
	for _, code := range []string{"NO_EUICC", "NO_SIM", "EUICC_CHANNEL_UNAVAILABLE", "DEVICE_CHANGED", "DEVICE_BUSY", "PERMISSION_DENIED", "ESIM_NETWORK_FAILED", "ESIM_REMOTE_ADDRESS", "INVALID_RESPONSE", "ESIM_RESULT_UNKNOWN"} {
		if strings.Contains(err.Error(), code) {
			return code
		}
	}
	if strings.Contains(err.Error(), "confirmation code is required") {
		return "CONFIRMATION_REQUIRED"
	}
	return "ESIM_OPERATION_FAILED"
}
