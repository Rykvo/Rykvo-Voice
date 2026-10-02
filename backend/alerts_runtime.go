package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

type healthObservation struct {
	Module      int64
	Reason, Key string
	Good        bool
	At          time.Time
}
type healthWindow struct {
	Since time.Time
	Card  string
	WiFi  bool
}

const wifiAlertGrace = 5 * time.Minute

func wifiAlertReason(reason string) bool {
	return reason == "WIFI_REGISTRATION_FAILED" || reason == "WIFI_ROAMING_RESTRICTED"
}

func (m *moduleManager) alertHealth(now time.Time, windows map[int64]healthWindow) []healthObservation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.ready || now.Sub(m.lastScan) > 20*time.Second {
		return nil
	}
	var result []healthObservation
	for id, sample := range m.values {
		current, present := m.seen[sample.Candidate.Key]
		if !present || !sameEndpoint(current, sample.Candidate) {
			delete(windows, id)
			continue
		}
		w := m.wifi[id]
		wifi := w != nil && w.ICCID == sample.Reading.ICCID && w.Enabled
		// A short successful registration between polls ends the old failure streak.
		if window := windows[id]; wifi && !w.registeredAt.IsZero() && ((!w.Registered && window.Since.IsZero()) || window.WiFi && w.registeredAt.After(window.Since)) {
			delete(windows, id)
			result = append(result, healthObservation{id, "", fmt.Sprintf("wifi-recovered:%d:%d", id, w.registeredAt.UnixNano()), true, w.registeredAt})
		}
		busy := len(m.gates[sample.Candidate.Key]) > 0 && !(w != nil && w.running)
		if busy || m.jobs[id].active() || now.Before(m.recoveryUntil[sample.Candidate.Key]) || w != nil && now.Before(w.refreshUntil) {
			continue
		}
		reading := sample.Reading
		reason := reading.Issue
		if reason == "OPERATION_ACTIVE" || reason == "READING" || reason == "RECOVERING" {
			continue
		}
		if reason == "" && !reading.Responsive {
			reason = "DEVICE_UNAVAILABLE"
		}
		stale := now.Sub(reading.UpdatedAt) > 90*time.Second
		if reason == "" && stale && !(w != nil && w.running) {
			reason = "STATE_STALE"
		}
		dynamic := reason == "STATE_STALE" || reason == "IDENTITY_CONFLICT"
		if reason == "" && wifi {
			dynamic = true
			if !w.Registered {
				reason = "WIFI_REGISTRATION_FAILED"
				if w.Issue == "WIFI_ROAMING_RESTRICTED" {
					reason = w.Issue
				}
			}
		}
		key := fmt.Sprintf("health:%d:%s:%s", id, reading.UpdatedAt.UTC().Format(time.RFC3339Nano), reason)
		if dynamic {
			key = fmt.Sprintf("health:%d:%d:%s", id, now.Unix()/30, reason)
		}
		if reason != "" {
			window := windows[id]
			if window.Since.IsZero() || window.Card != reading.ICCID || window.WiFi != wifiAlertReason(reason) {
				windows[id] = healthWindow{Since: now, Card: reading.ICCID, WiFi: wifiAlertReason(reason)}
				continue
			}
			grace := 90 * time.Second
			if wifiAlertReason(reason) {
				grace = wifiAlertGrace
			}
			if now.Sub(window.Since) < grace {
				continue
			}
		} else {
			delete(windows, id)
		}
		// A SIM read error is not proof of a modem failure. No-card is not an alarm.
		if strings.HasPrefix(reason, "CARD_") || reason == "NO_SIM" || reason == "SIM_NOT_READY" {
			continue
		}
		if wifi && wifiAlertReason(reason) && w.recoveryPending(m.wifiRetryLimit, now) {
			continue
		}
		result = append(result, healthObservation{id, reason, key, reason == "", now})
	}
	return result
}

// Recheck live registration immediately before delivery; unknown is not failure.
func (m *moduleManager) wifiAlertDelivery(id int64, now time.Time) string {
	if m == nil {
		return "pending"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sample, exists := m.values[id]
	current, present := m.seen[sample.Candidate.Key]
	if !m.ready || now.Sub(m.lastScan) > 20*time.Second || !exists || !present || !sameEndpoint(current, sample.Candidate) {
		return "pending"
	}
	w := m.wifi[id]
	if w == nil || !w.Enabled || w.ICCID != sample.Reading.ICCID || w.Registered {
		return "cancelled"
	}
	if w.unregisteredSince.IsZero() || now.Sub(w.unregisteredSince) < wifiAlertGrace || m.jobs[id].active() || now.Before(m.recoveryUntil[sample.Candidate.Key]) || now.Before(w.refreshUntil) || w.recoveryPending(m.wifiRetryLimit, now) {
		return "pending"
	}
	return "sending"
}

func (s *server) runAlerts(ctx context.Context) {
	if s.db == nil {
		return
	}
	// A process interruption after submission has an unknown delivery outcome.
	for {
		call, stop := context.WithTimeout(ctx, 3*time.Second)
		_, err := s.db.Exec(call, "UPDATE alert_notifications SET state='unknown',issue='DELIVERY_UNCONFIRMED' WHERE state='sending'")
		stop()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); s.runAlertDelivery(ctx) }()
	defer func() { <-done }()
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	windows := map[int64]healthWindow{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			if s.modules == nil {
				continue
			}
			call, stop := context.WithTimeout(ctx, 3*time.Second)
			var limit int
			err := s.db.QueryRow(call, "SELECT module FROM alert_settings").Scan(&limit)
			stop()
			if err != nil {
				continue
			}
			s.modules.recoverWiFi(limit, now)
			for _, o := range s.modules.alertHealth(now, windows) {
				call, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, err := s.db.Exec(call, "SELECT alert_observe($1,0,'module',$2,$3,$4,$5)", o.Module, o.Key, o.Good, o.Reason, o.At)
				cancel()
				if err != nil && ctx.Err() == nil {
					log.Print("Module alert observation failed")
				}
			}

		}
	}
}

func alertReason(kind, code string) string {
	switch kind {
	case "sip":
		return "SIP呼出失败"
	case "sms":
		return "短信发送异常"
	case "mms":
		return "彩信发送异常"
	}
	reasons := map[string]string{
		"READ_TIMEOUT": "设备读取超时", "DEVICE_UNAVAILABLE": "设备无响应", "AT_PORT_MISSING": "未找到 AT 串口",
		"PERMISSION_DENIED": "设备访问权限不足", "DEVICE_BUSY": "设备持续被占用", "STATE_STALE": "状态持续未更新",
		"QMI_UNAVAILABLE": "QMI 读取工具未就绪", "QMI_READ_FAILED": "模块状态读取失败", "QMI_STATUS_FAILED": "网络状态读取失败",
		"PCSC_UNAVAILABLE": "读卡器驱动未就绪", "IDENTITY_CONFLICT": "设备身份冲突",
		"VOICE_AUDIO_RESTORE_PENDING": "音频配置恢复未完成", "VOICE_MIC_CONFIG_FAILED": "外接麦克风关闭配置失败",
		"WIFI_REGISTRATION_FAILED": "Wi-Fi 通话未注册",
		"WIFI_ROAMING_RESTRICTED":  "运营商限制漫游 Wi-Fi 通话",
	}
	reason := reasons[code]
	if reason == "" {
		reason = "原因未确认"
	}
	return reason
}
