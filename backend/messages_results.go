package main

import (
	"context"
	"encoding/json"
	"log"
	"rykvo.local/auth/internal/durable"
)

type messageOutcome struct {
	ID               string          `json:"id"`
	State            string          `json:"state"`
	Issue            string          `json:"issue"`
	Result           json.RawMessage `json:"result,omitempty"`
	Body             *string         `json:"body,omitempty"`
	Image            *string         `json:"image,omitempty"`
	Peer             *string         `json:"peer,omitempty"`
	DownloadAttempts *int            `json:"downloadAttempts,omitempty"`
}

func (m *moduleManager) applyMessageOutcome(ctx context.Context, data []byte) error {
	var v messageOutcome
	if json.Unmarshal(data, &v) != nil || v.ID == "" {
		return durable.ErrCorrupt
	}
	switch v.State {
	case "waiting_network", "expired", "cancelled", "failed", "unknown", "accepted", "partial", "received":
	default:
		return durable.ErrCorrupt
	}
	var result any
	if len(v.Result) > 0 {
		result = []byte(v.Result)
	}
	_, err := m.db.Exec(ctx, `UPDATE messages SET state=$2,issue=$3,result=COALESCE($4::jsonb,result),
 body=COALESCE($5,body),image=COALESCE($6,image),peer=COALESCE($7,peer),
 metadata=CASE WHEN $8::int IS NULL THEN metadata ELSE jsonb_set(metadata,'{downloadAttempts}',to_jsonb($8::int)) END
 WHERE id=$1 AND deleted_at IS NULL AND
 (state IN ('queued','download_pending','waiting_network','sending','downloading') OR (state='unknown' AND issue='SEND_INTERRUPTED')
 OR (state='failed' AND issue='RESULT_TIMEOUT' AND $2 IN ('accepted','partial','failed') AND $3<>'RESULT_TIMEOUT'))`,
		v.ID, v.State, v.Issue, result, v.Body, v.Image, v.Peer, v.DownloadAttempts)
	return err
}

func (m *moduleManager) saveMessageOutcome(ctx context.Context, v messageOutcome) error {
	data, err := json.Marshal(v)
	if err == nil {
		err = m.messageResults.Apply(ctx, v.ID, data, m.applyMessageOutcome)
	}
	if err != nil {
		log.Printf("message result pending: id=%s", v.ID)
	}
	return err
}

func (m *moduleManager) messageState(ctx context.Context, id, state, issue string, result any) error {
	v := messageOutcome{ID: id, State: state, Issue: issue}
	if result != nil {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		v.Result = data
	}
	return m.saveMessageOutcome(ctx, v)
}
