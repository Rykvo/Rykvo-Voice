package main

import (
	"context"
	"time"
)

type incomingAccountRecord struct {
	id, outcome string
	ended       time.Time
	finished    bool
}

func (c *sipIncoming) recordOffer(ctx context.Context, account, number string) (string, error) {
	c.recordsMu.Lock()
	defer c.recordsMu.Unlock()
	if record := c.records[account]; record != nil {
		return record.id, nil
	}
	id, err := c.owner.g.server.startIncomingRecord(ctx, account, c.call.Module, number)
	if err != nil {
		return "", err
	}
	if c.records == nil {
		c.records = make(map[string]*incomingAccountRecord)
	}
	c.records[account] = &incomingAccountRecord{id: id}
	return id, nil
}

// Wait for all phones of this account; one rejected fork cannot end its winner's row.
func (c *sipIncoming) finishAccountRecord(account string) {
	c.recordsMu.Lock()
	record := c.records[account]
	if record == nil || record.finished {
		c.recordsMu.Unlock()
		return
	}
	var candidates []*sipIncomingLeg
	for _, l := range c.legs {
		if l.reg.Account != account {
			continue
		}
		if !l.finished.Load() {
			c.recordsMu.Unlock()
			return
		}
		candidates = append(candidates, l)
	}
	outcome, ended := incomingRecordResult(candidates)
	record.outcome, record.ended, record.finished = outcome, ended, true
	id := record.id
	state := "ended"
	for _, l := range candidates {
		if !l.peerClosed.Load() {
			state = "cleanup_pending"
		}
	}
	// Final cleanup must not overtake this write and then be downgraded to pending.
	c.owner.g.server.endCallRecord(id, c.call.Module, state, outcome, ended)
	c.recordsMu.Unlock()
}

func incomingRecordResult(legs []*sipIncomingLeg) (string, time.Time) {
	// The selected phone owns the result, including failure before carrier answer.
	priority := map[string]int{"answered_elsewhere": 6, "remote_cancelled": 5, "no_answer": 4, "rejected": 3, "busy": 2}
	reason, rank := "failed", -1
	var ended time.Time
	for _, l := range legs {
		l.mu.Lock()
		outcome, at := l.outcome, l.ended
		l.mu.Unlock()
		if l.selected.Load() {
			return outcome, at
		}
		if at.After(ended) {
			ended = at
		}
		if priority[outcome] > rank {
			reason, rank = outcome, priority[outcome]
		}
	}
	return reason, ended
}

func (c *sipIncoming) finalizeRecords() {
	c.recordsMu.Lock()
	var records []incomingAccountRecord
	for _, record := range c.records {
		if record.finished {
			records = append(records, *record)
		}
	}
	c.recordsMu.Unlock()
	for _, record := range records {
		c.owner.g.server.endCallRecord(record.id, c.call.Module, "ended", record.outcome, record.ended)
	}
}
