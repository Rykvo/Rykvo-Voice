package main

import (
	"context"
	"time"
)

const messageResultTimeout = 30 * time.Minute

// A timeout closes tracking, not proof of carrier rejection. Never requeue it.
func (m *moduleManager) expireMessageResults(ctx context.Context, now time.Time) error {
	_, err := m.db.Exec(ctx, `WITH stale AS (
 SELECT id FROM messages WHERE mine AND deleted_at IS NULL AND (
   (state IN ('sending','unknown') AND COALESCE(operation_at,created_at)<=$1)
   OR (state IN ('queued','waiting_network') AND expires_at<=$2))
 ORDER BY COALESCE(operation_at,created_at),id LIMIT 200 FOR UPDATE SKIP LOCKED
 ) UPDATE messages m SET
 state='failed',
 issue=CASE WHEN m.state IN ('queued','waiting_network') THEN 'MESSAGE_EXPIRED' ELSE 'RESULT_TIMEOUT' END
 FROM stale WHERE m.id=stale.id AND m.state IN ('queued','waiting_network','sending','unknown')`,
		now.Add(-messageResultTimeout), now)
	return err
}
