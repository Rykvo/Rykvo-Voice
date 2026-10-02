package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/carrierconfig"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/mms"
	"rykvo.local/auth/internal/vocat/device"
	"rykvo.local/auth/internal/vocat/vowifi"
)

type cellularMessageTransport interface {
	SendCellularSMS(context.Context, hardware.Candidate, string, string, string, string) (vowifi.SMSSubmitResult, error)
	ReadCellularSMS(context.Context, hardware.Candidate, string, string, func(context.Context, hardware.SMSDelivery) error) error
}
type cellularMMSTransport interface {
	SendCellularMMS(context.Context, hardware.Candidate, string, string, carrierconfig.Profile, string, string, string, *mms.Part) (hardware.MMSSubmitResult, error)
	ReceiveCellularMMS(context.Context, hardware.Candidate, string, string, carrierconfig.Profile, string, string, func(mms.PDU) error) error
}

type wifiMMSTransport interface {
	SendWiFiMMS(context.Context, hardware.Candidate, string, carrierconfig.Profile, string, string, string, *mms.Part) (string, error)
	ReceiveWiFiMMS(context.Context, hardware.Candidate, string, string, carrierconfig.Profile, string, string, func(mms.PDU) error) error
}

func cellularRegistered(r hardware.Reading) bool {
	return r.Registration == "home" || r.Registration == "roaming" || r.Registration == "registered"
}

type smsTransport interface {
	SendSMS(context.Context, hardware.Candidate, string, string, string, string) (vowifi.SMSSubmitResult, error)
}

func (m *moduleManager) reserveIncomingSMS(c hardware.Candidate, card string) func() {
	m.mu.RLock()
	var sample moduleSample
	for id, value := range m.values {
		w := m.wifi[id]
		if value.Candidate.Key == c.Key && sameEndpoint(c, value.Candidate) && value.Reading.ICCID == card && w != nil && w.running && w.ICCID == card {
			sample = value
			break
		}
	}
	m.mu.RUnlock()
	return m.reserveModuleWork(sample)
}

func (m *moduleManager) receiveSMS(ctx context.Context, c hardware.Candidate, card string, v hardware.SMSDelivery) error {
	var module int64
	var line string
	m.mu.RLock()
	for id, s := range m.values {
		w := m.wifi[id]
		if sameEndpoint(c, s.Candidate) && s.Reading.ICCID == card && w != nil && w.ICCID == card && w.running {
			module = id
			line = wifiLine(s.Reading)
			break
		}
	}
	m.mu.RUnlock()
	if module == 0 || line == "" {
		return errors.New("DEVICE_CHANGED")
	}
	return m.storeIncoming(ctx, module, line, card, v)
}
func (m *moduleManager) storeIncoming(ctx context.Context, module int64, line, card string, v hardware.SMSDelivery) error {
	if len(v.TPDU) > 1024 || len(v.Text) > 4096 || len(v.From) > 128 || v.From == "" {
		return errors.New("INVALID_MESSAGE")
	}
	raw, e := hex.DecodeString(v.TPDU)
	if e != nil || len(raw) == 0 {
		return errors.New("INVALID_MESSAGE")
	}
	fingerprint := messageDigest(card + "\x00" + v.TPDU)
	tx, e := m.db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", card); e != nil {
		return e
	}
	if v.Status != nil {
		if v.Status.Reference < 0 || v.Status.Reference > 255 || v.Status.Code < 0 || v.Status.Code > 255 {
			return errors.New("INVALID_MESSAGE")
		}
		_, e = tx.Exec(ctx, `INSERT INTO message_reports(iccid,fingerprint,peer,reference,status,scts) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, card, fingerprint, v.From, v.Status.Reference, v.Status.Code, v.SCTS)
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	var existing string
	var forgotten bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM message_fingerprints WHERE iccid=$1 AND fingerprint=$2 AND expires_at>now())", card, fingerprint).Scan(&forgotten); e != nil {
		return e
	}
	if forgotten {
		return nil
	}
	e = tx.QueryRow(ctx, "SELECT message_id FROM message_parts WHERE iccid=$1 AND fingerprint=$2", card, fingerprint).Scan(&existing)
	if e == nil {
		return nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	id := "rx-" + fingerprint
	state := "received"
	kind := "sms"
	body := v.Text
	seq, total := 1, 1
	metadata := map[string]any{}
	binary := false
	decoded, de := device.DecodeSMSDeliverTPDU(raw)
	if de == nil && decoded.Encoding == device.SMSEncoding8BitPDU {
		ud, e := hex.DecodeString(decoded.RawUserData)
		if e == nil && len(ud) > 0 && raw[0]&0x40 != 0 {
			n := int(ud[0]) + 1
			if n <= len(ud) {
				for i := 1; i+1 < n; {
					tag, l := ud[i], int(ud[i+1])
					i += 2
					if i+l > n {
						break
					}
					if tag == 5 && l == 4 && int(ud[i])<<8|int(ud[i+1]) == 2948 {
						binary = true
					}
					i += l
				}
				if binary || v.Concat != nil {
					body = hex.EncodeToString(ud[n:])
				}
			}
		}
	}
	if v.Concat != nil {
		seq, total = v.Concat.Sequence, v.Concat.Total
		if seq < 1 || total < 1 || total > 32 || seq > total || v.Concat.Reference < 0 || v.Concat.Reference > 65535 {
			return errors.New("INVALID_MESSAGE")
		}
		metadata = map[string]any{"reference": v.Concat.Reference, "total": total, "encoding": v.Encoding, "binary": binary}
		meta, _ := json.Marshal(metadata)
		e = tx.QueryRow(ctx, `SELECT id FROM messages WHERE iccid=$1 AND peer=$2 AND NOT mine AND state='receiving' AND metadata->>'reference'=($3::jsonb)->>'reference' AND metadata->>'total'=($3::jsonb)->>'total' AND metadata->>'encoding'=($3::jsonb)->>'encoding' AND created_at>now()-interval '10 minutes' ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, card, v.From, meta).Scan(&existing)
		if e == nil {
			var previous []byte
			if e = tx.QueryRow(ctx, "SELECT metadata FROM messages WHERE id=$1", existing).Scan(&previous); e != nil {
				return e
			}
			var old map[string]any
			_ = json.Unmarshal(previous, &old)
			if old["binary"] == true {
				binary = true
				metadata["binary"] = true
			}
			id = existing
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		// Conflicting fragments are kept as separate partial messages, never overwritten.
		var occupied bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM message_parts WHERE message_id=$1 AND sequence=$2)", id, seq).Scan(&occupied); e != nil {
			return e
		}
		if occupied {
			id = "rx-" + fingerprint
		}
		state = "receiving"
	}
	if v.DecodeError != "" {
		state = "decode_error"
	}
	// The insert itself emits a webhook: never publish a received placeholder.
	initialBody := body
	if binary {
		initialBody, kind, state = "", "mms", "receiving"
	}
	meta, _ := json.Marshal(metadata)
	_, e = tx.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,metadata) VALUES($1,$2,$3,$4,$5,false,$6,$9,$7,$8) ON CONFLICT(id) DO NOTHING`, id, module, card, line, v.From, kind, state, meta, initialBody)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO message_parts(iccid,fingerprint,message_id,sequence,body,raw_tpdu) VALUES($1,$2,$3,$4,$5,$6)`, card, fingerprint, id, seq, body, v.TPDU)
	if e != nil {
		return e
	}
	var count int
	var assembled string
	e = tx.QueryRow(ctx, "SELECT count(*),string_agg(body,'' ORDER BY sequence) FROM message_parts WHERE message_id=$1", id).Scan(&count, &assembled)
	if e != nil {
		return e
	}
	if count == total && v.DecodeError == "" {
		state = "received"
	}
	if binary {
		kind = "mms"
		if count == total {
			var peer string
			state, peer, metadata = decodeMMSPush(assembled, v.From)
			meta, _ = json.Marshal(metadata)
			if _, e = tx.Exec(ctx, "UPDATE messages SET peer=$2 WHERE id=$1", id, peer); e != nil {
				return e
			}
		}
		assembled = ""
	}
	_, e = tx.Exec(ctx, `UPDATE messages SET body=$2,state=$3,kind=$4,metadata=$5 WHERE id=$1
	 AND (body,state,kind,metadata) IS DISTINCT FROM ($2::text,$3::text,$4::text,$5::jsonb)`, id, assembled, state, kind, meta)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Queue briefly behind this module's polling; never spin or hold a global lock.
func (m *moduleManager) waitMessageGate(parent context.Context, key string) func() {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	gate := m.gate(key)
	select {
	case gate <- struct{}{}:
		if ctx.Err() != nil {
			<-gate
			return nil
		}
		return func() { <-gate }
	case <-ctx.Done():
		return nil
	}
}

func (m *moduleManager) processMessage(parent context.Context, module int64) {
	ctx, cancel := context.WithTimeout(parent, 480*time.Second)
	defer cancel()
	// Waiting work is retried only before any network submission is known to occur.
	var id, card, line, to, text, rawImage, kind, state string
	var metadata []byte
	var created time.Time
	var expires *time.Time
	var epoch int64
	var currentEpoch *int64
	read, stopRead := context.WithTimeout(ctx, 3*time.Second)
	e := m.db.QueryRow(read, `SELECT id,iccid,line_id,peer,body,image,kind,state,metadata,created_at,expires_at,alert_epoch,
 (SELECT card_epoch FROM modules WHERE modules.id=messages.module_id AND active_card=messages.iccid)
 FROM messages WHERE module_id=$1 AND deleted_at IS NULL AND (state IN ('queued','download_pending') OR (state='waiting_network' AND updated_at<now()-interval '30 seconds')) ORDER BY `+messagePriorityOrder+` LIMIT 1`, module).Scan(&id, &card, &line, &to, &text, &rawImage, &kind, &state, &metadata, &created, &expires, &epoch, &currentEpoch)
	stopRead()
	if e != nil || m.messageResults.Pending(id) {
		return
	}
	if time.Since(created) > 24*time.Hour || expires != nil && !expires.After(time.Now()) {
		terminal := "failed"
		if strings.HasPrefix(id, "rx-") {
			terminal = "expired"
		}
		m.messageState(ctx, id, terminal, "MESSAGE_EXPIRED", nil)
		return
	}
	if epoch == 0 || currentEpoch == nil || *currentEpoch != epoch {
		m.messageState(ctx, id, "cancelled", "DEVICE_CHANGED", nil)
		return
	}
	m.mu.RLock()
	sample, ok := m.values[module]
	current, present := m.seen[sample.Candidate.Key]
	valid := m.ready && ok && present && sameEndpoint(current, sample.Candidate) && sample.Reading.ICCID == card && wifiLine(sample.Reading) == line && !m.jobs[module].active() && time.Since(m.lastScan) < 20*time.Second
	profile := m.carrierConfigs[card]
	wifi := m.wifi[module]
	useWiFi := wifi != nil && (wifi.Enabled || wifi.running || wifi.State == "stopping")
	wifiRegistered := wifi != nil && wifi.Enabled && wifi.Registered
	roamingAllowed := m.roamingReady && m.roaming[card]
	ready := valid && ((useWiFi && wifi.Enabled && wifi.Registered && wifi.SMSReady) || (!useWiFi && cellularRegistered(sample.Reading)))
	m.mu.RUnlock()
	if !valid {
		m.messageState(ctx, id, "waiting_network", "DEVICE_CHANGED", nil)
		return
	}
	if kind == "sms" && !ready {
		m.messageState(ctx, id, "waiting_network", "SMS_NOT_READY", nil)
		return
	}
	choice := profile.MMSForBearer(useWiFi, sample.Reading.Registration == "roaming")
	if kind == "mms" && (choice.Status != "matched" || choice.Profile == nil) {
		m.messageState(ctx, id, "waiting_network", "MMS_CONFIG_REQUIRED", nil)
		return
	}
	if kind == "mms" && useWiFi && !wifiRegistered {
		m.messageState(ctx, id, "waiting_network", "MMS_NETWORK_REQUIRED", nil)
		return
	}
	if kind == "mms" && !useWiFi && (!cellularRegistered(sample.Reading) || sample.Reading.Registration == "roaming" && !roamingAllowed) {
		issue := "MMS_NETWORK_REQUIRED"
		if sample.Reading.Registration == "roaming" {
			issue = "MMS_ROAMING_DISABLED"
		}
		m.messageState(ctx, id, "waiting_network", issue, nil)
		return
	}
	// CAS also honors a cancellation that happened after selecting this job.
	next := "sending"
	if strings.HasPrefix(id, "rx-") {
		next = "downloading"
	}
	releaseWork := m.reserveModuleWork(sample)
	if releaseWork == nil {
		return
	}
	defer releaseWork()
	if !useWiFi {
		release := m.waitMessageGate(ctx, sample.Candidate.Key)
		if release == nil {
			m.messageState(ctx, id, "waiting_network", strings.ToUpper(kind)+"_BUSY", nil)
			return
		}
		defer release()
	}
	if time.Since(created) > 24*time.Hour || expires != nil && !expires.After(time.Now()) {
		terminal := "failed"
		if strings.HasPrefix(id, "rx-") {
			terminal = "expired"
		}
		m.messageState(ctx, id, terminal, "MESSAGE_EXPIRED", nil)
		return
	}
	claim, stopClaim := context.WithTimeout(ctx, 3*time.Second)
	tag, e := m.db.Exec(claim, `UPDATE messages SET state=$2,issue='',operation_at=CASE WHEN $2='sending' THEN clock_timestamp() ELSE operation_at END WHERE id=$1 AND state=$3 AND deleted_at IS NULL
 AND alert_epoch=(SELECT card_epoch FROM modules WHERE modules.id=messages.module_id AND active_card=messages.iccid)`, id, next, state)
	stopClaim()
	if e != nil || tag.RowsAffected() != 1 {
		return
	}
	if kind == "sms" {
		var result vowifi.SMSSubmitResult
		var err error
		if useWiFi {
			sender, ok := m.wifiEngine.(smsTransport)
			if !ok {
				m.messageState(ctx, id, "waiting_network", "SMS_NOT_READY", nil)
				return
			}
			result, err = sender.SendSMS(ctx, sample.Candidate, card, id, to, text)
		} else {
			sender, ok := m.source.(cellularMessageTransport)
			if !ok {
				m.messageState(ctx, id, "waiting_network", "SMS_NOT_READY", nil)
				return
			}
			m.mu.RLock()
			w := m.wifi[module]
			busy := w != nil && (w.Enabled || w.running) || m.jobs[module].active()
			m.mu.RUnlock()
			if busy {
				m.messageState(ctx, id, "waiting_network", "SMS_NOT_READY", nil)
				return
			}
			result, err = sender.SendCellularSMS(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), card, to, text)
		}
		status, issue := "unknown", "SMS_OUTCOME_UNKNOWN"
		if err != nil && (err.Error() == "SMS_NOT_READY" || err.Error() == "SMS_BUSY") {
			status, issue = "waiting_network", err.Error()
		} else if err != nil && err.Error() == "SMS_SMSC_UNAVAILABLE" {
			status, issue = "failed", err.Error()
		} else if result.AllPartsAccepted && result.PartsTotal > 0 {
			status, issue = "accepted", ""
		} else if result.PartsAccepted > 0 {
			status, issue = "partial", "SMS_PARTIAL"
		} else if (err != nil && err.Error() == "SMS_REJECTED") || result.PartsAttempted > 0 && err == nil {
			status, issue = "failed", "SMS_REJECTED"
		}
		finish, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		m.messageState(finish, id, status, issue, result)
		return
	}
	mmsProfile := *choice.Profile
	var native cellularMMSTransport
	if !useWiFi {
		native, _ = m.source.(cellularMMSTransport)
		if native == nil {
			m.messageState(ctx, id, "failed", "MMS_MODEM_UNSUPPORTED", nil)
			return
		}
		m.mu.RLock()
		w := m.wifi[module]
		busy := !m.ready || m.jobs[module].active() || w != nil && (w.Enabled || w.running || w.State == "stopping")
		m.mu.RUnlock()
		if busy {
			m.messageState(ctx, id, "waiting_network", "MMS_BUSY", nil)
			return
		}
	}
	if next == "downloading" {
		var meta struct {
			Location         string `json:"location"`
			Transaction      string `json:"transaction"`
			DownloadAttempts int    `json:"downloadAttempts"`
		}
		if json.Unmarshal(metadata, &meta) != nil {
			m.messageState(ctx, id, "failed", "MMS_INVALID_NOTIFICATION", nil)
			return
		}
		receivedOutcome := false
		persist := func(p mms.PDU) error {
			body, img := "", ""
			for _, part := range p.Parts {
				if part.Type == "text/plain" {
					if len(body)+len(part.Data) <= 4096 {
						body += string(part.Data)
					}
				} else if img == "" {
					candidate := "data:" + part.Type + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
					if _, err := messageImage(candidate); err == nil {
						img = candidate
					}
				}
			}
			if body == "" && img == "" {
				return errors.New("MMS_UNSUPPORTED_CONTENT")
			}
			peer := mmsPeer(p.From, to)
			finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			receivedOutcome = true
			return m.saveMessageOutcome(finish, messageOutcome{ID: id, State: "received", Body: &body, Image: &img, Peer: &peer})
		}
		if native != nil {
			e = native.ReceiveCellularMMS(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), card, mmsProfile, meta.Location, meta.Transaction, persist)
		} else if transport, ok := m.wifiEngine.(wifiMMSTransport); ok {
			e = transport.ReceiveWiFiMMS(ctx, sample.Candidate, card, id, mmsProfile, meta.Location, meta.Transaction, persist)
		} else {
			e = mms.ErrNetwork
		}

		if e != nil && !receivedOutcome {
			finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = m.failMMSDownload(finish, id, e, meta.DownloadAttempts)
		}
		return
	}
	part, e := messageImage(rawImage)
	if e != nil {
		m.messageState(ctx, id, "failed", "INVALID_IMAGE", nil)
		return
	}
	if native != nil {
		result, err := native.SendCellularMMS(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), card, mmsProfile, id, to, text, part)
		status, issue := nativeMMSState(result, err)
		finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		m.messageState(finish, id, status, issue, result)
		return
	}
	var networkID string
	if transport, ok := m.wifiEngine.(wifiMMSTransport); ok {
		networkID, e = transport.SendWiFiMMS(ctx, sample.Candidate, card, mmsProfile, id, to, text, part)
	} else {
		e = mms.ErrNetwork
	}
	status, issue := "accepted", ""
	if e != nil {
		issue = e.Error()
		status = "unknown"
		if mms.WaitingNetwork(e) {
			status = "waiting_network"
		} else if issue == "MMS_REJECTED" {
			status = "failed"
		}
	}
	finish, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	m.messageState(finish, id, status, issue, map[string]string{"messageId": networkID})
}
func nativeMMSState(result hardware.MMSSubmitResult, err error) (string, string) {
	if result.Accepted && err == nil {
		return "accepted", ""
	}
	issue := "MMS_OUTCOME_UNKNOWN"
	if err != nil {
		issue = err.Error()
	}
	if issue == "MMS_REJECTED" {
		return "failed", issue
	}
	if result.Attempted {
		return "unknown", issue
	}
	if issue == "MMS_NOT_READY" {
		return "waiting_network", issue
	}
	return "failed", issue
}

// Match all submissions, including already-resolved ones: a reused TP-MR
// must never let an old receipt advance a different message.
func (m *moduleManager) applySMSReports(ctx context.Context) error {
	_, err := m.db.Exec(ctx, `WITH parts AS (
 SELECT m.id,m.iccid,m.peer,m.created_at,(x->>'reference')::int reference,
 COALESCE(NULLIF(x->>'submittedAt','')::timestamptz,m.created_at) submitted_at
 FROM messages m CROSS JOIN LATERAL jsonb_array_elements(
 CASE WHEN jsonb_typeof(m.result->'partResults')='array' THEN m.result->'partResults' ELSE '[]'::jsonb END) x
 WHERE m.mine AND m.kind='sms' AND m.created_at>now()-interval '7 days'
 ), matched AS (
 SELECT p.iccid,p.fingerprint,p.reference,p.status,min(m.id) id,count(DISTINCT m.id) n
 FROM message_reports p JOIN parts m ON m.iccid=p.iccid
 AND ltrim(m.peer,'+')=ltrim(p.peer,'+') AND m.reference=p.reference
 AND p.received_at>=m.created_at AND (p.scts IS NULL OR
 p.scts BETWEEN m.submitted_at-interval '10 minutes' AND m.submitted_at+interval '10 minutes')
 GROUP BY p.iccid,p.fingerprint,p.reference,p.status
 ), outcomes AS (
 SELECT id,max(status) FILTER(WHERE status BETWEEN 64 AND 127) failure,
 count(DISTINCT reference) FILTER(WHERE status=0) delivered
 FROM matched WHERE n=1 GROUP BY id
 )
 UPDATE messages m SET
 state=CASE WHEN o.failure IS NOT NULL THEN 'failed' ELSE 'delivered' END,
 issue=CASE WHEN o.failure IS NOT NULL THEN 'SMS_STATUS_'||o.failure::text ELSE '' END
 FROM outcomes o WHERE m.id=o.id AND (m.state IN ('accepted','partial','unknown') OR (m.state='failed' AND m.issue='RESULT_TIMEOUT'))
 AND (o.failure IS NOT NULL OR (
 m.state<>'partial' AND COALESCE((m.result->>'partsTotal')::int,0)>0
 AND o.delivered=(m.result->>'partsTotal')::int
 AND jsonb_array_length(CASE WHEN jsonb_typeof(m.result->'partResults')='array'
 THEN m.result->'partResults' ELSE '[]'::jsonb END)=(m.result->>'partsTotal')::int))`)
	return err
}

func (m *moduleManager) pollCellularInbox(parent context.Context, id int64) {
	source, ok := m.source.(cellularMessageTransport)
	if !ok {
		return
	}
	m.mu.RLock()
	sample := m.values[id]
	w := m.wifi[id]
	current, present := m.seen[sample.Candidate.Key]
	valid := m.ready && present && sameEndpoint(current, sample.Candidate) && sample.Reading.SIM == "READY" && cellularRegistered(sample.Reading) && !m.jobs[id].active() && (w == nil || !w.Enabled && !w.running) && !time.Now().Before(m.recoveryUntil[sample.Candidate.Key])
	m.mu.RUnlock()
	if !valid {
		return
	}
	gate := m.gate(sample.Candidate.Key)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		return
	}
	// Background reads hold the hardware gate, not a business reservation.
	// A restart can queue behind this read; new polls yield to queued controls.
	m.mu.RLock()
	w = m.wifi[id]
	valid = m.moduleWorkAllowedLocked(sample) && (w == nil || !w.Enabled && !w.running)
	m.mu.RUnlock()
	if !valid {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	err := source.ReadCellularSMS(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), sample.Reading.ICCID, func(ctx context.Context, d hardware.SMSDelivery) error {
		return m.storeIncoming(ctx, id, wifiLine(sample.Reading), sample.Reading.ICCID, d)
	})
	if parent.Err() == nil {
		m.messageRuntime.inbox(id, err)
	}
}
