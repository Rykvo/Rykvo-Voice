package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
)

func TestMMSNotificationUsesSenderNotGateway(t *testing.T) {
	b := []byte{1, 6, 1, 0xbe, 0x8c, 0x82, 0x8d, 0x92, 0x98, 't', 0, 0x89, 23, 0x80}
	b = append(b, []byte("12025550123/TYPE=PLMN\x00")...)
	// From length is token plus encoded address, not the SMS gateway length.
	b[12] = byte(len("12025550123/TYPE=PLMN\x00") + 1)
	b = append(b, 0x83)
	b = append(b, []byte("http://mpc.t-mobile.com/test\x00")...)
	state, peer, meta := decodeMMSPush(hex.EncodeToString(b), "2300")
	if state != "download_pending" || peer != "12025550123" || meta["gateway"] != "2300" {
		t.Fatal(state, peer, meta)
	}
}

func testMMSDownloadResume(t *testing.T, ctx context.Context, m *moduleManager, module int64) {
	t.Helper()
	defer m.db.Exec(ctx, "DELETE FROM messages WHERE line_id='mms-resume-fixture'")
	for n, tc := range []struct {
		mine                           bool
		state, issue, image, age, want string
		deleted                        bool
	}{
		{false, "failed", "MMS_LOCATION_UNSUPPORTED", "", "0 hours", "download_pending", false},
		{false, "downloading", "", "", "0 hours", "download_pending", false},
		{true, "failed", "MMS_LOCATION_UNSUPPORTED", "", "0 hours", "failed", false},
		{true, "unknown", "MMS_OUTCOME_UNKNOWN", "", "0 hours", "unknown", false},
		{true, "accepted", "", "", "0 hours", "accepted", false},
		{false, "received", "", "fixture", "0 hours", "received", false},
		{false, "failed", "MMS_HTTP_403", "", "0 hours", "failed", false},
		{false, "failed", "MMS_LOCATION_UNSUPPORTED", "fixture", "0 hours", "failed", false},
		{false, "failed", "MMS_LOCATION_UNSUPPORTED", "", "48 hours", "failed", false},
		{false, "failed", "MMS_LOCATION_UNSUPPORTED", "", "0 hours", "failed", true},
	} {
		id := fmt.Sprintf("mms-resume-%d", n)
		_, err := m.db.Exec(ctx, `INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,issue,image,created_at,deleted_at) VALUES($1,$2,'resume-card','mms-resume-fixture','123', $3,'mms',$4,$5,$6,now()-$7::interval,CASE WHEN $8 THEN now() ELSE NULL END)`, id, module, tc.mine, tc.state, tc.issue, tc.image, tc.age, tc.deleted)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.resumeMMSDownloads(ctx); err != nil {
			t.Fatal(err)
		}
		var state string
		if err = m.db.QueryRow(ctx, "SELECT state FROM messages WHERE id=$1", id).Scan(&state); err != nil || state != tc.want {
			t.Fatal(id, state, tc.want, err)
		}
	}
}
func TestMMSDeliveryReportIsNotEmptyMessage(t *testing.T) {
	b := []byte{1, 6, 1, 0xbe, 0x8c, 0x86, 0x8d, 0x91, 0x8b, 'i', 'd', 0, 0x97}
	b = append(b, []byte("+12025550123/TYPE=PLMN\x00")...)
	b = append(b, 0x95, 0x82)
	state, _, meta := decodeMMSPush(hex.EncodeToString(b), "106581570001")
	if state != "mms_report" || meta["status"] != byte(0x82) || meta["messageId"] != "id" {
		t.Fatal(state, meta)
	}
}

func testMMSReportMatching(t *testing.T, ctx context.Context, m *moduleManager, module int64) {
	t.Helper()
	defer m.db.Exec(ctx, "DELETE FROM messages WHERE line_id='mms-report-fixture'")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := m.db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for n, tc := range []struct {
		status                         int
		card, recipient, network, want string
		duplicate                      bool
	}{
		{129, "report-card", "+12025550123/TYPE=PLMN", "network", "delivered", false},
		{130, "report-card", "12025550123/TYPE=PLMN", "network", "failed", false},
		{131, "report-card", "+12025550123/TYPE=PLMN", "network", "accepted", false},
		{129, "another-card", "+12025550123/TYPE=PLMN", "network", "accepted", false},
		{129, "report-card", "+12025550999/TYPE=PLMN", "network", "accepted", false},
		{129, "report-card", "+12025550123/TYPE=PLMN", "", "accepted", false},
		{129, "report-card", "+12025550123/TYPE=PLMN", "network", "accepted", true},
	} {
		id := fmt.Sprintf("mms-report-out-%d", n)
		network := tc.network
		if network != "" {
			network += fmt.Sprint(n)
		}
		exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,result) VALUES($1,$2,'report-card','mms-report-fixture','+12025550123',true,'mms','accepted',jsonb_build_object('messageId',$3::text))`, id, module, network)
		if tc.duplicate {
			exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,result) SELECT id||'-duplicate',module_id,iccid,line_id,peer,mine,kind,'delivered',result FROM messages WHERE id=$1`, id)
		}
		exec(`INSERT INTO messages(id,module_id,iccid,line_id,peer,mine,kind,state,metadata) VALUES($1,$2,$3,'mms-report-fixture','gateway',false,'mms','mms_report',jsonb_build_object('messageId',$4::text,'recipient',$5::text,'status',$6::int))`, id+"-report", module, tc.card, network, tc.recipient, tc.status)
		m.applyMMSReports(ctx)
		m.applyMMSReports(ctx)
		var state string
		if err := m.db.QueryRow(ctx, "SELECT state FROM messages WHERE id=$1", id).Scan(&state); err != nil || state != tc.want {
			t.Fatal(n, state, tc.want, err)
		}
	}
}
