CREATE TABLE IF NOT EXISTS alert_settings (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 sip integer NOT NULL DEFAULT 5 CHECK(sip BETWEEN 0 AND 100),
 message integer NOT NULL DEFAULT 5 CHECK(message BETWEEN 0 AND 100),
 module integer NOT NULL DEFAULT 5 CHECK(module BETWEEN 0 AND 100),
 revision bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE alert_settings
 ALTER COLUMN message SET DEFAULT 5,
 DROP CONSTRAINT IF EXISTS alert_settings_sip_check,
 DROP CONSTRAINT IF EXISTS alert_settings_message_check,
 DROP CONSTRAINT IF EXISTS alert_settings_module_check,
 ADD CONSTRAINT alert_settings_sip_check CHECK(sip BETWEEN 0 AND 100),
 ADD CONSTRAINT alert_settings_message_check CHECK(message BETWEEN 0 AND 100),
 ADD CONSTRAINT alert_settings_module_check CHECK(module BETWEEN 0 AND 100);
INSERT INTO alert_settings DEFAULT VALUES ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS telegram_settings (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 token text NOT NULL DEFAULT '', proxy text NOT NULL DEFAULT '',
 admin_id text NOT NULL DEFAULT '', notification_id text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1
);
INSERT INTO telegram_settings DEFAULT VALUES ON CONFLICT DO NOTHING;

ALTER TABLE modules ADD COLUMN IF NOT EXISTS active_card text NOT NULL DEFAULT '';
ALTER TABLE modules ADD COLUMN IF NOT EXISTS card_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE modules ADD COLUMN IF NOT EXISTS card_since timestamptz NOT NULL DEFAULT now();

CREATE TABLE IF NOT EXISTS alert_counters (
 module_id bigint NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('sip','sms','mms','module')),
 epoch bigint NOT NULL, failures integer NOT NULL DEFAULT 0,
 active boolean NOT NULL DEFAULT false, reason text NOT NULL DEFAULT '',
 cycle bigint NOT NULL DEFAULT 0, observed_at timestamptz NOT NULL DEFAULT '-infinity',
 PRIMARY KEY(module_id,kind)
);
-- Keep ordered evidence for the current failure streak; message history is separate.
CREATE TABLE IF NOT EXISTS alert_outcomes (
 module_id bigint NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
 kind text NOT NULL, epoch bigint NOT NULL, attempt_id text NOT NULL,
 operation_at timestamptz NOT NULL, good boolean NOT NULL,
 weight integer NOT NULL DEFAULT 1,
 reason text NOT NULL DEFAULT '',
 PRIMARY KEY(module_id,kind,epoch,attempt_id)
);
CREATE INDEX IF NOT EXISTS alert_outcomes_order ON alert_outcomes(module_id,kind,epoch,operation_at);
INSERT INTO alert_outcomes(module_id,kind,epoch,attempt_id,operation_at,good,weight,reason)
 SELECT c.module_id,c.kind,c.epoch,'legacy-streak',c.observed_at,false,c.failures,c.reason FROM alert_counters c
 WHERE c.failures>0 AND NOT EXISTS(SELECT 1 FROM alert_outcomes o WHERE o.module_id=c.module_id AND o.kind=c.kind AND o.epoch=c.epoch)
 ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS alert_events (
 id text PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS alert_events_time ON alert_events(created_at);
CREATE TABLE IF NOT EXISTS alert_notifications (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 module_id bigint NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
 kind text NOT NULL, epoch bigint NOT NULL, cycle bigint NOT NULL,
 revision bigint NOT NULL, failures integer NOT NULL, reason text NOT NULL,
 state text NOT NULL DEFAULT 'pending', issue text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0, message_id bigint,
 next_at timestamptz NOT NULL DEFAULT now(), created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(module_id,kind,epoch,cycle)
);
CREATE INDEX IF NOT EXISTS alert_notifications_pending ON alert_notifications(next_at,id) WHERE state='pending';

CREATE OR REPLACE FUNCTION alert_limit_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE alert_counters SET failures=0,active=false,reason=''
  WHERE (kind='sip' AND NEW.sip=0) OR (kind IN ('sms','mms') AND NEW.message=0) OR (kind='module' AND NEW.module=0);
 DELETE FROM alert_outcomes WHERE (kind='sip' AND NEW.sip=0) OR (kind IN ('sms','mms') AND NEW.message=0) OR (kind='module' AND NEW.module=0);
 UPDATE alert_notifications SET state='cancelled' WHERE state='pending' AND
  ((kind='sip' AND NEW.sip=0) OR (kind IN ('sms','mms') AND NEW.message=0) OR (kind='module' AND NEW.module=0));
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS alert_limit_changed ON alert_settings;
CREATE TRIGGER alert_limit_changed AFTER UPDATE OF sip,message,module ON alert_settings FOR EACH ROW EXECUTE FUNCTION alert_limit_changed();

CREATE OR REPLACE FUNCTION alert_card_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.active_card <> '' AND NEW.active_card IS DISTINCT FROM OLD.active_card THEN
  NEW.card_epoch := OLD.card_epoch+1;
  NEW.card_since := clock_timestamp();
  DELETE FROM alert_outcomes WHERE module_id=NEW.id AND kind<>'module';
  UPDATE alert_counters SET epoch=NEW.card_epoch, failures=0, active=false, reason='', observed_at=NEW.card_since
   WHERE module_id=NEW.id AND kind<>'module';
  UPDATE alert_notifications SET state='cancelled' WHERE module_id=NEW.id AND kind<>'module' AND state='pending';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS alert_card_changed ON modules;
CREATE TRIGGER alert_card_changed BEFORE UPDATE OF active_card ON modules FOR EACH ROW EXECUTE FUNCTION alert_card_changed();

CREATE OR REPLACE FUNCTION alert_observe(mid bigint, ep bigint, k text, event_id text, good boolean, why text, observed timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE current_epoch bigint; current_card text; limit_count integer; c alert_counters%ROWTYPE; inserted integer;
 last_good timestamptz; attempt text; failure_count bigint; newest_reason text; target_kind text; kinds text[];
BEGIN
 SELECT card_epoch,active_card INTO current_epoch,current_card FROM modules WHERE id=mid FOR UPDATE;
 IF NOT FOUND OR k NOT IN ('sip','sms','mms','module') THEN RETURN; END IF;
 IF k<>'module' AND (ep<>current_epoch OR ep=0 OR current_card='') THEN RETURN; END IF;
 IF k='module' THEN ep:=0; END IF;
 INSERT INTO alert_events(id) VALUES(event_id) ON CONFLICT DO NOTHING;
 GET DIAGNOSTICS inserted=ROW_COUNT;
 IF inserted=0 THEN RETURN; END IF;
 attempt:=regexp_replace(event_id,':success$','');
 -- One confirmed business success recovers this activation's SIM counters, not hardware faults.
 kinds:=CASE WHEN good AND k<>'module' THEN ARRAY['sip','sms','mms'] ELSE ARRAY[k] END;
 FOREACH target_kind IN ARRAY kinds LOOP
  SELECT CASE target_kind WHEN 'sip' THEN sip WHEN 'module' THEN module ELSE message END INTO limit_count FROM alert_settings FOR SHARE;
  IF limit_count=0 THEN CONTINUE; END IF;
  INSERT INTO alert_counters(module_id,kind,epoch) VALUES(mid,target_kind,ep) ON CONFLICT DO NOTHING;
  SELECT * INTO c FROM alert_counters WHERE module_id=mid AND kind=target_kind FOR UPDATE;
  IF target_kind='module' AND observed<=c.observed_at THEN CONTINUE; END IF;
  IF c.epoch<>ep THEN c.failures:=0; c.active:=false; END IF;
  INSERT INTO alert_outcomes(module_id,kind,epoch,attempt_id,operation_at,good,reason)
   VALUES(mid,target_kind,ep,attempt,observed,good,left(why,128))
   ON CONFLICT(module_id,kind,epoch,attempt_id) DO UPDATE SET good=alert_outcomes.good OR EXCLUDED.good;
  SELECT max(operation_at) INTO last_good FROM alert_outcomes WHERE module_id=mid AND kind=target_kind AND epoch=ep AND alert_outcomes.good;
  SELECT COALESCE(sum(weight),0) INTO failure_count FROM alert_outcomes
   WHERE module_id=mid AND kind=target_kind AND epoch=ep AND NOT alert_outcomes.good AND operation_at>COALESCE(last_good,'-infinity');
  SELECT reason INTO newest_reason FROM alert_outcomes WHERE module_id=mid AND kind=target_kind AND epoch=ep AND NOT alert_outcomes.good
   AND operation_at>COALESCE(last_good,'-infinity') ORDER BY operation_at DESC,attempt_id DESC LIMIT 1;
  c.failures:=LEAST(failure_count,1000000); newest_reason:=COALESCE(newest_reason,'');
  IF c.failures<limit_count THEN
   c.active:=false;
   UPDATE alert_notifications SET state='cancelled' WHERE module_id=mid AND kind=target_kind AND state='pending';
  ELSIF NOT c.active THEN
   c.active:=true; c.cycle:=c.cycle+1;
   INSERT INTO alert_notifications(module_id,kind,epoch,cycle,revision,failures,reason)
    SELECT mid,target_kind,ep,c.cycle,revision,c.failures,left(newest_reason,128) FROM telegram_settings WHERE token<>'' AND notification_id<>''
    ON CONFLICT DO NOTHING;
  END IF;
  UPDATE alert_counters SET epoch=ep,failures=c.failures,active=c.active,reason=left(newest_reason,128),cycle=c.cycle,observed_at=GREATEST(observed,c.observed_at)
   WHERE module_id=mid AND kind=target_kind;
  -- Keep the successful operation boundary so delayed results cannot erase newer failures.
  DELETE FROM alert_outcomes WHERE module_id=mid AND kind=target_kind AND (epoch<>ep OR operation_at<last_good);
 END LOOP;
END $$;

ALTER TABLE messages ADD COLUMN IF NOT EXISTS operation_at timestamptz;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS alert_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS alert_recorded boolean NOT NULL DEFAULT false;
CREATE OR REPLACE FUNCTION alert_message_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT card_epoch INTO NEW.alert_epoch FROM modules WHERE id=NEW.module_id AND active_card=NEW.iccid;
  NEW.alert_epoch:=COALESCE(NEW.alert_epoch,0);
  RETURN NEW;
 END IF;
 IF NOT NEW.mine OR NEW.alert_epoch=0 OR NEW.created_at<(SELECT created_at FROM alert_settings) THEN RETURN NEW; END IF;
 IF NEW.alert_recorded THEN
  -- A late confirmation can resolve a counted uncertain submission once.
  IF NEW.kind='sms' AND OLD.state='unknown' AND OLD.issue='SMS_OUTCOME_UNKNOWN' AND NEW.state IN ('accepted','delivered') THEN
   PERFORM alert_observe(NEW.module_id,NEW.alert_epoch,NEW.kind,'message:'||NEW.id||':success',true,'',COALESCE(NEW.operation_at,NEW.created_at));
  END IF;
  RETURN NEW;
 END IF;
 IF NEW.state IN ('accepted','delivered') THEN
  PERFORM alert_observe(NEW.module_id,NEW.alert_epoch,NEW.kind,'message:'||NEW.id||':success',true,'',COALESCE(NEW.operation_at,NEW.created_at));
  NEW.alert_recorded:=true;
 ELSIF (NEW.state='failed' AND NEW.issue NOT IN ('INVALID_IMAGE','INVALID_MESSAGE','DEVICE_CHANGED','NO_SIM','MMS_CANCELLED')) OR
  (NEW.kind='sms' AND NEW.state='unknown' AND NEW.issue='SMS_OUTCOME_UNKNOWN'
   AND jsonb_typeof(NEW.result->'partsAttempted')='number' AND NEW.result->'partsAttempted'>'0'::jsonb) THEN
  PERFORM alert_observe(NEW.module_id,NEW.alert_epoch,NEW.kind,'message:'||NEW.id,false,NEW.issue,COALESCE(NEW.operation_at,NEW.created_at));
  NEW.alert_recorded:=true;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS alert_message_result ON messages;
CREATE TRIGGER alert_message_result BEFORE INSERT OR UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION alert_message_result();

ALTER TABLE sip_call_records ADD COLUMN IF NOT EXISTS alert_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE sip_call_records ADD COLUMN IF NOT EXISTS alert_recorded boolean NOT NULL DEFAULT false;
ALTER TABLE sip_call_records ADD COLUMN IF NOT EXISTS alert_ringing boolean NOT NULL DEFAULT false;
CREATE OR REPLACE FUNCTION alert_call_result() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE mid bigint; started timestamptz;
BEGIN
 IF NEW.direction<>'outgoing' OR NEW.alert_recorded OR NEW.module_id !~ '^module-[0-9]{1,9}$' THEN RETURN NEW; END IF;
 mid:=substring(NEW.module_id from 8)::bigint;
 IF NEW.alert_epoch=0 AND (TG_OP='INSERT' OR OLD.module_id='') THEN
  SELECT card_epoch,card_since INTO NEW.alert_epoch,started FROM modules WHERE id=mid AND active_card<>'';
  NEW.alert_epoch:=COALESCE(NEW.alert_epoch,0);
  IF started>NEW.started_at THEN NEW.alert_epoch:=0; END IF;
 END IF;
 IF NEW.alert_epoch=0 OR NEW.started_at<(SELECT created_at FROM alert_settings) THEN RETURN NEW; END IF;
 IF NEW.alert_ringing OR NEW.answered_at IS NOT NULL THEN
  PERFORM alert_observe(mid,NEW.alert_epoch,'sip','call:'||NEW.id,true,'',NEW.started_at);
  NEW.alert_recorded:=true;
 ELSIF NEW.ended_at IS NOT NULL THEN
  IF NEW.outcome IN ('dial_failed','card_error','not_registered','radio_unavailable','module_error','media_network_error','service_unavailable','carrier_unavailable','carrier_rejected') THEN
   PERFORM alert_observe(mid,NEW.alert_epoch,'sip','call:'||NEW.id,false,NEW.outcome,NEW.started_at);
  END IF;
  NEW.alert_recorded:=true;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS alert_call_result ON sip_call_records;
CREATE TRIGGER alert_call_result BEFORE INSERT OR UPDATE ON sip_call_records FOR EACH ROW EXECUTE FUNCTION alert_call_result();
