CREATE TABLE IF NOT EXISTS retention_settings (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 days integer NOT NULL DEFAULT 3 CHECK(days BETWEEN 0 AND 36500),
 revision bigint NOT NULL DEFAULT 1,
 last_run timestamptz, issue text NOT NULL DEFAULT ''
);
ALTER TABLE retention_settings ALTER COLUMN days SET DEFAULT 3;
ALTER TABLE retention_settings ADD COLUMN IF NOT EXISTS upload_hours integer NOT NULL DEFAULT 2 CHECK(upload_hours BETWEEN 1 AND 8760);
INSERT INTO retention_settings DEFAULT VALUES ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS message_requests (
 id text PRIMARY KEY, request_hash text NOT NULL, expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS message_requests_expiry ON message_requests(expires_at);
CREATE TABLE IF NOT EXISTS message_fingerprints (
 iccid text NOT NULL, fingerprint text NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(iccid,fingerprint)
);
CREATE INDEX IF NOT EXISTS message_fingerprints_expiry ON message_fingerprints(expires_at);
ALTER TABLE developer_settings ADD COLUMN IF NOT EXISTS event_floor bigint NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS messages_retention ON messages(created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS sip_call_records_retention ON sip_call_records(ended_at,id) WHERE ended_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS messages_deleted ON messages(deleted_at,id) WHERE deleted_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS message_sync_state (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 floor bigint NOT NULL DEFAULT 0
);
INSERT INTO message_sync_state DEFAULT VALUES ON CONFLICT DO NOTHING;

CREATE INDEX IF NOT EXISTS messages_receipt_revision ON messages(revision DESC) WHERE mine AND state IN ('accepted','partial','unknown');
CREATE INDEX IF NOT EXISTS messages_mms_report_revision ON messages(revision DESC) WHERE NOT mine AND state='mms_report';
CREATE INDEX IF NOT EXISTS message_reports_received ON message_reports(received_at);
CREATE INDEX IF NOT EXISTS messages_result_deadline ON messages((COALESCE(operation_at,created_at)),id)
 WHERE mine AND deleted_at IS NULL AND state IN ('sending','unknown');
CREATE INDEX IF NOT EXISTS messages_queue_deadline ON messages(expires_at,id)
 WHERE mine AND deleted_at IS NULL AND state IN ('queued','waiting_network');
CREATE INDEX IF NOT EXISTS messages_timeout_receipt_revision ON messages(revision DESC)
 WHERE mine AND state='failed' AND issue='RESULT_TIMEOUT';
