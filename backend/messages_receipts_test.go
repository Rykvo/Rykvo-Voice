package main

import (
	"context"
	"testing"
	"time"
)

func testReceiptPollingDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	m := newModuleManager(s.db, nil)
	var p receiptPoller
	now := time.Now()
	if ran, err := p.refresh(ctx, m, now); err != nil || !ran {
		t.Fatal("initial reconciliation", ran, err)
	}
	// Applying receipts can itself advance a revision once; stabilize before idle checks.
	if _, err := p.refresh(ctx, m, now); err != nil {
		t.Fatal(err)
	}
	if ran, err := p.refresh(ctx, m, now); err != nil || ran {
		t.Fatal("unchanged receipts rescanned", ran, err)
	}
	previous := p
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.refresh(cancelled, m, now.Add(time.Minute)); err == nil || p != previous {
		t.Fatal("failed scan advanced receipt watermark")
	}
	if ran, err := p.refresh(ctx, m, now.Add(time.Minute)); err != nil || !ran {
		t.Fatal("window expiry not reconciled", ran, err)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO message_reports(iccid,fingerprint,peer,reference,status) VALUES('8986000000000090011','receipt-poll-fixture','+12025550123',17,0)`)
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := p.refresh(ctx, m, now.Add(time.Minute)); err != nil || !ran {
		t.Fatal("new report missed", ran, err)
	}
	if _, err = s.db.Exec(ctx, "DELETE FROM message_reports WHERE fingerprint='receipt-poll-fixture'"); err != nil {
		t.Fatal(err)
	}
}
