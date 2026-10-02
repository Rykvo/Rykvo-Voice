package main

import (
	"context"
	"errors"
	"log"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/uplink"
	"strings"
	"time"
)

type moduleWiFi struct {
	ICCID, RequestID   string
	Enabled            bool
	State, Issue       string
	Diagnostic         string
	Profile, Transport string
	Registered         bool
	registeredAt       time.Time
	unregisteredSince  time.Time
	SMSReady           bool
	RadioOff           bool
	refreshUntil       time.Time
	candidate          hardware.Candidate
	rebinding          bool
	networkRetry       time.Time
	retryFailures      uint
	recovery           wifiRecovery
	running            bool
	cancel             context.CancelFunc
}

func (w *moduleWiFi) setRegistered(registered bool, now time.Time) {
	if registered {
		w.recovery = wifiRecovery{}
		if !w.Registered {
			w.registeredAt = now
		}
		w.unregisteredSince = time.Time{}
	} else if w.unregisteredSince.IsZero() {
		w.unregisteredSince = now
	}
	w.Registered = registered
}

type wifiSource interface {
	// Block through registration maintenance and cleanup. Cancellation must release
	// the SIM and restore radio state before returning and releasing the module gate.
	WiFi(context.Context, hardware.Candidate, string, string, func(string)) error
}

func (m *moduleManager) loadWiFi(ctx context.Context) error {
	rows, err := m.db.Query(ctx, "SELECT module_id,iccid,enabled,request_id FROM module_wifi")
	if err != nil {
		return err
	}
	defer rows.Close()
	wifi := make(map[int64]*moduleWiFi)
	for rows.Next() {
		var id int64
		w := &moduleWiFi{State: "off"}
		if err := rows.Scan(&id, &w.ICCID, &w.Enabled, &w.RequestID); err != nil {
			return err
		}
		if w.Enabled {
			w.State = "waiting"
			w.setRegistered(false, time.Now())
		}
		wifi[id] = w
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.wifi = wifi
	m.mu.Unlock()
	return nil
}
func wifiLine(reading hardware.Reading) string {
	if reading.ICCID == "" {
		return ""
	}
	if reading.ESIM != nil && reading.ESIM.EID != "" {
		for _, p := range reading.ESIM.Profiles {
			if p.Enabled && p.ICCID == reading.ICCID {
				return hardware.ProfileID(reading.ESIM.EID, p.ICCID)
			}
		}
		return ""
	}
	return "line-" + hardware.Digest(reading.ICCID)[:24]
}
func (m *moduleManager) wifiView(id int64, iccid string) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := map[string]any{"enabled": false, "registered": false, "state": "off", "issue": ""}
	w := m.wifi[id]
	if w == nil || w.ICCID != iccid {
		return v
	}
	v["enabled"], v["registered"], v["state"], v["issue"] = w.Enabled, w.Registered, w.State, w.Issue
	v["carrierProfile"], v["transport"] = w.Profile, w.Transport
	v["diagnostic"] = w.Diagnostic
	v["radioOffConfirmed"] = w.RadioOff
	v["smsReady"] = w.Enabled && w.Registered && w.SMSReady
	v["recovering"] = w.Enabled && !w.Registered && w.recoveryPending(m.wifiRetryLimit, time.Now())
	v["retrying"] = w.Enabled && !w.Registered && (v["recovering"] == true || w.State == "connecting" || w.State == "waiting" || !w.networkRetry.IsZero() && (w.Issue == "NETWORK_UNAVAILABLE" || retryWiFiFailure(w)))
	return v
}
func (m *moduleManager) setWiFi(ctx context.Context, v moduleRecord, line, id string, enabled bool) error {
	if m.wifiEngine == nil {
		return errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	release, err := m.controlWrites.Lock(ctx, v.ID)
	if err != nil {
		return err
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining || !m.ready || m.ctx == nil || m.ctx.Err() != nil {
		return errors.New("DEVICE_UNAVAILABLE")
	}
	w := m.wifi[v.ID]
	if w != nil && w.RequestID == id {
		return nil
	}
	sample, ok := m.values[v.ID]
	current, present := m.seen[v.Endpoint]
	if !ok || !present || !sameEndpoint(sample.Candidate, current) || sample.Candidate.Key != v.Endpoint || time.Since(m.lastScan) > 20*time.Second {
		return errors.New("DEVICE_UNAVAILABLE")
	}
	if line != wifiLine(sample.Reading) || sample.Reading.SIM != "READY" {
		return errors.New("DEVICE_CHANGED")
	}
	if enabled && (!hardware.WiFiSupported(current) || sample.Reading.Issue != "" || !sample.Reading.Responsive) {
		return errors.New("WIFI_MODEM_UNSUPPORTED")
	}
	if enabled && (m.jobs[v.ID].active() || time.Now().Before(m.recoveryUntil[v.Endpoint])) {
		return errors.New("DEVICE_BUSY")
	}
	if enabled && w != nil && w.running && (!w.Enabled || w.ICCID != sample.Reading.ICCID) {
		return errors.New("DEVICE_BUSY")
	}
	if !enabled && (w == nil || !w.running) && (m.jobs[v.ID].active() || time.Now().Before(m.recoveryUntil[v.Endpoint])) {
		return errors.New("DEVICE_BUSY")
	}
	if m.work[v.Endpoint] > 0 {
		return errors.New("DEVICE_BUSY")
	}
	m.controlPending[v.Endpoint] = true
	defer delete(m.controlPending, v.Endpoint)
	m.operations.Add(1)
	defer m.operations.Done()
	m.mu.Unlock()
	_, err = m.db.Exec(ctx, `INSERT INTO module_wifi(module_id,iccid,enabled,request_id) VALUES($1,$2,$3,$4) ON CONFLICT(module_id) DO UPDATE SET iccid=$2,enabled=$3,request_id=$4`, v.ID, sample.Reading.ICCID, enabled, id)
	m.mu.Lock()
	if err != nil {
		return errors.New("DATABASE_UNAVAILABLE")
	}
	latest, present := m.seen[v.Endpoint]
	if !present || !sameEndpoint(latest, sample.Candidate) || m.values[v.ID].Reading.ICCID != sample.Reading.ICCID || m.jobs[v.ID].active() {
		return errors.New("DEVICE_CHANGED")
	}
	w = m.wifi[v.ID]
	if w == nil {
		w = &moduleWiFi{}
		m.wifi[v.ID] = w
	}
	if enabled && (!w.Enabled || w.ICCID != sample.Reading.ICCID) {
		w.unregisteredSince, w.registeredAt = time.Now(), time.Time{}
		w.recovery = wifiRecovery{}
	}
	w.ICCID, w.RequestID, w.Enabled = sample.Reading.ICCID, id, enabled
	if !enabled {
		w.recovery = wifiRecovery{}
		w.setRegistered(false, time.Now())
		if w.running {
			w.State = "stopping"
			w.cancel()
		} else {
			w.State, w.Issue = "off", ""
			if restorer, ok := m.wifiEngine.(radioRestorer); ok {
				call, cancel := context.WithTimeout(m.ctx, 30*time.Second)
				w.cancel = cancel
				w.running = true
				w.candidate = sample.Candidate
				w.State = "stopping"
				m.operations.Add(1)
				go func() { defer cancel(); m.runWiFi(call, v.ID, w, sample, radioRestoreSource{restorer}) }()
			}
		}
		return nil
	}
	if !w.running {
		w.State, w.Issue = "waiting", ""
		m.startWiFiLocked(v.ID, sample)
	}
	return nil
}

// Called under m.mu after a verified hardware sample.
func (m *moduleManager) startWiFiLocked(id int64, sample moduleSample) {
	w := m.wifi[id]
	if w != nil && w.recovery.Used && time.Now().Before(w.refreshUntil) {
		return
	}
	if w == nil || !w.Enabled || w.running || w.ICCID != sample.Reading.ICCID || sample.Reading.SIM != "READY" || !sample.Reading.Responsive || sample.Reading.Issue != "" || m.jobs[id].active() || time.Now().Before(m.recoveryUntil[sample.Candidate.Key]) {
		return
	}
	source := m.wifiEngine
	if source == nil || !hardware.WiFiSupported(sample.Candidate) {
		return
	}
	if m.draining || !m.ready || m.ctx == nil || m.ctx.Err() != nil || m.controlPending[sample.Candidate.Key] || m.networkChanging[id] {
		return
	}
	network := m.networks[id]
	if network == "" {
		network = uplink.DefaultNetwork()
	}
	if network != "" {
		if _, ok := source.(interface{ FixedNetworks() }); !ok {
			w.State, w.Issue = "failed", "NETWORK_UNSUPPORTED"
			return
		}
		if m.networkLinks[network] == "" {
			w.State, w.Issue = "failed", "NETWORK_UNAVAILABLE"
			return
		}
	}
	if w.State != "waiting" {
		retryNetwork := w.Issue == "NETWORK_UNAVAILABLE" && !time.Now().Before(w.networkRetry)
		retryTransport := !w.networkRetry.IsZero() && !time.Now().Before(w.networkRetry) && retryWiFiFailure(w)
		if (retryNetwork || retryTransport) && hardware.CommunicationHealthy(sample.Reading) {
			w.State = "waiting"
		}
	}
	if w.State != "waiting" {
		// A verified new USB generation gets one new attempt, not one per poll.
		if w.candidate.Key != sample.Candidate.Key || w.candidate.Generation == "" || sameEndpoint(w.candidate, sample.Candidate) || !hardware.CommunicationHealthy(sample.Reading) || !resumeWiFiIntent(w, sample.Reading) {
			return
		}
	}
	ctx, cancel := context.WithCancel(m.ctx)
	w.cancel, w.running, w.candidate = cancel, true, sample.Candidate
	w.State, w.Issue = "connecting", ""
	w.setRegistered(false, time.Now())
	w.Diagnostic = ""
	w.networkRetry = time.Time{}
	w.RadioOff = false
	m.operations.Add(1)
	go m.runWiFi(uplink.WithNetwork(ctx, network), id, w, sample, source)
}
func (m *moduleManager) runWiFi(ctx context.Context, id int64, w *moduleWiFi, sample moduleSample, source wifiSource) {
	defer m.operations.Done()
	gate := m.gate(sample.Candidate.Key)
	var err error
	acquired := false
	select {
	case gate <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	if acquired {
		defer func() { <-gate }()
	}
	if acquired && ctx.Err() == nil {
		connect := source.WiFi
		if policy, ok := source.(wifiPolicySource); ok {
			connect = func(ctx context.Context, c hardware.Candidate, identity, iccid string, emit func(string)) error {
				return policy.WiFiWithPolicy(ctx, c, identity, iccid, func() bool {
					m.mu.RLock()
					defer m.mu.RUnlock()
					return m.wifi[id] == w && (!w.Enabled || w.recovery.Stopping)
				}, emit)
			}
		}
		err = connect(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), sample.Reading.ICCID, func(stage string) {
			if raw, ok := strings.CutPrefix(stage, "config:"); ok {
				m.acceptCarrierConfig(ctx, id, w, sample, raw)
				return
			}
			if number, ok := strings.CutPrefix(stage, "phone:"); ok {
				m.acceptWiFiNumber(ctx, id, w, sample, number)
				return
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.wifi[id] != w {
				return
			}
			if strings.HasPrefix(stage, "carrier:") {
				w.Profile = strings.TrimPrefix(stage, "carrier:")
				log.Printf("module %d Wi-Fi profile: %s", id, w.Profile)
			}
			if strings.HasPrefix(stage, "attempt:") {
				log.Printf("module %d Wi-Fi attempt: %s", id, strings.TrimPrefix(stage, "attempt:"))
			}
			if strings.HasPrefix(stage, "cleanup-failed:") {
				log.Printf("module %d Wi-Fi cleanup: %s", id, strings.TrimPrefix(stage, "cleanup-failed:"))
			}
			if strings.HasPrefix(stage, "transport:") {
				w.Transport = strings.TrimPrefix(stage, "transport:")
				log.Printf("module %d Wi-Fi IMS transport: %s", id, w.Transport)
			}
			if strings.HasPrefix(stage, "diagnostic:") {
				w.recovery.RetryAllowed = false
				if stage != "diagnostic:network-retry-scheduled" && stage != "diagnostic:ims-deregistration-unconfirmed" && stage != "diagnostic:tunnel-delete-unconfirmed" {
					w.Diagnostic = strings.TrimPrefix(stage, "diagnostic:")
				}
				log.Printf("module %d Wi-Fi %s", id, stage)
			}
			switch stage {
			case "retry-failed":
				if w.Enabled && ctx.Err() == nil {
					m.wifiAttemptFailedLocked(id, w, time.Now())
				}
			case "sms-ready":
				w.SMSReady = true
			case "sms-unavailable":
				w.SMSReady = false
			case "radio-off":
				w.RadioOff = true
			case "connected", "renewed":
				if w.Enabled && ctx.Err() == nil {
					w.State, w.Issue = "connected", ""
					w.setRegistered(true, time.Now())
					w.Diagnostic = ""
					w.retryFailures = 0
				}
				value := m.values[id]
				if sameEndpoint(value.Candidate, sample.Candidate) {
					value.Reading.UpdatedAt = time.Now().UTC()
					m.values[id] = value
				}
			case "reconnecting":
				w.SMSReady = false
				w.State = "connecting"
				w.setRegistered(false, time.Now())
			case "ims-cleaned", "radio-restored":
				if stage == "radio-restored" {
					w.refreshUntil = time.Now().Add(80 * time.Second)
				}
				w.setRegistered(false, time.Now())
			}
		})
	}
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wifi[id] != w {
		return
	}
	w.running = false
	w.setRegistered(false, time.Now())
	if !w.Enabled {
		w.RadioOff = false
	}
	w.cancel = nil
	resetting := w.recovery.Stopping
	w.recovery.Stopping = false
	current, present := m.seen[sample.Candidate.Key]
	changed := !present || !sameEndpoint(current, sample.Candidate)
	code := hardware.WiFiIssue(err)
	uncertain := strings.HasSuffix(code, "_UNCONFIRMED")
	if uncertain {
		w.RadioOff = false
		// A disconnected worker may still be finishing its bounded cleanup.
		// Suppress immediate recovery/APN/eSIM writes against that endpoint.
		m.recoveryUntil[sample.Candidate.Key] = time.Now().Add(90 * time.Second)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		w.State, w.Issue = "failed", code
		log.Printf("module %d Wi-Fi stopped: %s", id, code)
	} else {
		w.State, w.Issue = "off", ""
		w.Diagnostic = ""
	}
	if resetting {
		if w.Enabled && !changed && !uncertain && (err == nil || errors.Is(err, context.Canceled)) {
			w.State, w.Issue = "waiting", ""
			w.RadioOff = false
			w.refreshUntil = time.Now().Add(80 * time.Second)
			w.recovery.VerifyUntil = time.Now().Add(4 * time.Minute)
			w.networkRetry = time.Time{}
			log.Printf("module %d Wi-Fi full recovery: cleanup confirmed", id)
		}
		return
	}
	if code == "NETWORK_UNAVAILABLE" {
		w.networkRetry = time.Now().Add(15 * time.Second)
	} else if w.Enabled && !uncertain && retryWiFiFailure(w) {
		delays := [...]time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
		index := min(w.retryFailures, uint(len(delays)-1))
		w.networkRetry = time.Now().Add(delays[index])
		if w.retryFailures < uint(len(delays)-1) {
			w.retryFailures++
		}
		log.Printf("module %d Wi-Fi retry in %s", id, delays[index])
	}
	rebind := w.rebinding
	if w.Enabled && (changed || rebind) && !uncertain {
		w.State, w.Issue = "waiting", ""
	}
	w.rebinding = false
	if rebind && !changed && !uncertain {
		m.resumeNetworkLocked(id)
	}
	if w.Enabled && ctx.Err() == nil && !changed && !rebind && (code == "NETWORK_UNAVAILABLE" || retryWiFiFailure(w)) {
		m.wifiAttemptFailedLocked(id, w, time.Now())
	}
}

// Retry transport/worker failures, never operator rejection or uncertain cleanup.
func retryWiFiFailure(w *moduleWiFi) bool {
	if w == nil || !w.Enabled || w.State != "failed" {
		return false
	}
	if w.Issue == "WIFI_WORKER_UNAVAILABLE" {
		return true
	}
	if w.Issue != "WIFI_CONNECTION_FAILED" {
		return false
	}
	switch w.Diagnostic {
	case "tunnel-other", "tunnel-timeout", "tunnel-closed", "tunnel-eof", "tunnel-reset", "ims-timeout", "ims-closed", "ims-eof", "ims-reset", "ims-expired":
		return true
	}
	return false
}

// eSIM jobs wait for the existing module gate; no write races RF cleanup.
// Persistence has committed before applying the stop in memory.
func (m *moduleManager) stopWiFiLocked(id int64) {
	w := m.wifi[id]
	if w == nil || !w.Enabled && !w.running {
		return
	}
	w.Enabled = false
	w.setRegistered(false, time.Now())
	if w.running {
		w.State = "stopping"
		w.cancel()
	} else {
		w.State = "off"
	}
}

// Called under m.mu only after confirmed device recovery or a new USB generation.
func resumeWiFiIntent(w *moduleWiFi, r hardware.Reading) bool {
	if w == nil || !w.Enabled || w.running || w.ICCID != r.ICCID || r.SIM != "READY" || !hardware.CommunicationHealthy(r) || (w.State != "failed" && w.State != "off") {
		return false
	}
	w.State, w.Issue = "waiting", ""
	w.setRegistered(false, time.Now())
	w.Profile, w.Transport = "", ""
	return true
}

type radioRestorer interface {
	RestoreRadio(context.Context, hardware.Candidate, string, string) error
}
type radioRestoreSource struct{ radioRestorer }

func (r radioRestoreSource) WiFi(ctx context.Context, c hardware.Candidate, identity, iccid string, _ func(string)) error {
	return r.RestoreRadio(ctx, c, identity, iccid)
}
