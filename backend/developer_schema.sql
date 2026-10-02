CREATE TABLE IF NOT EXISTS developer_settings (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 username text NOT NULL DEFAULT '', key_hash bytea NOT NULL DEFAULT '',
 webhook text NOT NULL DEFAULT '', webhook_secret text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1,
 host_id uuid NOT NULL DEFAULT gen_random_uuid()
);
INSERT INTO developer_settings DEFAULT VALUES ON CONFLICT DO NOTHING;
ALTER TABLE developer_settings ADD COLUMN IF NOT EXISTS host_name text NOT NULL DEFAULT '';

ALTER TABLE messages ADD COLUMN IF NOT EXISTS expires_at timestamptz;
CREATE INDEX IF NOT EXISTS messages_outgoing_recent ON messages(module_id,created_at) WHERE mine;

CREATE TABLE IF NOT EXISTS developer_uploads (
 id text PRIMARY KEY, content text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(octet_length(content)<=1500000)
);

CREATE SEQUENCE IF NOT EXISTS developer_event_seq;
CREATE TABLE IF NOT EXISTS developer_events (
 id bigint PRIMARY KEY,
 event_type text NOT NULL, resource_id text NOT NULL, data jsonb NOT NULL,
 destination text NOT NULL, secret text NOT NULL,
 state text NOT NULL CHECK(state IN ('disabled','pending','sending','delivered','failed','cancelled')),
 attempts integer NOT NULL DEFAULT 0,
 issue text NOT NULL DEFAULT '', response_status integer NOT NULL DEFAULT 0,
 next_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz,
 lease text NOT NULL DEFAULT ''
);
ALTER TABLE developer_events ADD COLUMN IF NOT EXISTS claimed_at timestamptz;
CREATE INDEX IF NOT EXISTS developer_events_pending ON developer_events(next_at,id) WHERE state='pending';
CREATE INDEX IF NOT EXISTS developer_events_created ON developer_events(created_at,id);
CREATE INDEX IF NOT EXISTS developer_events_resource_pending ON developer_events(resource_id) WHERE state IN ('pending','sending');
CREATE INDEX IF NOT EXISTS developer_events_lanes_pending ON developer_events((event_type='message.received'),next_at,id) WHERE state='pending';
CREATE INDEX IF NOT EXISTS developer_uploads_created ON developer_uploads(created_at,id);
CREATE TABLE IF NOT EXISTS developer_module_state (
 module_id bigint PRIMARY KEY REFERENCES modules(id) ON DELETE CASCADE,
 data jsonb NOT NULL
);

CREATE OR REPLACE FUNCTION developer_emit(kind text,resource text,payload jsonb) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE config developer_settings%ROWTYPE; eid bigint; at timestamptz;
BEGIN
 SELECT * INTO config FROM developer_settings;
 IF config.webhook='' AND octet_length(config.key_hash)=0 THEN RETURN 0; END IF;
 -- One commit-ordered cursor for webhook delivery and API catch-up.
 PERFORM pg_advisory_xact_lock(-734904);
 eid:=nextval('developer_event_seq'); at:=clock_timestamp();
 INSERT INTO developer_events(id,event_type,resource_id,data,destination,secret,state)
 VALUES(eid,kind,resource,jsonb_build_object('eventId',eid::text,'type',kind,'version',eid,
  'occurredAt',at,'hostId',config.host_id,'host',config.host_name,'data',payload),config.webhook,config.webhook_secret,
  CASE WHEN config.webhook<>'' AND config.webhook_secret<>'' THEN 'pending' ELSE 'disabled' END);
 RETURN eid;
END $$;

CREATE OR REPLACE FUNCTION developer_message_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE kind text; status text; message jsonb;
BEGIN
 IF NEW.state='mms_report' THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND (NEW.state,NEW.issue,NEW.deleted_at,NEW.body,NEW.image) IS NOT DISTINCT FROM
  (OLD.state,OLD.issue,OLD.deleted_at,OLD.body,OLD.image) THEN RETURN NEW; END IF;
 kind:=CASE WHEN NOT NEW.mine AND NEW.state='received' AND NEW.deleted_at IS NULL THEN 'message.received' ELSE 'message.status_changed' END;
 status:=CASE WHEN NEW.state IN ('accepted','delivered') THEN 'delivered' WHEN NEW.state='received' THEN 'received'
  WHEN NEW.state IN ('queued','waiting_network','sending','receiving','download_pending','downloading') THEN 'pending' ELSE 'not_delivered' END;
 message:=jsonb_build_object('id',NEW.id,'moduleId','module-'||lpad(NEW.module_id::text,GREATEST(2,length(NEW.module_id::text)),'0'),
  'cardVersion',NEW.alert_epoch,'lineId',NEW.line_id,'number',NEW.peer,'mine',NEW.mine,'kind',NEW.kind,'state',NEW.state,
  'issue',NEW.issue,'displayStatus',status,
  'statusText',CASE WHEN status='delivered' THEN '已送达' WHEN status='received' THEN '已收到' WHEN NEW.state='waiting_network' THEN '等待网络' WHEN status='not_delivered' THEN '尚未送达' ELSE '' END,'carrierAccepted',NEW.state IN ('accepted','delivered'),'deliveryConfirmed',NEW.state='delivered',
  'text',CASE WHEN NEW.deleted_at IS NULL THEN NEW.body ELSE '' END,'deleted',NEW.deleted_at IS NOT NULL,'revision',NEW.revision,
  'attachmentPath',CASE WHEN NEW.image<>'' AND NEW.deleted_at IS NULL THEN '/api/v1/messages/'||NEW.id||'/image' ELSE '' END);
 PERFORM developer_emit(kind,NEW.id,message);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS developer_message_changed ON messages;
CREATE TRIGGER developer_message_changed AFTER INSERT OR UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION developer_message_changed();
