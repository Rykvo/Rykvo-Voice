package hardware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"rykvo.local/auth/internal/carrierconfig"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const WiFiWorkerSocket = "/run/rykvo-voice-wifi.sock"

// The web process never receives CAP_NET_ADMIN. A private socket activates one
// fixed-function worker; no paths, AT commands, shell commands or keys are RPCs.
type VocatWorkerClient struct {
	Socket      string
	OnSMS       func(context.Context, Candidate, string, SMSDelivery) error
	smsMu       sync.Mutex
	smsSessions map[string]*smsClientSession
}

type wifiWorkerRequest struct {
	Endpoint, Generation, HardwareKey, ICCID string
	RestoreOnly                              bool `json:"restoreOnly,omitempty"`
}
type wifiWorkerEvent struct {
	SMS       *SMSDelivery `json:"sms,omitempty"`
	SMSResult *SMSReply    `json:"smsResult,omitempty"`

	CarrierConfig *carrierconfig.Selection `json:"carrierConfig,omitempty"`
	PhoneNumber   string                   `json:"phoneNumber,omitempty"`
	Stage         string                   `json:"stage,omitempty"`
	Done          bool                     `json:"done,omitempty"`
	Code          string                   `json:"code,omitempty"`
}
type wifiWorkerEngine interface {
	WiFi(context.Context, Candidate, string, string, func(string)) error
}

func (client *VocatWorkerClient) WiFi(ctx context.Context, c Candidate, identity, iccid string, emit func(string)) error {
	return client.WiFiWithPolicy(ctx, c, identity, iccid, func() bool { return false }, emit)
}
func (client *VocatWorkerClient) WiFiWithPolicy(ctx context.Context, c Candidate, identity, iccid string, restore func() bool, emit func(string)) error {
	path := client.Socket
	if path == "" {
		path = WiFiWorkerSocket
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return errors.New("WIFI_WORKER_UNAVAILABLE")
	}
	defer conn.Close()
	session := newSMSClientSession(ctx, conn, func(ctx context.Context, delivery SMSDelivery) error {
		if client.OnSMS == nil {
			return errors.New("SMS_STORAGE_UNAVAILABLE")
		}
		return client.OnSMS(ctx, c, iccid, delivery)
	})
	key := smsSessionKey(c, iccid)
	client.smsMu.Lock()
	if client.smsSessions == nil {
		client.smsSessions = map[string]*smsClientSession{}
	}
	client.smsSessions[key] = session
	client.smsMu.Unlock()
	defer func() {
		client.smsMu.Lock()
		if client.smsSessions[key] == session {
			delete(client.smsSessions, key)
		}
		client.smsMu.Unlock()
		session.close()
	}()
	return wifiWorkerExchangeSMS(ctx, conn, wifiWorkerRequest{Endpoint: c.Key, Generation: c.Generation, HardwareKey: identity, ICCID: iccid}, emit, session, restore)
}

func wifiWorkerExchange(ctx context.Context, conn net.Conn, request wifiWorkerRequest, emit func(string), restore ...func() bool) error {
	return wifiWorkerExchangeSMS(ctx, conn, request, emit, nil, restore...)
}
func wifiWorkerExchangeSMS(ctx context.Context, conn net.Conn, request wifiWorkerRequest, emit func(string), sms *smsClientSession, restore ...func() bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if json.NewEncoder(conn).Encode(request) != nil {
		return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
	}
	if sms != nil {
		close(sms.ready)
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		// Request cleanup, but retain the read side and the caller's device gate
		// until the worker acknowledges cleanup. A missing ACK is not success.
		_ = conn.SetReadDeadline(time.Now().Add(80 * time.Second))
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		command := "stop\n"
		if len(restore) > 0 && restore[0] != nil && restore[0]() {
			command = "stop-cellular\n"
		}
		if sms != nil {
			sms.writeMu.Lock()
			defer sms.writeMu.Unlock()
		}
		_, _ = io.WriteString(conn, command)
	})
	defer func() {
		if !stop() {
			<-stopped
		}
	}()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024), 32768)
	for scanner.Scan() {
		var event wifiWorkerEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			break
		}
		if event.SMS != nil || event.SMSResult != nil {
			if sms == nil || event.CarrierConfig != nil || event.PhoneNumber != "" || event.Stage != "" || event.Done || event.Code != "" || (event.SMS != nil && event.SMSResult != nil) {
				break
			}
			if !sms.event(event) {
				break
			}
			continue
		}
		if event.CarrierConfig != nil {
			if !event.CarrierConfig.Valid() || event.PhoneNumber != "" || event.Stage != "" || event.Done || event.Code != "" {
				break
			}
			b, _ := json.Marshal(event.CarrierConfig)
			emit("config:" + string(b))
			continue
		}
		if event.PhoneNumber != "" {
			if !ValidAssociatedNumber(event.PhoneNumber) || event.Stage != "" || event.Done || event.Code != "" {
				break
			}
			emit("phone:" + event.PhoneNumber)
			continue
		}
		if event.Done {
			if event.Code == "" {
				return nil
			}
			if event.Code == "CANCELLED" {
				return context.Canceled
			}
			return errors.New(wifiWorkerCode(errors.New(event.Code)))
		}
		if wifiWorkerStage(event.Stage) {
			emit(event.Stage)
		} else {
			break
		}
	}
	return errors.New("WIFI_RADIO_RESTORE_UNCONFIRMED")
}

func wifiWorkerStage(s string) bool {
	switch s {
	case "sms-ready", "sms-unavailable", "connected", "reconnecting", "ims-cleaned", "radio-restored", "radio-off":
		return true
	}
	if s == "diagnostic:ims-deregistration-unconfirmed" || s == "diagnostic:network-retry-scheduled" {
		return true
	}
	if strings.HasPrefix(s, "diagnostic:") {
		for _, kind := range []string{"ims", "tunnel", "other"} {
			for _, reason := range []string{"other", "timeout", "closed", "expired", "eof", "refresh", "rejected", "auth", "throttled", "reset"} {
				if s == "diagnostic:"+kind+"-"+reason {
					return true
				}
			}
		}
		for _, component := range []string{"ims", "tunnel", "radio", "unknown"} {
			if s == "diagnostic:cleanup-"+component {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(s, "carrier:") && carrierID.MatchString(strings.TrimPrefix(s, "carrier:"))
}
func wifiWorkerCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELLED"
	}
	for _, code := range []string{"DEVICE_CHANGED", "DEVICE_BUSY", "DEVICE_UNAVAILABLE", "PERMISSION_DENIED", "READ_TIMEOUT", "SIM_NOT_READY", "WIFI_MODEM_UNSUPPORTED", "WIFI_DATA_ACTIVE", "WIFI_DATA_STATE_UNKNOWN", "WIFI_RADIO_STATE_UNKNOWN", "WIFI_RADIO_RESTORE_UNCONFIRMED", "WIFI_AUTH_REJECTED", "WIFI_CONNECTION_FAILED"} {
		if err.Error() == code {
			return code
		}
	}
	return "WIFI_CONNECTION_FAILED"
}

// StandardInput/Output are the accepted systemd socket, never a public listener.
func WiFiWorker() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return serveWiFiWorker(ctx, os.Stdin, os.Stdout, &VocatWiFi{System: NewSystem()})
}

func serveWiFiWorker(parent context.Context, input io.Reader, output io.Writer, engine wifiWorkerEngine) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1024), 32768)
	var request wifiWorkerRequest
	// stdin socket deadlines are not portable. Bound initial input explicitly;
	// the single-session process exits even if a hostile peer leaves it idle.
	first := make(chan bool, 1)
	go func() { first <- scanner.Scan() }()
	idle := time.NewTimer(10 * time.Second)
	defer idle.Stop()
	var valid bool
	select {
	case valid = <-first:
	case <-parent.Done():
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		return parent.Err()
	case <-idle.C:
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		return errors.New("DEVICE_CHANGED")
	}
	idle.Stop()
	if valid {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		valid = decoder.Decode(&request) == nil && decoder.Decode(new(any)) == io.EOF
	}
	encoder := &smsEventEncoder{encoder: json.NewEncoder(output)}
	if !valid || len(request.Endpoint) > 256 || request.Endpoint == "" || request.Generation == "" || len(request.Generation) > 256 || !strings.HasPrefix(request.HardwareKey, "imei:") || len(request.HardwareKey) != 69 || !decimal(request.ICCID, 18, 20) {
		_ = encoder.Encode(wifiWorkerEvent{Done: true, Code: "DEVICE_CHANGED"})
		return errors.New("DEVICE_CHANGED")
	}
	if request.RestoreOnly {
		actual, ok := engine.(*VocatWiFi)
		var err error
		if !ok {
			err = errors.New("WIFI_MODEM_UNSUPPORTED")
		} else {
			err = actual.RestoreRadio(ctx, Candidate{Key: request.Endpoint, Generation: request.Generation}, request.HardwareKey, request.ICCID)
		}
		_ = encoder.Encode(wifiWorkerEvent{Done: true, Code: wifiWorkerCode(err)})
		return err
	}
	sms := newSMSWorker(ctx, encoder, cancel)
	// EOF, explicit stop, or malformed extra input all cancel this one lease.
	var restore atomic.Bool
	if actual, ok := engine.(*VocatWiFi); ok {
		copied := *actual
		copied.RestoreCellular = restore.Load
		copied.messaging = sms
		engine = &copied
	}
	go func() {
		defer cancel()
		for scanner.Scan() {
			switch scanner.Text() {
			case "stop-cellular":
				restore.Store(true)
				return
			case "stop":
				return
			default:
				if !sms.command(scanner.Bytes()) {
					return
				}
			}
		}
	}()
	err := engine.WiFi(ctx, Candidate{Key: request.Endpoint, Generation: request.Generation}, request.HardwareKey, request.ICCID, func(stage string) {
		if raw, ok := strings.CutPrefix(stage, "config:"); ok {
			var selection carrierconfig.Selection
			if json.Unmarshal([]byte(raw), &selection) == nil && selection.Valid() {
				if encoder.Encode(wifiWorkerEvent{CarrierConfig: &selection}) != nil {
					cancel()
				}
			}
			return
		}
		if number, ok := strings.CutPrefix(stage, "phone:"); ok {
			if ValidAssociatedNumber(number) && encoder.Encode(wifiWorkerEvent{PhoneNumber: number}) != nil {
				cancel()
			}
			return
		}
		if wifiWorkerStage(stage) && encoder.Encode(wifiWorkerEvent{Stage: stage}) != nil {
			cancel()
		}
	})
	// Engine returns only after protocol teardown and RF restoration complete.
	_ = encoder.Encode(wifiWorkerEvent{Done: true, Code: wifiWorkerCode(err)})
	return err
}

func (client *VocatWorkerClient) RestoreRadio(ctx context.Context, c Candidate, identity, iccid string) error {
	path := client.Socket
	if path == "" {
		path = WiFiWorkerSocket
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", path)
	if err != nil {
		return errors.New("WIFI_WORKER_UNAVAILABLE")
	}
	defer conn.Close()
	return wifiWorkerExchange(ctx, conn, wifiWorkerRequest{Endpoint: c.Key, Generation: c.Generation, HardwareKey: identity, ICCID: iccid, RestoreOnly: true}, func(string) {})
}
