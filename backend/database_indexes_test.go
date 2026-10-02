package main

import (
	"context"
	"strings"
	"testing"
)

func testMaintenanceIndexesDatabase(t *testing.T, s *server) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Large synthetic history stays inside the isolated test transaction.
	_, err = tx.Exec(ctx, `
INSERT INTO sip_call_records(id,account_id,module_id,peer,state,outcome,started_at,ended_at)
SELECT 'index-'||n,'index-test','module-01','fixture','ended','completed',now()-interval '8 days',now()-interval '8 days'
FROM generate_series(1,50000) n;
INSERT INTO developer_events(id,event_type,resource_id,data,destination,secret,state)
SELECT -n,'fixture','resource-'||n,'{}','','',CASE WHEN n%2=0 THEN 'pending' ELSE 'delivered' END
FROM generate_series(1,50000) n;
INSERT INTO developer_uploads(id,content,created_at)
SELECT 'index-'||n,'fixture',now()-interval '3 hours' FROM generate_series(1,5000) n;
ANALYZE sip_call_records; ANALYZE developer_events; ANALYZE developer_uploads;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ query, index string }{
		{`SELECT id FROM sip_call_records WHERE ended_at IS NOT NULL AND ended_at<now()-interval '7 days' ORDER BY ended_at,id LIMIT 256`, "sip_call_records_retention"},
		{`SELECT 1 FROM developer_events WHERE resource_id='resource-2' AND state IN ('pending','sending')`, "developer_events_resource_pending"},
		{`SELECT id FROM developer_uploads WHERE created_at<now()-interval '2 hours' ORDER BY created_at LIMIT 256`, "developer_uploads_created"},
	} {
		var plan string
		if err = tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+tc.query).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, tc.index) || strings.Contains(plan, `"Node Type": "Seq Scan"`) {
			t.Fatalf("unbounded maintenance plan for %s: %s", tc.index, plan)
		}
	}
}
