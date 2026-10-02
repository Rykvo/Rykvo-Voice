CREATE INDEX IF NOT EXISTS messages_thread_history ON messages(module_id,line_id,peer,created_at DESC,id DESC)
 WHERE deleted_at IS NULL AND state<>'mms_report';

-- Backfill once. Subsequent starts only replace the trigger, not scan history.
DO $$ BEGIN
 IF to_regclass('message_threads') IS NULL THEN
  CREATE TABLE message_threads (
   module_id bigint NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
   line_id text NOT NULL, peer text NOT NULL,
   last_id text, revision bigint NOT NULL,
   PRIMARY KEY(module_id,line_id,peer)
  );
  INSERT INTO message_threads(module_id,line_id,peer,last_id,revision)
   SELECT g.module_id,g.line_id,g.peer,m.id,g.revision
   FROM (SELECT module_id,line_id,peer,max(revision) revision FROM messages WHERE state<>'mms_report' GROUP BY module_id,line_id,peer) g
   LEFT JOIN LATERAL (SELECT id FROM messages WHERE module_id=g.module_id AND line_id=g.line_id AND peer=g.peer
    AND deleted_at IS NULL AND state<>'mms_report' ORDER BY created_at DESC,id DESC LIMIT 1) m ON true;
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS message_threads_revision ON message_threads(revision);

CREATE OR REPLACE FUNCTION message_thread_update() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE latest text;
BEGIN
 IF TG_OP<>'INSERT' THEN
  IF OLD.state<>'mms_report' AND (TG_OP='DELETE' OR (OLD.module_id,OLD.line_id,OLD.peer) IS DISTINCT FROM (NEW.module_id,NEW.line_id,NEW.peer) OR NEW.state='mms_report') THEN
   IF TG_OP='DELETE' AND OLD.deleted_at IS NOT NULL THEN RETURN OLD; END IF;
   PERFORM pg_advisory_xact_lock(-734901);
   SELECT id INTO latest FROM messages WHERE module_id=OLD.module_id AND line_id=OLD.line_id AND peer=OLD.peer
    AND deleted_at IS NULL AND state<>'mms_report' ORDER BY created_at DESC,id DESC LIMIT 1;
   INSERT INTO message_threads(module_id,line_id,peer,last_id,revision)
    VALUES(OLD.module_id,OLD.line_id,OLD.peer,latest,nextval('message_revision_seq'))
    ON CONFLICT(module_id,line_id,peer) DO UPDATE SET last_id=excluded.last_id,revision=excluded.revision;
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 IF NEW.state='mms_report' THEN RETURN NEW; END IF;
 SELECT id INTO latest FROM messages WHERE module_id=NEW.module_id AND line_id=NEW.line_id AND peer=NEW.peer
  AND deleted_at IS NULL AND state<>'mms_report' ORDER BY created_at DESC,id DESC LIMIT 1;
 INSERT INTO message_threads(module_id,line_id,peer,last_id,revision)
  VALUES(NEW.module_id,NEW.line_id,NEW.peer,latest,NEW.revision)
  ON CONFLICT(module_id,line_id,peer) DO UPDATE SET last_id=excluded.last_id,revision=GREATEST(message_threads.revision,excluded.revision);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS message_thread_trigger ON messages;
CREATE TRIGGER message_thread_trigger AFTER INSERT OR UPDATE OR DELETE ON messages FOR EACH ROW EXECUTE FUNCTION message_thread_update();
