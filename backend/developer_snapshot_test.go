package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"rykvo.local/auth/internal/hardware"
)

func TestDeveloperNumberUsesOneSnapshot(t *testing.T) {
	for _, test := range []struct {
		name, seen, committed, want string
	}{
		{"current", "card-a", "card-a", "+12025550101"},
		{"old-view", "card-a", "card-b", ""},
		{"old-database", "card-b", "card-a", ""},
		{"unconfirmed", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := map[string]any{"number": "+12025550101", "hardware": hardware.Reading{ICCID: test.seen, Number: "+12025550101"}}
			if got := developerModuleNumber(view, test.committed); got != test.want {
				t.Fatalf("number=%q want=%q", got, test.want)
			}
		})
	}
	if developerModuleNumber(map[string]any{"number": "+12025550101"}, "card-a") != "" {
		t.Fatal("number published without its card identity")
	}
}

type developerSnapshotTrace struct {
	once       sync.Once
	switchCard func()
}

func (t *developerSnapshotTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT id,card_epoch,active_card FROM modules") {
		t.once.Do(t.switchCard)
	}
	return ctx
}
func (*developerSnapshotTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func testDeveloperModuleSnapshotDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	sample := wifiModuleFixture()
	sample.Candidate.Key, sample.Reading.IMEI = "usb:developer-snapshot", "990000000008899"
	sample.Reading.Number, sample.Reading.SIM = "+12025550101", "READY"
	v, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(ctx, "DELETE FROM modules WHERE id=$1", v.ID)
	m := newModuleManager(s.db, nil)
	m.ready, m.lastScan = true, time.Now()
	m.values[v.ID], m.seen[v.Endpoint] = sample, sample.Candidate
	var oldEpoch int64
	if err = s.db.QueryRow(ctx, "SELECT card_epoch FROM modules WHERE id=$1", v.ID).Scan(&oldEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(ctx, "SELECT alert_observe($1,$2,'sms','snapshot-failure',false,'SMS_REJECTED',clock_timestamp())", v.ID, oldEpoch); err != nil {
		t.Fatal(err)
	}
	config := s.db.Config()
	config.ConnConfig.Tracer = &developerSnapshotTrace{switchCard: func() {
		// Switch after counters were read, before reading their card version.
		sample.Reading.ICCID, sample.Reading.Number = "89123456789012345679", "+12025550102"
		if _, err := bindModule(ctx, s.db, sample.Candidate, sample.Reading); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		m.values[v.ID] = sample
		m.mu.Unlock()
	}}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	api := &server{db: pool, modules: m}
	for index := 0; index < 2; index++ {
		items, err := api.developerModules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range items {
			if item["id"] != moduleID(v.ID) {
				continue
			}
			found = true
			alerts := item["alerts"].([]moduleAlert)
			if index == 0 {
				if item["cardVersion"] != oldEpoch || item["number"] != "" || len(alerts) != 1 || alerts[0].Failures != 1 {
					t.Fatalf("mixed card/counter snapshot: %+v", item)
				}
			} else if item["cardVersion"] != oldEpoch+1 || item["number"] != sample.Reading.Number || len(alerts) != 0 {
				t.Fatalf("next snapshot missed committed card: %+v", item)
			}
		}
		if !found {
			t.Fatal("module missing")
		}
	}
}
