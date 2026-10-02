package main

import (
	"context"
	"time"
)

type receiptPoller struct {
	messages, mms int64
	sms           time.Time
	checkedAt     time.Time
	expiredAt     time.Time
}

// Only changed submissions/reports need expensive receipt matching. Periodic
// reconciliation still handles the seven-day matching window advancing.
func (p *receiptPoller) refresh(ctx context.Context, m *moduleManager, now time.Time) (bool, error) {
	var next receiptPoller
	next.expiredAt = p.expiredAt
	if p.expiredAt.IsZero() || now.Sub(p.expiredAt) >= 10*time.Second {
		if err := m.expireMessageResults(ctx, now); err != nil {
			return false, err
		}
		next.expiredAt = now
	}
	err := m.db.QueryRow(ctx, `SELECT
  COALESCE((SELECT max(revision) FROM messages WHERE mine AND (state IN ('accepted','partial','unknown') OR (state='failed' AND issue='RESULT_TIMEOUT'))),0),
  COALESCE((SELECT max(revision) FROM messages WHERE NOT mine AND state='mms_report'),0),
  COALESCE((SELECT max(received_at) FROM message_reports),TIMESTAMPTZ 'epoch')`).Scan(&next.messages, &next.mms, &next.sms)
	if err != nil {
		return false, err
	}
	if !p.checkedAt.IsZero() && now.Sub(p.checkedAt) < time.Minute && p.messages == next.messages && p.mms == next.mms && p.sms.Equal(next.sms) {
		p.expiredAt = next.expiredAt
		return false, nil
	}
	if err = m.applySMSReports(ctx); err == nil {
		err = m.applyMMSReports(ctx)
	}
	if err != nil {
		return false, err
	}
	next.checkedAt = now
	*p = next
	return true, nil
}
