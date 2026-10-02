package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type webhookBackoff struct {
	mu    sync.Mutex
	until time.Time
}

func (b *webhookBackoff) ready(now time.Time) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !now.Before(b.until)
}

func (b *webhookBackoff) pause(until time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if until.After(b.until) {
		b.until = until
	}
}

func (s *server) compactWebhookStatuses(ctx context.Context) error {
	// Keep the event log for cursor catch-up; supersede only unsent status hints.
	_, err := s.db.Exec(ctx, `WITH obsolete AS (
 SELECT e.id FROM developer_events e
 WHERE e.state='pending' AND e.event_type='message.status_changed'
 AND EXISTS(SELECT 1 FROM developer_events newer
   WHERE newer.resource_id=e.resource_id AND newer.event_type=e.event_type
   AND newer.state='pending' AND newer.id>e.id
   AND newer.destination=e.destination AND newer.secret=e.secret)
 ORDER BY e.id LIMIT 512
 ) UPDATE developer_events e SET state='cancelled',issue='STATUS_SUPERSEDED',finished_at=now(),lease=''
 FROM obsolete o WHERE e.id=o.id AND e.state='pending'`)
	return err
}

func (s *server) pendingWebhooks(ctx context.Context, inbound bool) ([]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT id FROM developer_events
 WHERE state='pending' AND next_at<=now() AND (event_type='message.received')=$1
 ORDER BY next_at,id LIMIT 48`, inbound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func webhookRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(max(0, min(seconds, 43200))) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, min(date.Sub(now), 12*time.Hour))
	}
	return 0
}
