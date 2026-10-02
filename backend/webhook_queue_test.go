package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 0}, {"invalid", 0}, {"-1", 0}, {" 60 ", time.Minute},
		{"9999999", 12 * time.Hour}, {now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		if got := webhookRetryAfter(tc.raw, now); got != tc.want {
			t.Fatalf("%q: %v != %v", tc.raw, got, tc.want)
		}
	}
	b := &webhookBackoff{}
	b.pause(now.Add(time.Minute))
	b.pause(now.Add(time.Second))
	if b.ready(now.Add(59*time.Second)) || !b.ready(now.Add(time.Minute)) {
		t.Fatal("cooldown shortened or never recovered")
	}
}

func TestWebhookDatabase(t *testing.T) {
	s := &server{db: freshTestDatabase(t, "TEST_WEBHOOK_DATABASE_URL")}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE developer_settings SET webhook='https://example.com/hook',webhook_secret='fixture-secret',key_hash='\x01'`)
	t.Run("incoming SMS emits only complete received payload", func(t *testing.T) { testIncomingWebhook(t, s) })
	exec("DELETE FROM developer_events")
	emit := func(kind, resource string) int64 {
		t.Helper()
		var id int64
		if err := s.db.QueryRow(ctx, `SELECT developer_emit($1,$2,jsonb_build_object('id',$2::text))`, kind, resource).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	state := func(id int64) string {
		t.Helper()
		var value string
		if err := s.db.QueryRow(ctx, `SELECT state FROM developer_events WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("coalescing retains the event log and independent events", func(t *testing.T) {
		old := emit("message.status_changed", "same-message")
		latest := emit("message.status_changed", "same-message")
		in1 := emit("message.received", "inbound")
		in2 := emit("message.received", "inbound")
		claimed := emit("message.status_changed", "claimed")
		exec(`UPDATE developer_events SET state='sending' WHERE id=$1`, claimed)
		emit("message.status_changed", "claimed")
		rotated := emit("message.status_changed", "rotated")
		exec(`UPDATE developer_events SET secret='old-secret' WHERE id=$1`, rotated)
		emit("message.status_changed", "rotated")
		module := emit("module.updated", "module-01")
		emit("module.updated", "module-01")
		if err := s.compactWebhookStatuses(ctx); err != nil {
			t.Fatal(err)
		}
		if state(old) != "cancelled" || state(latest) != "pending" || state(claimed) != "sending" {
			t.Fatal("wrong status superseded")
		}
		for _, id := range []int64{in1, in2, rotated, module} {
			if state(id) != "pending" {
				t.Fatal("independent event removed", id)
			}
		}
		w := httptest.NewRecorder()
		s.developerEventList(ctx, w, httptest.NewRequest("GET", "/api/v1/events?after=0", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"eventId":"`+strconv.FormatInt(old, 10)+`"`) {
			t.Fatal("superseded event lost from cursor", w.Body.String())
		}
		if _, err := s.db.Exec(ctx, developerSchema); err != nil {
			t.Fatal("repeat migration", err)
		}
		if state(old) != "cancelled" {
			t.Fatal("repeat migration reset delivery state")
		}
	})
	exec("DELETE FROM developer_events")
	t.Run("bounded coalescing", func(t *testing.T) {
		exec(`SELECT developer_emit('message.status_changed','batch','{}') FROM generate_series(1,600)`)
		if err := s.compactWebhookStatuses(ctx); err != nil {
			t.Fatal(err)
		}
		var pending int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM developer_events WHERE state='pending'`).Scan(&pending); err != nil || pending != 88 {
			t.Fatal("batch bound", pending, err)
		}
		if err := s.compactWebhookStatuses(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM developer_events WHERE state='pending'`).Scan(&pending); err != nil || pending != 1 {
			t.Fatal("latest status missing", pending, err)
		}
	})
	exec("DELETE FROM developer_events")
	t.Run("retry-after persists and pauses new delivery", func(t *testing.T) {
		id := emit("message.status_changed", "rate-limit")
		next := emit("message.received", "waiting")
		calls := 0
		client := &http.Client{Transport: webhookRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		b := &webhookBackoff{}
		s.deliverWebhook(ctx, id, client, b)
		s.deliverWebhook(ctx, next, client, b)
		var wait float64
		if err := s.db.QueryRow(ctx, `SELECT extract(epoch FROM next_at-now())::float8 FROM developer_events WHERE id=$1`, id).Scan(&wait); err != nil || wait < 58 || wait > 61 {
			t.Fatal("Retry-After not saved", wait, err)
		}
		if calls != 1 || state(id) != "pending" || state(next) != "pending" {
			t.Fatal("destination cooldown bypassed")
		}
		exec(`UPDATE developer_events SET attempts=5,next_at=now() WHERE id=$1`, id)
		client.Transport = webhookRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})
		short := &webhookBackoff{}
		s.deliverWebhook(ctx, id, client, short)
		if short.ready(time.Now()) || !short.ready(time.Now().Add(6*time.Second)) {
			t.Fatal("one old retry blocked the whole endpoint for hours")
		}
	})
	exec("DELETE FROM developer_events")
	t.Run("inbound progresses while status slots are blocked", func(t *testing.T) {
		for i := 0; i < 40; i++ {
			emit("message.status_changed", fmt.Sprintf("out-%d", i))
		}
		var active atomic.Int32
		inbound := make(chan struct{}, 16)
		client := &http.Client{Transport: webhookRoundTrip(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"type": "message.received"`) || strings.Contains(string(body), `"type":"message.received"`) {
				inbound <- struct{}{}
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			active.Add(1)
			defer active.Add(-1)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		run, cancel := context.WithTimeout(ctx, 8*time.Second)
		done := make(chan struct{})
		go func() { defer close(done); s.runWebhooks(run, client) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(5 * time.Second)
		for active.Load() != 24 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if active.Load() != 24 {
			t.Fatalf("status slots=%d, want 24", active.Load())
		}
		for i := 0; i < 8; i++ {
			emit("message.received", fmt.Sprintf("in-%d", i))
		}
		for i := 0; i < 8; i++ {
			select {
			case <-inbound:
			case <-time.After(2 * time.Second):
				t.Fatal("inbound starved behind status")
			}
		}
		if active.Load() != 24 {
			t.Fatal("status capacity was not bounded")
		}
	})
	exec("DELETE FROM developer_events")
	t.Run("mixed backlog drains without dropping inbox events", func(t *testing.T) {
		for i := 0; i < 150; i++ {
			for j := 0; j < 3; j++ {
				emit("message.status_changed", fmt.Sprintf("mixed-%d", i))
			}
		}
		for i := 0; i < 100; i++ {
			emit("message.received", fmt.Sprintf("received-%d", i))
		}
		delay := 20 * time.Millisecond
		if ms, err := strconv.Atoi(os.Getenv("TEST_WEBHOOK_DELAY_MS")); err == nil && ms > 0 {
			delay = time.Duration(min(ms, 1200)) * time.Millisecond
		}
		var calls atomic.Int32
		client := &http.Client{Transport: webhookRoundTrip(func(r *http.Request) (*http.Response, error) {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			calls.Add(1)
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		run, cancel := context.WithTimeout(ctx, 40*time.Second)
		done := make(chan struct{})
		started := time.Now()
		go func() { defer close(done); s.runWebhooks(run, client) }()
		defer func() { cancel(); <-done }()
		for {
			var remaining int
			if err := s.db.QueryRow(ctx, `SELECT count(*) FROM developer_events WHERE state IN ('pending','sending')`).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining == 0 {
				break
			}
			select {
			case <-run.Done():
				t.Fatal("backlog did not drain", remaining)
			case <-time.After(20 * time.Millisecond):
			}
		}
		var inbox, completed, merged int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE event_type='message.received' AND state='delivered'),
 count(*) FILTER(WHERE state='delivered'),count(*) FILTER(WHERE issue='STATUS_SUPERSEDED') FROM developer_events`).Scan(&inbox, &completed, &merged); err != nil {
			t.Fatal(err)
		}
		if inbox != 100 || completed != 250 || merged != 300 || calls.Load() != 250 {
			t.Fatal("lost or duplicate event", inbox, completed, merged, calls.Load())
		}
		t.Logf("550 events: 300 superseded, 250 callbacks, 100 inbox; callback delay %s; drained in %s", delay, time.Since(started))
	})
}
