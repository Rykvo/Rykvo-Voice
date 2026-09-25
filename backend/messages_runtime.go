package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/mms"
	"rykvo.local/auth/internal/vocat/device"
	"rykvo.local/auth/internal/vocat/vowifi"
)

type cellularMessageTransport interface {
	SendCellularSMS(context.Context, hardware.Candidate, string, string, string, string) (vowifi.SMSSubmitResult, error)
	ReadCellularSMS(context.Context, hardware.Candidate, string, string, func(context.Context, hardware.SMSDelivery) error) error
}

func cellularRegistered(r hardware.Reading) bool {
	return r.Registration == "home" || r.Registration == "roaming" || r.Registration == "registered"
}

type smsTransport interface {
	SendSMS(context.Context, hardware.Candidate, string, string, string, string) (vowifi.SMSSubmitResult, error)
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
		if seq < 1 || total < 2 || total > 32 || seq > total || v.Concat.Reference < 0 || v.Concat.Reference > 65535 {
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
	meta, _ := json.Marshal(metadata)
	_, e = tx.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,state,metadata) VALUES($1,$2,$3,$4,$5,false,$6,'',$7,$8) ON CONFLICT(id) DO NOTHING`, id, module, card, line, v.From, kind, state, meta)
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
			payload, e := hex.DecodeString(assembled)
			if e == nil {
				push, e := mms.Push(payload)
				if e == nil && push.Type == 0x82 && push.Location != "" && push.Transaction != "" {
					state = "download_pending"
					metadata = map[string]any{"location": push.Location, "transaction": push.Transaction}
					meta, _ = json.Marshal(metadata)
				} else {
					state = "unsupported_push"
				}
			} else {
				state = "decode_error"
			}
		}
		assembled = ""
	}
	_, e = tx.Exec(ctx, "UPDATE messages SET body=$2,state=$3,kind=$4,metadata=$5 WHERE id=$1", id, assembled, state, kind, meta)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (m *moduleManager) runMessages(ctx context.Context) {
	// A crashed POST can have reached the network. Never replay an uncertain send.
	if _, e := m.db.Exec(ctx, "UPDATE messages SET state='unknown',issue='SEND_INTERRUPTED' WHERE mine AND state='sending'"); e != nil {
		return
	}
	_, _ = m.db.Exec(ctx, "UPDATE messages SET state='download_pending' WHERE NOT mine AND state='downloading'")
	cursor := 0
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.processMessage(ctx)
			m.applySMSReports(ctx)
			m.pollCellularInbox(ctx, &cursor)
		}
	}
}
func (m *moduleManager) processMessage(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 100*time.Second)
	defer cancel()
	// Waiting work is retried only before any network submission is known to occur.
	var id, card, line, to, text, rawImage, kind, state string
	var module int64
	var metadata []byte
	var created time.Time
	e := m.db.QueryRow(ctx, `SELECT id,module_id,iccid,line_id,peer,body,image,kind,state,metadata,created_at FROM messages WHERE deleted_at IS NULL AND (state IN ('queued','download_pending') OR (state='waiting_network' AND updated_at<now()-interval '30 seconds')) ORDER BY updated_at LIMIT 1`).Scan(&id, &module, &card, &line, &to, &text, &rawImage, &kind, &state, &metadata, &created)
	if e != nil {
		return
	}
	if time.Since(created) > 24*time.Hour {
		m.messageState(ctx, id, "expired", "MESSAGE_EXPIRED", nil)
		return
	}
	m.mu.RLock()
	sample, ok := m.values[module]
	current, present := m.seen[sample.Candidate.Key]
	valid := m.ready && ok && present && sameEndpoint(current, sample.Candidate) && sample.Reading.ICCID == card && wifiLine(sample.Reading) == line && !m.jobs[module].active() && time.Since(m.lastScan) < 20*time.Second
	profile := m.carrierConfigs[card]
	wifi := m.wifi[module]
	useWiFi := wifi != nil && (wifi.Enabled || wifi.running || wifi.State == "stopping")
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
	if kind == "mms" && (profile.MMS.Status != "matched" || profile.MMS.Profile == nil) {
		m.messageState(ctx, id, "waiting_network", "MMS_CONFIG_REQUIRED", nil)
		return
	}
	// CAS also honors a cancellation that happened after selecting this job.
	next := "sending"
	if strings.HasPrefix(id, "rx-") {
		next = "downloading"
	}
	tag, e := m.db.Exec(ctx, "UPDATE messages SET state=$2,issue='' WHERE id=$1 AND state=$3 AND deleted_at IS NULL", id, next, state)
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
			gate := m.gate(sample.Candidate.Key)
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			default:
				m.messageState(ctx, id, "waiting_network", "SMS_BUSY", nil)
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
	if next == "downloading" {
		var meta struct {
			Location    string `json:"location"`
			Transaction string `json:"transaction"`
		}
		if json.Unmarshal(metadata, &meta) != nil {
			m.messageState(ctx, id, "failed", "MMS_INVALID_NOTIFICATION", nil)
			return
		}
		p, e := mms.Retrieve(ctx, *profile.MMS.Profile, meta.Location)
		if e != nil {
			m.messageState(ctx, id, "waiting_network", "MMS_NETWORK_REQUIRED", nil)
			return
		}
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
			m.messageState(ctx, id, "failed", "MMS_UNSUPPORTED_CONTENT", nil)
			return
		}
		peer := strings.TrimSuffix(p.From, "/TYPE=PLMN")
		if !hardware.ValidSMS(peer, "x") {
			peer = to
		}
		if _, e = m.db.Exec(ctx, "UPDATE messages SET body=$2,image=$3,peer=$4,state='received',issue='' WHERE id=$1", id, body, img, peer); e == nil {
			_ = mms.Acknowledge(ctx, *profile.MMS.Profile, meta.Transaction)
		}
		return
	}
	part, e := messageImage(rawImage)
	if e != nil {
		m.messageState(ctx, id, "failed", "INVALID_IMAGE", nil)
		return
	}
	networkID, e := mms.Send(ctx, *profile.MMS.Profile, id, to, text, part)
	status, issue := "accepted", ""
	if e != nil {
		issue = e.Error()
		status = "unknown"
		if errors.Is(e, mms.ErrNetwork) {
			status = "waiting_network"
		} else if issue == "MMS_REJECTED" {
			status = "failed"
		}
	}
	finish, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	m.messageState(finish, id, status, issue, map[string]string{"messageId": networkID})
}
func (m *moduleManager) messageState(ctx context.Context, id, state, issue string, result any) {
	b := []byte(`{}`)
	if result != nil {
		b, _ = json.Marshal(result)
	}
	_, _ = m.db.Exec(ctx, "UPDATE messages SET state=$2,issue=$3,result=$4 WHERE id=$1 AND state<>'cancelled'", id, state, issue, b)
}

// Delivery receipts are evidence for a TP-MR, not proof of reading. Ambiguous
// reference reuse stays accepted; only one matching submission is advanced.
func (m *moduleManager) applySMSReports(ctx context.Context) {
	_, _ = m.db.Exec(ctx, `WITH matched AS (
 SELECT p.iccid,p.peer,p.reference,p.status,min(m.id) id,count(*) n FROM message_reports p JOIN messages m ON m.iccid=p.iccid AND m.peer=p.peer AND m.mine AND m.kind='sms' AND m.state IN ('accepted','partial') AND m.created_at>now()-interval '7 days' AND p.received_at>=m.created_at AND (p.scts IS NULL OR p.scts>=m.created_at-interval '10 minutes') AND EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(m.result->'partResults','[]')) x WHERE (x->>'reference')::int=p.reference AND x->>'accepted'='true') GROUP BY p.iccid,p.peer,p.reference,p.status
 ), complete AS (
 SELECT m.id FROM messages m WHERE m.state='accepted' AND jsonb_array_length(COALESCE(m.result->'partResults','[]'))>0 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(m.result->'partResults') x WHERE NOT EXISTS(SELECT 1 FROM matched p WHERE p.id=m.id AND p.n=1 AND p.status=0 AND p.reference=(x->>'reference')::int))
 ) UPDATE messages SET state='delivered' WHERE id IN (SELECT id FROM complete)`)
}

func (m *moduleManager) pollCellularInbox(parent context.Context, cursor *int) {
	source, ok := m.source.(cellularMessageTransport)
	if !ok {
		return
	}
	m.mu.RLock()
	ids := []int64{}
	for id := range m.values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		m.mu.RUnlock()
		return
	}
	id := ids[*cursor%len(ids)]
	*cursor++
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
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	_ = source.ReadCellularSMS(ctx, sample.Candidate, sample.Candidate.Identity(sample.Reading), sample.Reading.ICCID, func(ctx context.Context, d hardware.SMSDelivery) error {
		return m.storeIncoming(ctx, id, wifiLine(sample.Reading), sample.Reading.ICCID, d)
	})
}
