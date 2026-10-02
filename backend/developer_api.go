package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"rykvo.local/auth/internal/hardware"
)

type apiLimiter struct {
	mu      sync.Mutex
	entries map[string]attempts
}

func (l *apiLimiter) allow(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.entries == nil {
		l.entries = make(map[string]attempts)
	}
	for k, v := range l.entries {
		if now.After(v.until) {
			delete(l.entries, k)
		}
	}
	v, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= 4096 {
			return false
		}
		v.until = now.Add(time.Minute)
	}
	if v.count >= limit {
		return false
	}
	v.count++
	l.entries[key] = v
	return true
}
func trustedProxyHTTPS(r *http.Request) bool {
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(peer)
	return r.TLS != nil || ip != nil && ip.IsLoopback() && r.Header.Get("X-Forwarded-Proto") == "https"
}
func (s *server) developerBase(r *http.Request) string {
	if s.tunnels != nil {
		s.tunnels.mu.Lock()
		b := s.tunnels.binding
		s.tunnels.mu.Unlock()
		if b.Resume && b.DNSID != "" && b.Domain != "" {
			return "https://" + b.Domain + "/api/v1"
		}
	}
	if strings.HasPrefix(s.origin, "https://") {
		return s.origin + "/api/v1"
	}
	return "http://" + r.Host + "/api/v1"
}
func (s *server) developerAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if !localOrigin("http://"+r.Host) && !trustedProxyHTTPS(r) {
		fail(w, 403, "HTTPS_REQUIRED")
		return
	}
	if r.URL.RawQuery != "" {
		q := r.URL.Query()
		for _, k := range []string{"apiKey", "api_key", "token", "key", "username"} {
			if q.Has(k) {
				fail(w, 400, "CREDENTIALS_IN_URL")
				return
			}
		}
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(peer); ip != nil && ip.IsLoopback() && net.ParseIP(r.Header.Get("X-Real-IP")) != nil {
		peer = r.Header.Get("X-Real-IP")
	}
	key := r.Header.Get("X-API-Key")
	if len(r.Header.Values("X-API-Key")) != 1 || !validDeveloperSecret(key) {
		fail(w, 401, "API_UNAUTHENTICATED")
		return
	}
	release := s.admitDeveloper(w, r, peer)
	if release == nil {
		return
	}
	defer release()
	settings, err := s.readDeveloper(ctx)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	keyOK := subtle.ConstantTimeCompare(tokenHash(key), settings.KeyHash)
	if keyOK != 1 {
		fail(w, 401, "API_UNAUTHENTICATED")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1")
	budgets := []apiBudget{developerTotalBudget()}
	if r.Method != http.MethodPost || tail != "/messages/batch" {
		budgets = append(budgets, developerCategory(r.Method, tail))
	}
	if delay := s.developerWork.take(time.Now(), budgets...); delay > 0 {
		developerReject(w, 429, "API_RATE_LIMIT", delay)
		return
	}
	switch {
	case tail == "/events" && r.Method == "GET":
		s.developerEventList(ctx, w, r)
	case tail == "/modules" && r.Method == "GET":
		items, err := s.developerModules(ctx)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		host, _ := os.Hostname()
		reply(w, 200, map[string]any{"data": map[string]any{"hostId": settings.HostID, "host": host, "items": items,
			"features": map[string]int{"messageBatch": developerBatchLimit, "messageLookup": developerLookupLimit}}})
	case tail == "/messages/batch" && r.Method == "POST":
		s.developerMessageBatch(ctx, w, r, settings.Revision)
	case tail == "/messages/lookup" && r.Method == "POST":
		s.developerMessageLookup(ctx, w, r)
	case tail == "/messages" && r.Method == "POST":
		var in messageInput
		if !decodeMessageBody(w, r, &in) {
			return
		}
		if in.Image != "" {
			if delay := s.developerWork.take(time.Now(), developerCategory(http.MethodPost, "/attachments")); delay > 0 {
				developerReject(w, 429, "API_RATE_LIMIT", delay)
				return
			}
		}
		if in.Image != "" || in.AttachmentID != "" {
			releaseImage := s.developerWork.image()
			if releaseImage == nil {
				developerReject(w, 503, "API_BUSY", time.Second)
				return
			}
			defer releaseImage()
		}
		v, created, err := s.enqueueMessage(ctx, in, settings.Revision)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		status := 200
		if created {
			status = 202
		}
		s.developerMessageReply(ctx, w, status, v)
	case tail == "/messages" && r.Method == "GET":
		s.developerMessageList(ctx, w, r)
	case strings.HasPrefix(tail, "/messages/") && r.Method == "GET":
		id := strings.TrimPrefix(tail, "/messages/")
		if strings.HasSuffix(id, "/image") {
			copy := r.Clone(ctx)
			copy.URL.Path = "/api/messages/" + id
			s.messagesAPI(ctx, w, copy)
			return
		}
		v, err := scanMessage(s.db.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", id))
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 404, "NOT_FOUND")
			return
		}
		if err != nil {
			writeServiceError(w, err)
			return
		}
		s.developerMessageReply(ctx, w, 200, v)
	case tail == "/attachments" && r.Method == "POST":
		var in struct {
			Image string `json:"image"`
		}
		if !decodeMessageBody(w, r, &in) {
			return
		}
		part, err := messageImage(in.Image)
		if err != nil || part == nil {
			fail(w, 400, "INVALID_IMAGE")
			return
		}
		tx, err := s.db.Begin(ctx)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(-734903)"); err != nil {
			writeServiceError(w, err)
			return
		}
		var count, uploadHours int
		if err = tx.QueryRow(ctx, "SELECT upload_hours FROM retention_settings FOR SHARE").Scan(&uploadHours); err != nil {
			writeServiceError(w, err)
			return
		}
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM developer_uploads WHERE created_at>now()-make_interval(hours=>$1)", uploadHours).Scan(&count); err != nil {
			writeServiceError(w, err)
			return
		}
		if count >= 256 {
			fail(w, 429, "UPLOAD_QUEUE_FULL")
			return
		}
		id := "upload-" + token()
		if _, err = tx.Exec(ctx, "INSERT INTO developer_uploads(id,content) VALUES($1,$2)", id, in.Image); err != nil {
			writeServiceError(w, err)
			return
		}
		if err = tx.Commit(ctx); err != nil {
			writeServiceError(w, err)
			return
		}
		reply(w, 201, map[string]any{"data": map[string]any{"id": id, "expiresIn": uploadHours * 3600, "type": part.Type, "size": len(part.Data)}})
	default:
		fail(w, 404, "NOT_FOUND")
	}
}
func decodeMessageBody(w http.ResponseWriter, r *http.Request, in any) bool {
	return decodeJSONBody(w, r, in, 1600000, "INVALID_MESSAGE")
}

func messageDisplay(state string) (status, text string) {
	switch state {
	case "accepted", "delivered":
		return "delivered", "已送达"
	case "received":
		return "received", "已收到"
	case "waiting_network":
		return "pending", "等待网络"
	case "queued", "sending", "receiving", "download_pending", "downloading":
		return "pending", ""
	default:
		return "not_delivered", "尚未送达"
	}
}

type developerMessage struct {
	messageView
	ModuleID          string `json:"moduleId"`
	CardVersion       int64  `json:"cardVersion"`
	DisplayStatus     string `json:"displayStatus"`
	StatusText        string `json:"statusText"`
	CarrierAccepted   bool   `json:"carrierAccepted"`
	DeliveryConfirmed bool   `json:"deliveryConfirmed"`
	AttachmentPath    string `json:"attachmentPath,omitempty"`
}

func developerMessageView(v messageView, epoch int64) developerMessage {
	status, text := messageDisplay(v.State)
	out := developerMessage{messageView: v, ModuleID: v.SenderID, CardVersion: epoch, DisplayStatus: status, StatusText: text, CarrierAccepted: v.State == "accepted" || v.State == "delivered", DeliveryConfirmed: v.State == "delivered"}
	if v.Image != "" && !v.Deleted {
		out.AttachmentPath = "/api/v1/messages/" + v.ID + "/image"
	}
	if v.Deleted {
		out.Text = ""
		out.Image = ""
	}
	return out
}
func (s *server) developerMessageReply(ctx context.Context, w http.ResponseWriter, status int, v messageView) {
	var epoch int64
	if err := s.db.QueryRow(ctx, "SELECT alert_epoch FROM messages WHERE id=$1", v.ID).Scan(&epoch); err != nil {
		writeServiceError(w, err)
		return
	}
	reply(w, status, map[string]any{"data": developerMessageView(v, epoch)})
}
func (s *server) developerMessageList(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := s.beginMessageSync(ctx, q)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer page.tx.Rollback(ctx)
	after := page.after
	limit := 100
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 200 || q.Get("direction") != "" && q.Get("direction") != "incoming" && q.Get("direction") != "outgoing" {
		fail(w, 400, "INVALID_INPUT")
		return
	}
	module := q.Get("moduleId")
	rows, err := page.tx.Query(ctx, `SELECT `+messageColumns+`,alert_epoch FROM messages WHERE revision>$1 AND revision<=$5
 AND ($2='' OR 'module-'||lpad(module_id::text,GREATEST(2,length(module_id::text)),'0')=$2)
 AND ($3='' OR mine=($3='outgoing')) ORDER BY revision LIMIT $4`, after, module, q.Get("direction"), limit+1, page.snapshot)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer rows.Close()
	items := []developerMessage{}
	for rows.Next() {
		// Scan the common projection without a second query per message.
		var v messageView
		var id, epoch int64
		var hasImage bool
		var at time.Time
		if err = rows.Scan(&v.ID, &id, &v.LineID, &v.Number, &v.Text, &hasImage, &v.Mine, &v.Kind, &v.State, &v.Issue, &at, &v.Revision, &v.Deleted, &epoch); err != nil {
			writeServiceError(w, err)
			return
		}
		if v.State == "mms_report" {
			v.Deleted = true
		}
		v.SenderID = moduleID(id)
		v.At = at.UnixMilli()
		if hasImage {
			v.Image = v.ID
		}
		items = append(items, developerMessageView(v, epoch))
	}
	if err = rows.Err(); err != nil {
		writeServiceError(w, err)
		return
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	if len(items) > 0 {
		after = items[len(items)-1].Revision
	}
	reply(w, 200, map[string]any{"data": map[string]any{"items": items, "cursor": page.cursor(after, more), "more": more, "snapshot": page.snapshot}})
}
func (s *server) developerModules(ctx context.Context) ([]map[string]any, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	return s.developerModuleSnapshot(ctx, tx)
}

// Module identity, card version and counters belong to one committed snapshot.
func (s *server) developerModuleSnapshot(ctx context.Context, db moduleReader) ([]map[string]any, error) {
	records, err := listModuleRecords(ctx, db)
	if err != nil {
		return nil, err
	}
	alerts, err := readModuleAlerts(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(ctx, "SELECT id,card_epoch,active_card FROM modules")
	if err != nil {
		return nil, err
	}
	type card struct {
		epoch int64
		iccid string
	}
	cards := map[int64]card{}
	for rows.Next() {
		var id int64
		var c card
		if err = rows.Scan(&id, &c.epoch, &c.iccid); err != nil {
			rows.Close()
			return nil, err
		}
		cards[id] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(records))
	for _, v := range records {
		view := s.moduleView(v)
		list := alerts[moduleID(v.ID)]
		if list == nil {
			list = []moduleAlert{}
		}
		number := developerModuleNumber(view, cards[v.ID].iccid)
		items = append(items, map[string]any{"id": moduleID(v.ID), "name": moduleDisplayName(v.Label), "number": number, "numberKnown": number != "", "cardVersion": cards[v.ID].epoch, "status": view["status"], "signal": view["signal"], "alerts": list, "capabilities": view["capabilities"]})
	}
	return items, nil
}

func developerModuleNumber(view map[string]any, card string) string {
	// Re-reading live state here could pair a new card with the old view's number.
	reading, ok := view["hardware"].(hardware.Reading)
	if !ok || card == "" || reading.ICCID != card {
		return ""
	}
	return reading.Number
}

func developerReject(w http.ResponseWriter, status int, code string, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
	fail(w, status, code)
}

func (s *server) admitDeveloper(w http.ResponseWriter, r *http.Request, peer string) func() {
	if delay := s.developerWork.take(time.Now(), apiBudget{"ingress", 6000, 100}, apiBudget{"peer:" + peer, 6000, 100}); delay > 0 {
		developerReject(w, 429, "API_RATE_LIMIT", delay)
		return nil
	}
	upload := r.Method == http.MethodPost && strings.TrimPrefix(r.URL.Path, "/api/v1") == "/attachments"
	release := s.developerWork.enter(upload)
	if release == nil {
		developerReject(w, 503, "API_BUSY", time.Second)
	}
	return release
}
