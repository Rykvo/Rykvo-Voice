package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func testDeveloperMessageParity(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE developer_settings SET key_hash=decode(repeat('ab',32),'hex')")
	var mid int64
	err = tx.QueryRow(ctx, `INSERT INTO modules(hardware_key,endpoint,kind,label,label_key,active_card,card_epoch) VALUES('parity','parity','fixture','Parity','parity','8986000000000090011',1) RETURNING id`).Scan(&mid)
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	for _, kind := range []string{"sms", "mms"} {
		for _, state := range []string{"queued", "sending", "waiting_network", "accepted", "delivered", "failed", "unknown", "expired", "cancelled", "partial", "receiving", "download_pending", "downloading", "received", "decode_error", "unsupported_push"} {
			index++
			id := fmt.Sprintf("parity-message-%03d", index)
			mine := state != "receiving" && state != "download_pending" && state != "downloading" && state != "received" && state != "decode_error" && state != "unsupported_push"
			exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,body,image,state) VALUES($1,$2,'8986000000000090011','fixture','+12025550123',$3,$4,'fixture','fixture',$5)`, id, mid, mine, kind, state)
			for _, deleted := range []bool{false, true} {
				if deleted {
					exec("UPDATE messages SET deleted_at=now() WHERE id=$1", id)
				}
				row, err := scanMessage(tx.QueryRow(ctx, "SELECT "+messageColumns+" FROM messages WHERE id=$1", id))
				if err != nil {
					t.Fatal(err)
				}
				var epoch int64
				if err = tx.QueryRow(ctx, "SELECT alert_epoch FROM messages WHERE id=$1", id).Scan(&epoch); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(developerMessageView(row, epoch))
				var api, event map[string]any
				if err = json.Unmarshal(raw, &api); err != nil {
					t.Fatal(err)
				}
				if err = tx.QueryRow(ctx, "SELECT data->'data' FROM developer_events WHERE resource_id=$1 ORDER BY id DESC LIMIT 1", id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &event); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"id", "moduleId", "lineId", "cardVersion", "revision", "state", "statusText", "displayStatus", "carrierAccepted", "deliveryConfirmed", "mine", "kind", "text", "deleted"} {
					if !reflect.DeepEqual(api[key], event[key]) {
						t.Fatalf("%s/%s deleted=%v %s: API=%v webhook=%v", kind, state, deleted, key, api[key], event[key])
					}
				}
				if deleted && (api["image"] != "" || event["attachmentPath"] != "") {
					t.Fatal("deleted message exposed attachment")
				}
			}
		}
	}
}
