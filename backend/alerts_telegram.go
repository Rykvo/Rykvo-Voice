package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type telegramResult struct {
	State, Issue string
	MessageID    int64
	Retry        time.Duration
}

func telegramClient(proxy string) (*http.Client, error) {
	u, err := parseTelegramProxy(proxy)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyURL(u), DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxConnsPerHost: 1, MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second}
	return &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func sendTelegram(ctx context.Context, client *http.Client, cfg telegramSettings, message string) telegramResult {
	body, _ := json.Marshal(map[string]string{"chat_id": cfg.Chat, "text": message})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+cfg.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return telegramResult{State: "failed", Issue: "INVALID_BOT_TOKEN"}
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && (op.Op == "dial" || op.Op == "proxyconnect" || op.Op == "socks connect") {
			return telegramResult{State: "pending", Issue: "TELEGRAM_CONNECTION_FAILED", Retry: 30 * time.Second}
		}
		// Never retry an HTTP request whose acceptance cannot be determined.
		return telegramResult{State: "unknown", Issue: "DELIVERY_UNCONFIRMED"}
	}
	defer response.Body.Close()
	var result struct {
		OK     bool `json:"ok"`
		Code   int  `json:"error_code"`
		Result struct {
			ID int64 `json:"message_id"`
		} `json:"result"`
		Parameters struct {
			Retry int `json:"retry_after"`
		} `json:"parameters"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 32768)).Decode(&result)
	if err == nil && response.StatusCode == 200 && result.OK && result.Result.ID > 0 {
		return telegramResult{State: "sent", MessageID: result.Result.ID}
	}
	if err == nil && !result.OK && response.StatusCode == 429 {
		delay := time.Duration(min(max(result.Parameters.Retry, 5), 86400)) * time.Second
		return telegramResult{State: "pending", Issue: "TELEGRAM_RATE_LIMIT", Retry: delay}
	}
	issue := "TELEGRAM_REJECTED"
	switch response.StatusCode {
	case 401:
		issue = "INVALID_BOT_TOKEN"
	case 403:
		issue = "TELEGRAM_ACCESS_DENIED"
	case 400:
		issue = "TELEGRAM_DESTINATION_INVALID"
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return telegramResult{State: "failed", Issue: issue}
	}
	return telegramResult{State: "unknown", Issue: "DELIVERY_UNCONFIRMED"}
}

func (s *server) runAlertDelivery(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.deliverAlert(ctx)
		}
	}
}
func (s *server) deliverAlert(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	cfg, err := s.readTelegram(ctx)
	if err != nil || cfg.Token == "" || cfg.Chat == "" {
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var id, module int64
	var kind, label, number, reason, card string
	var failures, attempts int
	err = tx.QueryRow(ctx, `SELECT n.id,n.module_id,n.kind,m.label,COALESCE(p.number,''),n.failures,n.reason,n.attempts,m.active_card
 FROM alert_notifications n JOIN alert_counters c ON c.module_id=n.module_id AND c.kind=n.kind
 JOIN modules m ON m.id=n.module_id LEFT JOIN card_phone_numbers p ON p.iccid=m.active_card
 WHERE n.state='pending' AND n.next_at<=now() AND n.revision=$1 AND c.active AND c.cycle=n.cycle
 AND c.epoch=n.epoch AND (n.kind='module' OR m.card_epoch=n.epoch)
 ORDER BY n.id LIMIT 1 FOR UPDATE OF n SKIP LOCKED`, cfg.Revision).Scan(&id, &module, &kind, &label, &number, &failures, &reason, &attempts, &card)
	if err != nil {
		return
	}
	if _, err = tx.Exec(ctx, "UPDATE alert_notifications SET state='sending',attempts=attempts+1 WHERE id=$1", id); err != nil {
		return
	}
	if err = tx.Commit(ctx); err != nil {
		return
	}
	if number == "" && s.modules != nil {
		s.modules.mu.RLock()
		sample := s.modules.values[module]
		if sample.Reading.ICCID == card {
			number = sample.Reading.Number
		}
		s.modules.mu.RUnlock()
	}
	if number == "" {
		number = "未识别"
	}
	hostCall := s.hostnameCall
	if hostCall == nil {
		hostCall = hostnameCall
	}
	host, err := hostCall(ctx, map[string]string{"action": "get"})
	if err != nil {
		host.Hostname, _ = os.Hostname()
	}
	if host.Hostname == "" {
		host.Hostname = "未识别"
	}
	var current bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM alert_notifications n JOIN alert_counters c ON c.module_id=n.module_id AND c.kind=n.kind JOIN modules m ON m.id=n.module_id JOIN telegram_settings t ON t.revision=n.revision WHERE n.id=$1 AND n.state='sending' AND c.active AND c.cycle=n.cycle AND c.epoch=n.epoch AND (n.kind='module' OR m.card_epoch=n.epoch))`, id).Scan(&current)
	if err != nil || !current {
		save, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_, _ = s.db.Exec(save, "UPDATE alert_notifications SET state='cancelled' WHERE id=$1 AND state='sending'", id)
		return
	}
	if kind == "module" && wifiAlertReason(reason) {
		if state := s.modules.wifiAlertDelivery(module, time.Now()); state != "sending" {
			_, _ = s.db.Exec(ctx, "UPDATE alert_notifications SET state=$2,attempts=GREATEST(attempts-1,0),next_at=now()+interval '30 seconds' WHERE id=$1 AND state='sending'", id, state)
			return
		}
	}
	title := "SIM 异常"
	if kind == "module" {
		title = "异常"
	}
	clean := func(v string) string {
		return strings.Map(func(r rune) rune {
			if r < ' ' || r == 127 {
				return -1
			}
			return r
		}, v)
	}
	message := fmt.Sprintf("%s\n\n主机：%s\n模块：%s\n号码：%s\n失败：连续 %d 次\n原因：%s", title, clean(host.Hostname), clean(label), clean(number), failures, alertReason(kind, reason))
	client, err := telegramClient(cfg.Proxy)
	result := telegramResult{State: "failed", Issue: "INVALID_TELEGRAM_PROXY"}
	if err == nil {
		result = sendTelegram(ctx, client, cfg, message)
		client.CloseIdleConnections()
	}
	if result.State == "pending" && attempts >= 4 {
		result.State = "failed"
	}
	save, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_, _ = s.db.Exec(save, "UPDATE alert_notifications SET state=$2,issue=$3,message_id=NULLIF($4,0),next_at=$5 WHERE id=$1 AND state='sending'", id, result.State, result.Issue, result.MessageID, time.Now().Add(result.Retry))
}
