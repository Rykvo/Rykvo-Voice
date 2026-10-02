package main

import (
	"context"
	"testing"
	"time"

	"rykvo.local/auth/internal/telephony"
)

func testIncomingSharedRecordDatabase(t *testing.T, s *server, account string) {
	t.Helper()
	ctx := context.Background()
	c := &sipIncoming{owner: newSIPGateway(s).calls, call: telephony.Call{Module: "module-03"}}
	a := &sipIncomingLeg{owner: c, reg: telephony.Registration{ID: 1, Account: account}}
	b := &sipIncomingLeg{owner: c, reg: telephony.Registration{ID: 2, Account: account}}
	c.legs = map[telephony.ID]*sipIncomingLeg{1: a, 2: b}
	for _, l := range []*sipIncomingLeg{a, b} {
		var err error
		l.record, err = c.recordOffer(ctx, account, "+12025550123")
		if err != nil {
			t.Fatal(err)
		}
	}
	if a.record != b.record {
		t.Fatal("two phones created duplicate account records")
	}
	a.outcome, a.ended = "rejected", time.Now()
	a.finished.Store(true)
	a.peerClosed.Store(true)
	c.finishAccountRecord(account)
	var state, reason string
	var answered, ended *time.Time
	read := func() {
		t.Helper()
		if err := s.db.QueryRow(ctx, `SELECT state,outcome,answered_at,ended_at FROM sip_call_records WHERE id=$1`, a.record).Scan(&state, &reason, &answered, &ended); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if state != "ringing" || reason != "" || answered != nil || ended != nil {
		t.Fatal("one rejection ended another phone's offer", state, reason)
	}
	b.recordConnected()
	read()
	if state != "connected" || answered == nil || ended != nil {
		t.Fatal("winner not connected", state)
	}
	b.outcome, b.ended = "remote_cancelled", time.Now().Truncate(time.Microsecond)
	b.finished.Store(true)
	c.finishAccountRecord(account)
	read()
	if state != "cleanup_pending" || reason != "remote_cancelled" || ended == nil || !ended.Equal(b.ended) {
		t.Fatal("winner final result lost", state, reason, ended)
	}
	b.peerClosed.Store(true)
	c.finalizeRecords()
	read()
	if state != "ended" || reason != "remote_cancelled" || answered == nil || !ended.Equal(b.ended) {
		t.Fatal("cleanup changed billing evidence", state, reason, ended)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM sip_call_records WHERE id=$1`, a.record); err != nil {
		t.Fatal(err)
	}
}

func TestSIPGatewayIncomingAccountRecordAggregation(t *testing.T) {
	at := time.Now()
	rejected := &sipIncomingLeg{outcome: "rejected", ended: at}
	winner := &sipIncomingLeg{outcome: "cancelled", ended: at.Add(time.Minute)}
	winner.selected.Store(true)
	for _, legs := range [][]*sipIncomingLeg{{rejected, winner}, {winner, rejected}} {
		reason, ended := incomingRecordResult(legs)
		if reason != "cancelled" || !ended.Equal(winner.ended) {
			t.Fatal("losing phone overwrote winner", reason, ended)
		}
	}
	loser := &sipIncomingLeg{outcome: "answered_elsewhere", ended: at.Add(time.Second)}
	reason, _ := incomingRecordResult([]*sipIncomingLeg{rejected, loser})
	if reason != "answered_elsewhere" {
		t.Fatal(reason)
	}
	winner.outcome = "module_error"
	reason, _ = incomingRecordResult([]*sipIncomingLeg{loser, winner})
	if reason != "module_error" {
		t.Fatal("same-account loser masked carrier answer failure", reason)
	}
}

func TestSIPGatewayIncomingAnswerTimeoutIsNotMissedCall(t *testing.T) {
	for _, selected := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		leg := &sipIncomingLeg{ctx: ctx, cancel: cancel}
		leg.selected.Store(selected)
		leg.stop("no_answer")
		want := "no_answer"
		if selected {
			want = "call_timeout"
		}
		if reason, at := incomingRecordResult([]*sipIncomingLeg{leg}); reason != want || at.IsZero() {
			t.Fatal("answer timeout misclassified", selected, reason, at)
		}
	}
}
