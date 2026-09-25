package hardware

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"rykvo.local/auth/internal/vocat/device"
	"rykvo.local/auth/internal/vocat/vowifi"
	"strconv"
	"strings"
	"time"
)

func (s *System) cellSMS(ctx context.Context, c Candidate, identity, card string) (*atSession, error) {
	found, e := s.Discover(ctx)
	if e != nil {
		return nil, e
	}
	valid := false
	for _, v := range found {
		if v.Key == c.Key && v.Generation == c.Generation {
			c = v
			valid = true
			break
		}
	}
	if !valid {
		return nil, errors.New("DEVICE_CHANGED")
	}
	at, e := openWiFiSession(ctx, c, identity)
	if e != nil {
		return nil, e
	}
	a := &vocatAT{session: at, device: c.Key, iccid: card}
	if e = a.verify(ctx); e != nil {
		at.port.Close()
		return nil, e
	}
	mode, e := wifiRadioMode(ctx, at)
	if e != nil || mode != 1 {
		at.port.Close()
		return nil, errors.New("SMS_NOT_READY")
	}
	if _, e = at.query(ctx, "AT+CMGF=0"); e != nil {
		at.port.Close()
		return nil, e
	}
	return at, nil
}
func smsATWrite(ctx context.Context, at *atSession, data []byte) error {
	for len(data) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, e := at.port.Write(data)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func smsATRead(ctx context.Context, at *atSession, prompt bool) (int, error) {
	reference := -1
	size := 0
	for {
		if ctx.Err() != nil {
			return -1, errors.New("SMS_OUTCOME_UNKNOWN")
		}
		if prompt {
			if i := strings.Index(at.buffer, ">"); i >= 0 {
				at.buffer = at.buffer[i+1:]
				return 0, nil
			}
		}
		if i := strings.IndexAny(at.buffer, "\r\n"); i >= 0 {
			line := strings.TrimSpace(at.buffer[:i])
			at.buffer = at.buffer[i+1:]
			if line == "ERROR" || strings.HasPrefix(line, "+CMS ERROR") || strings.HasPrefix(line, "+CME ERROR") {
				return -1, errors.New("SMS_REJECTED")
			}
			if strings.HasPrefix(line, "+CMGS:") {
				v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "+CMGS:")))
				if err != nil || v < 0 || v > 255 {
					return -1, errors.New("SMS_OUTCOME_UNKNOWN")
				}
				reference = v
			}
			if line == "OK" && !prompt && reference >= 0 && reference <= 255 {
				return reference, nil
			}
			continue
		}
		b := make([]byte, 1024)
		n, e := at.port.Read(b)
		if e != nil {
			return -1, errors.New("SMS_OUTCOME_UNKNOWN")
		}
		at.buffer += string(b[:n])
		size += n
		if size > 8192 {
			return -1, errors.New("SMS_OUTCOME_UNKNOWN")
		}
	}
}
func (s *System) SendCellularSMS(ctx context.Context, c Candidate, identity, card, to, text string) (vowifi.SMSSubmitResult, error) {
	result := vowifi.SMSSubmitResult{To: to}
	if !ValidSMS(to, text) {
		return result, errors.New("INVALID_MESSAGE")
	}
	parts, e := device.PrepareSMSSubmitTPDUs(to, text)
	if e != nil || len(parts) > 32 {
		return result, errors.New("INVALID_MESSAGE")
	}
	result.PartsTotal = len(parts)
	at, e := s.cellSMS(ctx, c, identity, card)
	if e != nil {
		return result, errors.New("SMS_NOT_READY")
	}
	defer at.port.Close()
	for _, p := range parts {
		call, cancel := context.WithTimeout(ctx, 45*time.Second)
		command := fmt.Sprintf("AT+CMGS=%d\r", len(p.TPDU))
		e = smsATWrite(call, at, []byte(command))
		if e == nil {
			_, e = smsATRead(call, at, true)
		}
		if e != nil {
			// Abort text-entry mode before any TPDU has been submitted. Never send Ctrl-Z here.
			abort, stop := context.WithTimeout(context.Background(), time.Second)
			_ = smsATWrite(abort, at, []byte{27})
			stop()
			cancel()
			return result, e
		}
		result.PartsAttempted++
		atTime := time.Now().UTC()
		result.SubmittedAt = atTime
		e = smsATWrite(call, at, append([]byte("00"+strings.ToUpper(hex.EncodeToString(p.TPDU))), 26))
		ref := -1
		if e == nil {
			ref, e = smsATRead(call, at, false)
		}
		cancel()
		accepted := e == nil
		r := vowifi.SMSSubmitPart{Part: p.Part, Total: p.Total, Reference: ref, Accepted: accepted, SubmittedAt: atTime}
		if accepted {
			result.PartsAccepted++
			r.SubmissionStatus = "accepted"
		}
		result.PartResults = append(result.PartResults, r)
		if e != nil {
			return result, e
		}
	}
	result.AllPartsAccepted = result.PartsAccepted == result.PartsTotal
	result.SubmissionStatus = "accepted"
	return result, nil
}
func (s *System) ReadCellularSMS(ctx context.Context, c Candidate, identity, card string, store func(context.Context, SMSDelivery) error) error {
	at, e := s.cellSMS(ctx, c, identity, card)
	if e != nil {
		return e
	}
	defer at.port.Close()
	// Received SMS may be in ME even when the current read store is SM.
	config, e := at.query(ctx, "AT+CPMS?")
	if e != nil {
		return e
	}
	original := ""
	for _, line := range config {
		if strings.HasPrefix(line, "+CPMS:") {
			f := fields(line)
			if len(f) > 0 {
				original = f[0]
			}
		}
	}
	if original != "SM" && original != "ME" && original != "MT" {
		return errors.New("SMS_STORAGE_UNAVAILABLE")
	}
	changed := false
	defer func() {
		if changed {
			restore, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = at.query(restore, `AT+CPMS="`+original+`"`)
		}
	}()
	stores := []string{original}
	for _, name := range []string{"SM", "ME"} {
		if name != original {
			stores = append(stores, name)
		}
	}
	for _, name := range stores {
		if name != original || changed {
			if _, e = at.query(ctx, `AT+CPMS="`+name+`"`); e != nil {
				if e.Error() == "COMMAND_UNSUPPORTED" {
					continue
				}
				return e
			}
			changed = true
		}
		lines, e := at.exchange(ctx, "AT+CMGL=4", 8*time.Second)
		if e != nil {
			return e
		}
		if e = storeCellularPDU(ctx, lines, store); e != nil {
			return e
		}
	}
	return nil
}
func storeCellularPDU(ctx context.Context, lines []string, store func(context.Context, SMSDelivery) error) error {
	// Never delete SIM storage as part of polling. Deduplication is durable by TPDU.
	for i, line := range lines {
		if !strings.HasPrefix(line, "+CMGL:") || i+1 >= len(lines) {
			continue
		}
		pdu, e := hex.DecodeString(strings.TrimSpace(lines[i+1]))
		if e != nil || len(pdu) < 2 {
			continue
		}
		offset := int(pdu[0]) + 1
		if offset >= len(pdu) {
			continue
		}
		tpdu := pdu[offset:]
		v, e := device.DecodeSMSDeliverTPDU(tpdu)
		if v.SIMDataDownload || v.Direction != device.SMSDirectionReceived && v.Direction != device.SMSDirectionStatusReport {
			continue
		}
		d := SMSDelivery{ID: "cellular", From: v.From, Text: v.Text, At: time.Now().UTC(), SCTS: v.ServiceCenterTimestamp, Encoding: string(v.Encoding), Concat: v.Concat, TPDU: strings.ToUpper(hex.EncodeToString(tpdu))}
		if e != nil {
			d.DecodeError = "DECODE_FAILED"
		}
		if v.MessageReference != nil && v.StatusCode != nil {
			d.From = v.To
			d.Status = &SMSReport{Reference: *v.MessageReference, Code: *v.StatusCode}
		}
		if e = store(ctx, d); e != nil {
			return e
		}
	}
	return nil
}
