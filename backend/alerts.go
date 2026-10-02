package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type alertSettings struct {
	SIP      int   `json:"sip"`
	Message  int   `json:"message"`
	Module   int   `json:"module"`
	Revision int64 `json:"revision"`
}
type telegramSettings struct {
	Token, Proxy string
	Admin        string `json:"adminId"`
	Chat         string `json:"notificationId"`
	Revision     int64  `json:"revision"`
}
type moduleAlert struct {
	Kind        string `json:"kind"`
	Failures    int    `json:"failures"`
	Active      bool   `json:"active"`
	Reason      string `json:"reason"`
	Description string `json:"description"`
}

var alertCardPattern = regexp.MustCompile(`^[0-9]{18,20}$`)
var telegramTokenPattern = regexp.MustCompile(`^[0-9]{3,24}:[A-Za-z0-9_-]{20,128}$`)
var telegramChatPattern = regexp.MustCompile(`^-?[1-9][0-9]{0,18}$`)
var telegramAdminPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var telegramProxyPattern = regexp.MustCompile(`^(\[[^\]]+\]|[^:]+):([0-9]+):([^:]*):(.*)$`)

func validICCID(card string) bool { return alertCardPattern.MatchString(card) }

func parseTelegramProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	p := telegramProxyPattern.FindStringSubmatch(raw)
	if len(raw) > 2048 || len(p) != 5 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, errors.New("INVALID_TELEGRAM_PROXY")
	}
	host := strings.Trim(p[1], "[]")
	port, err := strconv.Atoi(p[2])
	ip := net.ParseIP(host)
	if err != nil || port < 1 || port > 65535 || ip == nil || ip.IsUnspecified() || ip.IsMulticast() || (p[3] == "") != (p[4] == "") || len(p[3]) > 255 || len(p[4]) > 255 {
		return nil, errors.New("INVALID_TELEGRAM_PROXY")
	}
	u := &url.URL{Scheme: "socks5", Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if p[3] != "" {
		u.User = url.UserPassword(p[3], p[4])
	}
	return u, nil
}

func (s *server) alertSettingsAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var v alertSettings
	switch r.Method {
	case http.MethodGet:
		if err := s.db.QueryRow(ctx, "SELECT sip,message,module,revision FROM alert_settings").Scan(&v.SIP, &v.Message, &v.Module, &v.Revision); err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
	case http.MethodPut:
		var input struct {
			SIP      *int  `json:"sip"`
			Message  *int  `json:"message"`
			Module   *int  `json:"module"`
			Revision int64 `json:"revision"`
		}
		if !decodeBody(w, r, &input) {
			return
		}
		if input.SIP == nil || input.Message == nil || input.Module == nil {
			fail(w, 400, "INVALID_ALERT_LIMIT")
			return
		}
		v = alertSettings{*input.SIP, *input.Message, *input.Module, input.Revision}
		if v.SIP < 0 || v.SIP > 100 || v.Message < 0 || v.Message > 100 || v.Module < 0 || v.Module > 100 || v.Revision < 1 {
			fail(w, 400, "INVALID_ALERT_LIMIT")
			return
		}
		// Zero closes that alert cycle; other changes keep the existing evidence.
		err := s.db.QueryRow(ctx, "UPDATE alert_settings SET sip=$1,message=$2,module=$3,revision=revision+1 WHERE revision=$4 RETURNING revision", v.SIP, v.Message, v.Module, v.Revision).Scan(&v.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 409, "SETTINGS_CHANGED")
			return
		}
		if err != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
	default:
		w.Header().Set("Allow", "GET, PUT")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	reply(w, 200, map[string]any{"data": v})
}

func (s *server) readTelegram(ctx context.Context) (v telegramSettings, err error) {
	err = s.db.QueryRow(ctx, "SELECT token,proxy,admin_id,notification_id,revision FROM telegram_settings").Scan(&v.Token, &v.Proxy, &v.Admin, &v.Chat, &v.Revision)
	return
}
func secretPatch(raw json.RawMessage, old string) (string, error) {
	if raw == nil {
		return old, nil
	}
	if string(raw) == "null" {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", errors.New("INVALID_INPUT")
	}
	return strings.TrimSpace(value), nil
}
func (s *server) developerSettingsAPI(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	v, err := s.readTelegram(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	dev, err := s.readDeveloper(ctx)
	if err != nil {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPatch:
		oldTelegram := v
		var input struct {
			developerPatch
			Token    json.RawMessage `json:"botToken"`
			Proxy    json.RawMessage `json:"telegramProxy"`
			Admin    *string         `json:"adminId"`
			Chat     *string         `json:"notificationId"`
			Revision int64           `json:"revision"`
		}
		if !decodeJSONBody(w, r, &input, 8192, "INVALID_INPUT") {
			return
		}
		if input.Revision != v.Revision {
			fail(w, 409, "SETTINGS_CHANGED")
			return
		}
		v.Token, err = secretPatch(input.Token, v.Token)
		if err != nil || v.Token != "" && !telegramTokenPattern.MatchString(v.Token) {
			fail(w, 400, "INVALID_BOT_TOKEN")
			return
		}
		v.Proxy, err = secretPatch(input.Proxy, v.Proxy)
		if err != nil {
			fail(w, 400, "INVALID_TELEGRAM_PROXY")
			return
		}
		if _, err = parseTelegramProxy(v.Proxy); err != nil {
			fail(w, 400, "INVALID_TELEGRAM_PROXY")
			return
		}
		if input.Admin != nil {
			v.Admin = strings.TrimSpace(*input.Admin)
		}
		if input.Chat != nil {
			v.Chat = strings.TrimSpace(*input.Chat)
		}
		if v.Admin != "" && !telegramAdminPattern.MatchString(v.Admin) || v.Chat != "" && !telegramChatPattern.MatchString(v.Chat) {
			fail(w, 400, "INVALID_TELEGRAM_ID")
			return
		}
		var developerChanged bool
		dev, developerChanged, err = input.developerPatch.apply(dev)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		telegramChanged := oldTelegram.Token != v.Token || oldTelegram.Proxy != v.Proxy || oldTelegram.Admin != v.Admin || oldTelegram.Chat != v.Chat
		tx, e := s.db.Begin(ctx)
		if e != nil {
			fail(w, 503, "DATABASE_UNAVAILABLE")
			return
		}
		defer tx.Rollback(ctx)
		if telegramChanged {
			err = tx.QueryRow(ctx, "UPDATE telegram_settings SET token=$1,proxy=$2,admin_id=$3,notification_id=$4,revision=revision+1 WHERE revision=$5 RETURNING revision", v.Token, v.Proxy, v.Admin, v.Chat, v.Revision).Scan(&v.Revision)
			if errors.Is(err, pgx.ErrNoRows) {
				fail(w, 409, "SETTINGS_CHANGED")
				return
			}
			if err == nil {
				_, err = tx.Exec(ctx, "UPDATE alert_notifications SET state='cancelled' WHERE state='pending'")
			}
		}
		if err == nil && developerChanged {
			err = saveDeveloper(ctx, tx, &dev)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			writeServiceError(w, err)
			return
		}
	default:
		w.Header().Set("Allow", "GET, PATCH")
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	var delivery, issue string
	err = s.db.QueryRow(ctx, "SELECT state,issue FROM alert_notifications WHERE revision=$1 AND state IN ('sent','failed','unknown') ORDER BY id DESC LIMIT 1", v.Revision).Scan(&delivery, &issue)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		fail(w, 503, "DATABASE_UNAVAILABLE")
		return
	}
	reply(w, 200, map[string]any{"data": map[string]any{"hasApiKey": len(dev.KeyHash) > 0, "webhook": dev.Webhook, "apiRevision": dev.Revision, "hostId": dev.HostID, "apiBase": s.developerBase(r), "adminId": v.Admin, "notificationId": v.Chat, "revision": v.Revision, "hasBotToken": v.Token != "", "hasTelegramProxy": v.Proxy != "", "deliveryState": delivery, "deliveryIssue": issue}})
}

func (s *server) moduleAlerts(ctx context.Context) (map[string][]moduleAlert, error) {
	return readModuleAlerts(ctx, s.db)
}

func readModuleAlerts(ctx context.Context, db moduleReader) (map[string][]moduleAlert, error) {
	rows, err := db.Query(ctx, "SELECT c.module_id,c.kind,c.failures,c.active,c.reason FROM alert_counters c JOIN modules m ON m.id=c.module_id WHERE c.failures>0 AND (c.kind='module' OR c.epoch=m.card_epoch) ORDER BY c.module_id,c.kind")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := map[string][]moduleAlert{}
	for rows.Next() {
		var id int64
		var a moduleAlert
		if err = rows.Scan(&id, &a.Kind, &a.Failures, &a.Active, &a.Reason); err != nil {
			return nil, err
		}
		a.Description = alertReason(a.Kind, a.Reason)
		all[moduleID(id)] = append(all[moduleID(id)], a)
	}
	return all, rows.Err()
}
func attachModuleAlerts(view map[string]any, all map[string][]moduleAlert) {
	wifi, _ := view["wifi"].(map[string]any)
	list := []moduleAlert{}
	for _, a := range all[view["id"].(string)] {
		if a.Kind == "module" && wifiAlertReason(a.Reason) && (wifi["registered"] == true || wifi["enabled"] == false || wifi["recovering"] == true) {
			continue
		}
		list = append(list, a)
	}
	view["alerts"] = list
	for _, a := range list {
		if a.Active && a.Kind == "module" && wifiAlertReason(a.Reason) && view["status"] == "online" {
			view["status"] = "error"
			view["issue"] = a.Reason
		}
	}
}
