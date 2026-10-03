package ims

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"time"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func (session *Session) processSIMDownload(request *sipRequest, rpdu rpMessage, payload []byte, pid, dcs byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(request.value("Call-ID") + "\x00" + request.value("CSeq") + "\x00"))
	hash.Write(payload)
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	session.simDownloadMu.Lock()
	defer session.simDownloadMu.Unlock()
	if cached, ok := session.simDownloadReports[key]; ok {
		return bytes.Clone(cached)
	}
	var result vowifi.SMSPPResult
	var downloadErr error
	if session.provider.config.OnSIMDataDownload == nil {
		downloadErr = errors.New("ims: UICC download handler unavailable")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		download := SIMDataDownload{DeviceID: session.request.DeviceID, IMSI: session.request.Identity.IMSI,
			PID: pid, DCS: dcs, TPDU: bytes.Clone(rpdu.tpdu), RPDU: bytes.Clone(payload)}
		result, downloadErr = session.provider.config.OnSIMDataDownload(ctx, download)
		clear(download.TPDU)
		clear(download.RPDU)
		cancel()
	}
	report, success := simDownloadReport(rpdu.reference, pid, dcs, result, downloadErr)
	clear(result.Data)
	if !success {
		session.logInboundSMS(slog.LevelWarn, "IMS SIM data download failed", request,
			"stage", "uicc", "rp_reference", int(rpdu.reference), "uicc_status", result.Status)
	}
	if session.simDownloadReports == nil {
		session.simDownloadReports = make(map[[32]byte][]byte)
	}
	if len(session.simDownloadOrder) == 64 {
		old := session.simDownloadOrder[0]
		clear(session.simDownloadReports[old])
		delete(session.simDownloadReports, old)
		session.simDownloadOrder = session.simDownloadOrder[1:]
	}
	session.simDownloadReports[key] = bytes.Clone(report)
	session.simDownloadOrder = append(session.simDownloadOrder, key)
	return report
}

func simDownloadReport(ref, pid, dcs byte, result vowifi.SMSPPResult, err error) ([]byte, bool) {
	if err != nil {
		result.Data = nil
	}
	success := err == nil && (result.Status == 0x9000 || result.Status>>8 == 0x91)
	fcs := byte(0xd5) // USIM data download error, not memory capacity exceeded.
	if err == nil && result.Status == 0x9300 {
		fcs = 0xd4 // USIM application toolkit busy.
	}
	if len(result.Data) > 140 {
		success = false
		result.Data = nil
	}
	report := []byte{0x02, ref}
	if !success {
		report = buildRPError(ref, 111)
	}
	if success && len(result.Data) == 0 {
		return report, true
	}
	tpdu := []byte{0}
	if !success {
		tpdu = append(tpdu, fcs)
	}
	if len(result.Data) == 0 {
		tpdu = append(tpdu, 0)
	} else {
		udl := len(result.Data)
		if dcs&0x8c == 0 || dcs&0xf4 == 0xf0 {
			udl = udl * 8 / 7
		}
		tpdu = append(tpdu, 7, pid, dcs, byte(udl))
		tpdu = append(tpdu, result.Data...)
	}
	report = append(report, 0x41, byte(len(tpdu)))
	report = append(report, tpdu...)
	clear(tpdu)
	return report, success
}

// A successful durable receipt proves storage is available. Notify the same
// authenticated gateway once per session, including after older false-full errors.
func (session *Session) notifySMSMemoryAvailable(request *sipRequest) {
	if !session.provider.config.NotifySMSMemoryAvailable {
		return
	}
	target := firstURI(request.value("P-Asserted-Identity"))
	if target == "" {
		target = firstURI(request.value("From"))
	}
	if target == "" {
		return
	}
	session.mu.Lock()
	if session.smsMemoryNoticeStarted || session.closed || !session.evidence.Registered || !session.smsCapabilityReady() {
		session.mu.Unlock()
		return
	}
	session.smsMemoryNoticeStarted = true
	parent := session.refreshContext
	session.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	preferred := request.value("P-Called-Party-ID")
	go func() {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		ref, pending, err := session.beginRP()
		if err == nil {
			defer session.endRP(ref)
			var response *sipResponse
			response, err = session.sendSIPMessageWithIdentity(ctx, target, []byte{6, ref}, "", smsContentType, "smsip", preferred)
			if err == nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
				_, err = session.awaitRP(ctx, pending)
			} else if err == nil {
				err = errors.New("ims: memory-available notification rejected")
			}
		}
		level, stage := slog.LevelInfo, "memory_available_confirmed"
		if err != nil {
			level, stage = slog.LevelWarn, "memory_available_unconfirmed"
		}
		session.logInboundSMS(level, "IMS SMS memory availability notification", nil, "stage", stage)
	}()
}
