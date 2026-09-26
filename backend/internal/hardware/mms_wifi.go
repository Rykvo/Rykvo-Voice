package hardware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"rykvo.local/auth/internal/mms"
	"rykvo.local/auth/internal/vocat/vowifi"
	"rykvo.local/auth/internal/vocat/vowifi/ike"
)

// The MMS bearer shares the verified SIM/AKA adapter, not the IMS APN or radio.
type mmsTunnelProvider struct {
	base        vowifi.TunnelProvider
	mu          sync.Mutex
	lease       *mmsIMSLease
	nextAttempt time.Time
}
type mmsIMSLease struct {
	vowifi.TunnelSession
	request vowifi.TunnelRequest
	ctx     context.Context
	cancel  context.CancelFunc
}

func (l *mmsIMSLease) Close(ctx context.Context) error { l.cancel(); return l.TunnelSession.Close(ctx) }
func (l *mmsIMSLease) OpenMediaRoute(ctx context.Context, local, remote *net.UDPAddr) (io.Closer, error) {
	if router, ok := l.TunnelSession.(vowifi.MediaRouter); ok {
		return router.OpenMediaRoute(ctx, local, remote)
	}
	return nil, errors.New("VOICE_MEDIA_ROUTE_UNAVAILABLE")
}
func (l *mmsIMSLease) Failures() <-chan error {
	if n, ok := l.TunnelSession.(vowifi.RuntimeFailureNotifier); ok {
		return n.Failures()
	}
	return nil
}
func (p *mmsTunnelProvider) Start(ctx context.Context, request vowifi.TunnelRequest) (vowifi.TunnelSession, error) {
	s, err := p.base.Start(ctx, request)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	l := &mmsIMSLease{s, request, life, cancel}
	p.mu.Lock()
	p.lease = l
	p.mu.Unlock()
	return l, nil
}
func (p *mmsTunnelProvider) exchange(ctx context.Context, c mmsCommand, store func(mms.PDU) error) (string, error) {
	p.mu.Lock()
	l := p.lease
	cooling := time.Now().Before(p.nextAttempt)
	p.mu.Unlock()
	if l == nil || l.ctx.Err() != nil || cooling || !l.Evidence().Established {
		return "", mms.ErrNetwork
	}
	selection := matchCarrier(l.request.Identity)
	if selection.MMSWiFi.Status != "matched" || selection.MMSWiFi.Profile == nil || *selection.MMSWiFi.Profile != c.Profile {
		return "", errors.New("MMS_CONFIG_REQUIRED")
	}
	if !apnNamePattern.MatchString(c.Profile.APN) || strings.EqualFold(c.Profile.APN, "ims") || strings.EqualFold(c.Profile.APN, "sos") {
		return "", errors.New("MMS_PROFILE_UNSUPPORTED")
	}
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	provider, err := ike.NewProvider(ike.Config{APN: c.Profile.APN, DataNetwork: true, DataProtocol: c.Profile.Protocol, AutoProposalFallback: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return "", errors.New("MMS_PROFILE_UNSUPPORTED")
	}
	setup, done := context.WithTimeout(call, 90*time.Second)
	s, err := provider.Start(setup, l.request)
	done()
	if err != nil {
		p.mu.Lock()
		p.nextAttempt = time.Now().Add(5 * time.Minute)
		p.mu.Unlock()
		return "", errors.New(mmsSetupIssue(err))
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		_ = s.Close(clean)
	}()
	data, ok := s.(*ike.Session)
	if !ok {
		return "", mms.ErrNetwork
	}
	client, err := mms.NewClient(c.Profile, data.DialData)
	if err != nil {
		return "", err
	}
	if c.Op == "mms-send" {
		return client.Send(call, c.ID, c.To, c.Text, c.Image)
	}
	v, err := client.Retrieve(call, c.Location)
	if err != nil {
		return "", err
	}
	if err = store(v); err != nil {
		return "", err
	}
	_ = client.Acknowledge(call, c.Transaction)
	return "", nil
}

func mmsBridgeCode(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "MMS_IWLAN_AUTH_REJECTED", "MMS_IWLAN_TIMEOUT", "MMS_IWLAN_ROUTE_FAILED", "MMS_IWLAN_SECURITY_FAILED", "MMS_NETWORK_REQUIRED", "MMS_IWLAN_UNAVAILABLE", "MMS_CONFIG_REQUIRED", "MMS_PROFILE_UNSUPPORTED", "MMS_REJECTED", "MMS_INVALID_PDU", "MMS_STORAGE_UNCONFIRMED", "MMS_BUSY":
		return err.Error()
	}
	return "MMS_OUTCOME_UNKNOWN"
}

var _ vowifi.TunnelProvider = (*mmsTunnelProvider)(nil)

func mmsSetupIssue(err error) string {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "authentication_failed"), strings.Contains(text, "authentication failed"), strings.Contains(text, "eap failure"):
		return "MMS_IWLAN_AUTH_REJECTED"
	case strings.Contains(text, "timeout"), strings.Contains(text, "timed out"):
		return "MMS_IWLAN_TIMEOUT"
	case strings.Contains(text, "install child_sa"):
		return "MMS_IWLAN_ROUTE_FAILED"
	case strings.Contains(text, "certificate"), strings.Contains(text, "responder auth"):
		return "MMS_IWLAN_SECURITY_FAILED"
	default:
		return "MMS_IWLAN_UNAVAILABLE"
	}
}
