package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"rykvo.local/auth/internal/dispatch"
)

var webhookBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("2001:20::/28"), netip.MustParsePrefix("2001:10::/28"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
}

func webhookPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range webhookBlocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// DNS is checked on every connection and the checked IP is dialed directly.
func webhookHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 32, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 32, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "443" {
				return nil, errors.New("WEBHOOK_DESTINATION_REJECTED")
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, errors.New("WEBHOOK_DNS_FAILED")
			}
			if len(ips) == 0 {
				return nil, errors.New("WEBHOOK_DNS_FAILED")
			}
			for _, ip := range ips {
				if !webhookPublicIP(ip) {
					return nil, errors.New("WEBHOOK_DESTINATION_REJECTED")
				}
			}
			for _, ip := range ips {
				conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if e == nil {
					return conn, nil
				}
				err = e
			}
			return nil, err
		}}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func webhookSignature(secret, timestamp string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(timestamp))
	h.Write([]byte("."))
	h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

type webhookJob struct {
	ID                         int64
	Destination, Secret, Lease string
	Data                       []byte
	Attempts                   int
}

func (s *server) runDeveloper(ctx context.Context) {
	client := webhookHTTPClient()
	defer client.CloseIdleConnections()
	s.runWebhooks(ctx, client)
}

func (s *server) runWebhooks(ctx context.Context, client *http.Client) {
	// Incoming messages keep their own slots when delivery-state notifications queue up.
	inbox := dispatch.New[int64](ctx, 8)
	status := dispatch.New[int64](ctx, 24)
	defer inbox.Close()
	defer status.Close()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer s.webhookStatus.update("webhook", "WORKER_STOPPED")
	initialized := false
	nextSnapshot := time.Time{}
	nextMaintenance := time.Time{}
	backoff := &webhookBackoff{}
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := func() error {
			if !initialized {
				if _, err := s.db.Exec(call, "UPDATE developer_events SET state=CASE WHEN attempts>=8 THEN 'failed' ELSE 'pending' END,issue='DELIVERY_INTERRUPTED',lease='' WHERE state='sending'"); err != nil {
					return err
				}
				initialized = true
			}
			if !time.Now().Before(nextSnapshot) {
				if err := s.publishDeveloperModules(call); err != nil {
					return err
				}
				nextSnapshot = time.Now().Add(5 * time.Second)
			}
			if !time.Now().Before(nextMaintenance) {
				for _, q := range []string{
					`UPDATE developer_events SET state='cancelled',issue='WEBHOOK_SETTINGS_CHANGED',finished_at=now(),lease=''
     WHERE state='pending' AND NOT EXISTS(SELECT 1 FROM developer_settings WHERE webhook=destination AND webhook_secret=secret AND webhook<>'')`,
					`UPDATE developer_events SET state=CASE WHEN attempts>=8 THEN 'failed' ELSE 'pending' END,issue='DELIVERY_UNCONFIRMED',lease='',next_at=now()
     WHERE state='sending' AND claimed_at<now()-interval '30 seconds'`,
				} {
					if _, err := s.db.Exec(call, q); err != nil {
						return err
					}
				}
				if err := s.compactWebhookStatuses(call); err != nil {
					return err
				}
				nextMaintenance = time.Now().Add(time.Second)
			}
			if !backoff.ready(time.Now()) {
				return nil
			}
			for _, lane := range []struct {
				inbound bool
				group   *dispatch.Group[int64]
			}{{true, inbox}, {false, status}} {
				ids, err := s.pendingWebhooks(call, lane.inbound)
				if err != nil {
					return err
				}
				for _, id := range ids {
					lane.group.Start(id, func(job context.Context) { s.deliverWebhook(job, id, client, backoff) })
				}
			}
			return nil
		}()
		cancel()
		if ctx.Err() != nil {
			return
		}
		issue := ""
		if err != nil {
			issue = "WEBHOOK_WORKER_FAILED"
		}
		s.webhookStatus.update("webhook", issue)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-inbox.Changed():
		case <-status.Changed():
		}
	}
}
func (s *server) publishDeveloperModules(ctx context.Context) error {
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	if _, err = s.db.Exec(ctx, "UPDATE developer_settings SET host_name=$1 WHERE host_name<>$1", host); err != nil {
		return err
	}
	settings, err := s.readDeveloper(ctx)
	if err != nil {
		return err
	}
	if settings.Webhook == "" && len(settings.KeyHash) == 0 {
		return nil
	}
	items, err := s.developerModules(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		idText := item["id"].(string)
		mid, _ := strconv.ParseInt(strings.TrimPrefix(idText, "module-"), 10, 64)
		payload, _ := json.Marshal(item)
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO developer_module_state(module_id,data) VALUES($1,$2) ON CONFLICT(module_id) DO UPDATE SET data=$2 WHERE developer_module_state.data IS DISTINCT FROM $2::jsonb`, mid, payload)
		if err == nil && tag.RowsAffected() > 0 {
			_, err = tx.Exec(ctx, "SELECT developer_emit('module.updated',$1,$2)", idText, payload)
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			tx.Rollback(ctx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *server) deliverWebhook(ctx context.Context, id int64, client *http.Client, backoff *webhookBackoff) {
	if !backoff.ready(time.Now()) {
		return
	}
	claim, cancel := context.WithTimeout(ctx, 3*time.Second)
	var job webhookJob
	job.Lease = token()
	err := s.db.QueryRow(claim, `UPDATE developer_events SET state='sending',attempts=attempts+1,lease=$2,claimed_at=now() WHERE id=$1 AND state='pending' AND next_at<=now()
 AND EXISTS(SELECT 1 FROM developer_settings WHERE webhook=destination AND webhook_secret=secret AND webhook<>'')
 RETURNING id,destination,secret,data,attempts`, id, job.Lease).Scan(&job.ID, &job.Destination, &job.Secret, &job.Data, &job.Attempts)
	cancel()
	if err != nil {
		return
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	request, err := http.NewRequestWithContext(ctx, "POST", job.Destination, bytes.NewReader(job.Data))
	status := 0
	var retryAfter time.Duration
	issue := "WEBHOOK_CONNECTION_FAILED"
	if err == nil && validWebhook(job.Destination) {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", "Rykvo-Voice-Webhook/1")
		request.Header.Set("X-Rykvo-Event-ID", strconv.FormatInt(job.ID, 10))
		request.Header.Set("X-Rykvo-Timestamp", timestamp)
		request.Header.Set("X-Rykvo-Signature", webhookSignature(job.Secret, timestamp, job.Data))
		response, e := client.Do(request)
		if e == nil {
			status = response.StatusCode
			if status == 429 || status == 503 {
				retryAfter = webhookRetryAfter(response.Header.Get("Retry-After"), time.Now())
			}
			// Consume a bounded response; never log callback bodies or authorization material.
			io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			response.Body.Close()
			issue = fmt.Sprintf("WEBHOOK_HTTP_%d", status)
		}
	}
	state := "pending"
	delays := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour}
	delay := max(delays[min(job.Attempts-1, len(delays)-1)], retryAfter)
	if status == 429 || status == 503 {
		backoff.pause(time.Now().Add(max(5*time.Second, retryAfter)))
	}
	if status >= 200 && status < 300 {
		state = "delivered"
		issue = ""
	} else if job.Attempts >= 8 || status >= 300 && status < 500 && status != 408 && status != 425 && status != 429 {
		state = "failed"
	}
	save, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	// At-least-once delivery: a lost DB write is replayed with the same event ID.
	_, err = s.db.Exec(save, `UPDATE developer_events SET state=$3,issue=$4,response_status=$5,next_at=now()+make_interval(secs=>$6),lease='',
 finished_at=CASE WHEN $3 IN ('delivered','failed') THEN now() ELSE NULL END WHERE id=$1 AND state='sending' AND lease=$2`, id, job.Lease, state, issue, status, delay.Seconds())
	if err != nil {
		// The durable lease is reclaimed by the next worker cycle, without losing the event.
		return
	}
}
