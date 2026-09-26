package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"rykvo.local/auth/internal/hardware"
	"rykvo.local/auth/internal/mms"
)

func mmsPeer(from, fallback string) string {
	peer := strings.TrimSuffix(from, "/TYPE=PLMN")
	if hardware.ValidSMS(peer, "x") {
		return peer
	}
	return fallback
}

func decodeMMSPush(raw, gateway string) (state, peer string, metadata map[string]any) {
	state, peer = "unsupported_push", gateway
	metadata = map[string]any{"pushVersion": 1, "gateway": gateway}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "decode_error", peer, metadata
	}
	p, err := mms.Push(b)
	if err != nil {
		return
	}
	metadata["pushType"] = p.Type
	switch p.Type {
	case 0x82:
		if p.Location == "" || p.Transaction == "" {
			return
		}
		state, peer = "download_pending", mmsPeer(p.From, gateway)
		metadata["location"], metadata["transaction"] = p.Location, p.Transaction
	case 0x86:
		// Network delivery reports are protocol records, not empty chat messages.
		if p.MessageID == "" || p.To == "" {
			return
		}
		state = "mms_report"
		metadata["messageId"], metadata["recipient"], metadata["status"] = p.MessageID, p.To, p.Status
	}
	return
}

// Reinterpret preserved raw notifications; never resend or overwrite received content.
func (m *moduleManager) repairMMSNotifications(ctx context.Context) error {
	rows, err := m.db.Query(ctx, `SELECT m.id,m.peer,string_agg(p.body,'' ORDER BY p.sequence) FROM messages m JOIN message_parts p ON p.message_id=m.id WHERE NOT m.mine AND m.kind='mms' AND m.deleted_at IS NULL AND (m.state IN ('unsupported_push','waiting_network','download_pending') OR (m.state='failed' AND m.issue='MMS_LOCATION_UNSUPPORTED')) AND m.metadata->>'pushVersion' IS DISTINCT FROM '1' GROUP BY m.id ORDER BY m.created_at DESC LIMIT 200`)
	if err != nil {
		return err
	}
	type record struct{ id, peer, raw string }
	var records []record
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.peer, &r.raw); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range records {
		state, peer, meta := decodeMMSPush(r.raw, r.peer)
		data, _ := json.Marshal(meta)
		_, err = m.db.Exec(ctx, `UPDATE messages SET state=$2,peer=$3,metadata=$4,issue='' WHERE id=$1 AND NOT mine AND (state IN ('unsupported_push','waiting_network','download_pending') OR (state='failed' AND issue='MMS_LOCATION_UNSUPPORTED')) AND deleted_at IS NULL`, r.id, state, peer, data)
		if err != nil {
			return err
		}
	}
	return nil
}
