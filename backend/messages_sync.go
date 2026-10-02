package main

import (
	"context"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type messageSync struct {
	tx              pgx.Tx
	after, snapshot int64
}

// One committed snapshot per page; purged tombstones require a fresh baseline.
func (s *server) beginMessageSync(ctx context.Context, q url.Values) (*messageSync, error) {
	read := func(key string) (int64, error) {
		if !q.Has(key) {
			return 0, nil
		}
		n, err := strconv.ParseInt(q.Get(key), 10, 64)
		if err != nil || n < 0 {
			return 0, rejected(400, "INVALID_CURSOR")
		}
		return n, nil
	}
	after, err := read("after")
	if err != nil {
		return nil, err
	}
	snapshot, err := read("snapshot")
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			tx.Rollback(ctx)
		}
	}()
	var floor, head int64
	if err = tx.QueryRow(ctx, "SELECT floor,GREATEST(floor,COALESCE((SELECT max(revision) FROM messages),0),COALESCE((SELECT max(revision) FROM message_threads),0)) FROM message_sync_state").Scan(&floor, &head); err != nil {
		return nil, err
	}
	if snapshot > head || after > head || snapshot > 0 && after > snapshot {
		return nil, rejected(400, "INVALID_CURSOR")
	}
	if snapshot > 0 && snapshot < floor || snapshot == 0 && after > 0 && after < floor {
		return nil, rejected(410, "CURSOR_EXPIRED")
	}
	if snapshot == 0 {
		snapshot = head
	}
	complete = true
	return &messageSync{tx: tx, after: after, snapshot: snapshot}, nil
}

func (p *messageSync) cursor(last int64, more bool) int64 {
	if more {
		return last
	}
	return p.snapshot
}
