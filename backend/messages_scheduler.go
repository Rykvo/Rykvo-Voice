package main

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"rykvo.local/auth/internal/dispatch"
	"rykvo.local/auth/internal/durable"
)

const messageSendConcurrency = 64
const messagePriorityOrder = "(COALESCE(metadata->>'bulk','false')='true'),updated_at,id"

type messageRuntime struct {
	mu           sync.RWMutex
	state        messageRuntimeState
	receiptIssue string
	inboxIssues  map[int64]string
}

func (r *messageRuntime) inbox(id int64, err error) {
	issue := ""
	if err != nil {
		issue = "INBOX_READ_FAILED"
		for _, code := range []string{"DEVICE_CHANGED", "DEVICE_BUSY", "READ_TIMEOUT", "NO_SIM", "SMS_NOT_READY", "COMMAND_UNSUPPORTED", "INVALID_RESPONSE", "INVALID_MESSAGE", "SMS_RECORD_REJECTED", "SMS_STORAGE_UNAVAILABLE", "SMS_STORAGE_CHANGED", "PERMISSION_DENIED"} {
			if err.Error() == code {
				issue = code
				break
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inboxIssues[id] == issue {
		return
	}
	if r.inboxIssues == nil {
		r.inboxIssues = make(map[int64]string)
	}
	if issue == "" {
		delete(r.inboxIssues, id)
		log.Printf("message inbox: module=%d recovered", id)
	} else {
		r.inboxIssues[id] = issue
		log.Printf("message inbox: module=%d issue=%s", id, issue)
	}
}

type messageRuntimeState struct {
	Ready     bool      `json:"ready"`
	Issue     string    `json:"issue"`
	CheckedAt time.Time `json:"checkedAt"`
	Sending   int       `json:"sending"`
	Receiving int       `json:"receiving"`
}

func (r *messageRuntime) update(state messageRuntimeState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.state.Issue != state.Issue
	r.state = state
	if changed && state.Issue != "" {
		log.Printf("message worker: %s", state.Issue)
	}
}

func (r *messageRuntime) snapshot() messageRuntimeState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.state
	if state.Issue == "" && r.receiptIssue != "" {
		state.Issue = r.receiptIssue
		state.Ready = false
	}
	return state
}

func (r *messageRuntime) receipts(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	issue := ""
	if err != nil {
		issue = "MESSAGE_RECEIPT_UPDATE_FAILED"
	}
	if issue != "" && r.receiptIssue != issue {
		log.Printf("message worker: %s", issue)
	}
	r.receiptIssue = issue
}

func (m *moduleManager) prepareMessages(ctx context.Context) error {
	if err := m.messageResults.Prepare(); err != nil {
		return err
	}
	if err := m.messageResults.Replay(ctx, m.applyMessageOutcome); err != nil && (!errors.Is(err, durable.ErrCorrupt) || errors.Is(err, durable.ErrPending)) {
		return err
	}
	if err := m.repairMMSNotifications(ctx); err != nil {
		return err
	}
	// Missing delivery receipts never revoke a confirmed carrier acceptance.
	if _, err := m.db.Exec(ctx, `UPDATE messages SET state='accepted',issue=''
 WHERE mine AND deleted_at IS NULL AND state='unknown' AND
 ((kind='sms' AND issue='SMS_DELIVERY_UNCONFIRMED') OR (kind='mms' AND issue='MMS_DELIVERY_UNCONFIRMED'))`); err != nil {
		return err
	}
	// A crashed POST may have reached the carrier. Only inbound GETs are resumed.
	if _, err := m.db.Exec(ctx, "UPDATE messages SET state='unknown',issue='SEND_INTERRUPTED' WHERE mine AND state='sending'"); err != nil {
		return err
	}
	return m.resumeMMSDownloads(ctx)
}

func (m *moduleManager) runMessages(ctx context.Context) {
	defer func() {
		m.messageRuntime.update(messageRuntimeState{Issue: "MESSAGE_WORKER_STOPPED", CheckedAt: time.Now().UTC()})
	}()
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := m.prepareMessages(call)
		cancel()
		if err == nil {
			break
		}
		delay, issue := 5*time.Second, "MESSAGE_INITIALIZATION_FAILED"
		if errors.Is(err, durable.ErrPending) {
			delay, issue = 250*time.Millisecond, "MESSAGE_RESULT_RECOVERY_PENDING"
		}
		m.messageRuntime.update(messageRuntimeState{Issue: issue, CheckedAt: time.Now().UTC()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	sends := dispatch.New[int64](ctx, messageSendConcurrency)
	inboxes := dispatch.New[int64](ctx, 8)
	receipts := dispatch.New[int](ctx, 1)
	var receiptPoll receiptPoller
	defer sends.Close()
	defer inboxes.Close()
	defer receipts.Close()
	cursor := 0
	var sendCursor, replyCursor int64
	inboxAfter := map[int64]time.Time{}
	sendAfter := map[int64]time.Time{}
	dispatchSends := func(ctx context.Context) error {
		if m.isDraining() {
			return nil
		}
		ids, priority, err := m.pendingMessageModules(ctx)
		if err == nil {
			now := time.Now()
			pending := make(map[int64]bool, len(ids))
			urgent := make(map[int64]bool, len(priority))
			for _, id := range priority {
				urgent[id] = true
			}
			for _, id := range append(fairMessageModules(priority, replyCursor), fairMessageModules(ids, sendCursor)...) {
				pending[id] = true
				if !now.Before(sendAfter[id]) && sends.Start(id, func(ctx context.Context) { m.processMessage(ctx, id) }) {
					sendAfter[id] = now.Add(250 * time.Millisecond)
					if urgent[id] {
						replyCursor = id
					} else {
						sendCursor = id
					}
				}
			}
			for id := range sendAfter {
				if !pending[id] {
					delete(sendAfter, id)
				}
			}
		}
		return err
	}
	tick := func() {
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := m.messageResults.Replay(call, m.applyMessageOutcome)
		recoveryIssue := errors.Is(err, durable.ErrCorrupt)
		recoveryPending := errors.Is(err, durable.ErrPending)
		if recoveryIssue || recoveryPending {
			err = nil
		}
		if err == nil {
			err = dispatchSends(call)
		}
		cancel()
		status := messageRuntimeState{Ready: err == nil, CheckedAt: time.Now().UTC(), Sending: sends.Active(), Receiving: inboxes.Active()}
		if err != nil {
			status.Issue = "MESSAGE_DATABASE_UNAVAILABLE"
		} else {
			m.scheduleInboxes(inboxes, &cursor, inboxAfter)
			receipts.Start(0, func(ctx context.Context) {
				call, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_, err := receiptPoll.refresh(call, m, time.Now())
				if ctx.Err() == nil {
					m.messageRuntime.receipts(err)
				}
			})
		}
		if recoveryPending && err == nil {
			status.Ready = false
			status.Issue = "MESSAGE_RESULT_RECOVERY_PENDING"
		}
		if recoveryIssue && err == nil {
			status.Ready = false
			status.Issue = "MESSAGE_RESULT_RECOVERY_FAILED"
		}
		m.messageRuntime.update(status)
	}
	tick()
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			tick()
		case <-sends.Changed():
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := dispatchSends(call)
			cancel()
			if err != nil {
				m.messageRuntime.update(messageRuntimeState{Issue: "MESSAGE_DATABASE_UNAVAILABLE", CheckedAt: time.Now()})
			}
		case <-inboxes.Changed():
			m.scheduleInboxes(inboxes, &cursor, inboxAfter)
		}
	}
}

func (m *moduleManager) pendingMessageModules(ctx context.Context) ([]int64, []int64, error) {
	rows, err := m.db.Query(ctx, `SELECT module_id,bool_or(COALESCE(metadata->>'bulk','false')<>'true') FROM messages
 WHERE deleted_at IS NULL AND (state IN ('queued','download_pending')
 OR (state='waiting_network' AND updated_at<now()-interval '30 seconds'))
 GROUP BY module_id ORDER BY min(updated_at),module_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids, priority []int64
	for rows.Next() {
		var id int64
		var urgent bool
		if err := rows.Scan(&id, &urgent); err != nil {
			return nil, nil, err
		}
		if urgent {
			priority = append(priority, id)
		} else {
			ids = append(ids, id)
		}
	}
	return ids, priority, rows.Err()
}

func (m *moduleManager) scheduleInboxes(group *dispatch.Group[int64], cursor *int, after map[int64]time.Time) {
	if _, ok := m.source.(cellularMessageTransport); !ok || m.isDraining() {
		return
	}
	now := time.Now()
	m.mu.RLock()
	ids := make([]int64, 0, len(m.values))
	for id := range m.values {
		ids = append(ids, id)
	}
	for id := range after {
		if _, ok := m.values[id]; !ok {
			delete(after, id)
		}
	}
	m.mu.RUnlock()
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	start := *cursor % len(ids)
	for i := range len(ids) {
		index := (start + i) % len(ids)
		id := ids[index]
		if now.Before(after[id]) {
			continue
		}
		if group.Start(id, func(ctx context.Context) { m.pollCellularInbox(ctx, id) }) {
			after[id] = now.Add(2 * time.Second)
			*cursor = (index + 1) % len(ids)
		}
	}
}

// Resume after the last dispatched module, even when an earlier module has a backlog.
func fairMessageModules(ids []int64, after int64) []int64 {
	ordered := append([]int64(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	start := sort.Search(len(ordered), func(i int) bool { return ordered[i] > after })
	return append(ordered[start:], ordered[:start]...)
}
