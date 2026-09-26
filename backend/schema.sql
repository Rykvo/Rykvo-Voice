CREATE TABLE IF NOT EXISTS administrators (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username text NOT NULL UNIQUE CHECK (length(username) BETWEEN 1 AND 128),
    password_hash bytea NOT NULL CHECK (octet_length(password_hash) = 32),
    password_salt bytea NOT NULL CHECK (octet_length(password_salt) = 16),
    password_iterations integer NOT NULL CHECK (password_iterations >= 600000),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS login_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    administrator_id bigint NOT NULL REFERENCES administrators(id) ON DELETE CASCADE,
    csrf_token text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS login_sessions_expiry ON login_sessions(expires_at);
CREATE INDEX IF NOT EXISTS login_sessions_user ON login_sessions(administrator_id);

ALTER TABLE login_sessions ADD COLUMN IF NOT EXISTS visibility_until timestamptz;
ALTER TABLE login_sessions ADD COLUMN IF NOT EXISTS visibility_version bigint;

CREATE TABLE IF NOT EXISTS visibility_security (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    password_hash bytea NOT NULL CHECK (octet_length(password_hash) = 32),
    password_salt bytea NOT NULL CHECK (octet_length(password_salt) = 16),
    password_iterations integer NOT NULL CHECK (password_iterations >= 600000),
    version bigint NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS visibility_preferences (
    administrator_id bigint PRIMARY KEY REFERENCES administrators(id) ON DELETE CASCADE,
    features jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(features) = 'object')
);
CREATE TABLE IF NOT EXISTS visibility_attempts (
    administrator_id bigint PRIMARY KEY REFERENCES administrators(id) ON DELETE CASCADE,
    count integer NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS administrator_attempts (
 administrator_id bigint PRIMARY KEY REFERENCES administrators(id) ON DELETE CASCADE,
 count integer NOT NULL,
 started_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS tunnel_settings (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 binding jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(binding) = 'object'),
 updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO tunnel_settings DEFAULT VALUES ON CONFLICT(singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS modules (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 hardware_key text NOT NULL UNIQUE,
 endpoint text NOT NULL,
 kind text NOT NULL,
 model text NOT NULL DEFAULT '',
 label text NOT NULL CHECK (length(label) BETWEEN 1 AND 20),
 label_key text NOT NULL UNIQUE,
 label_custom boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 last_seen timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS modules_endpoint ON modules(endpoint);
ALTER TABLE modules ADD COLUMN IF NOT EXISTS serial_key text NOT NULL DEFAULT '';
ALTER TABLE modules ADD COLUMN IF NOT EXISTS endpoint_generation text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS modules_serial_key ON modules(serial_key) WHERE serial_key<>'';

CREATE TABLE IF NOT EXISTS module_jobs (
 id text PRIMARY KEY,
 module_id bigint NOT NULL REFERENCES modules(id),
 action text NOT NULL,
 state text NOT NULL,
 stage text NOT NULL DEFAULT '',
 issue text NOT NULL DEFAULT '',
 warning text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS module_jobs_latest ON module_jobs(module_id,created_at DESC);
CREATE TABLE IF NOT EXISTS card_phone_numbers (
    iccid text PRIMARY KEY CHECK (iccid ~ '^[0-9]{18,20}$'),
    number text NOT NULL CHECK (number ~ '^\+[0-9]{5,15}$'),
    source text NOT NULL DEFAULT 'ims' CHECK (source = 'ims'),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE module_jobs ADD COLUMN IF NOT EXISTS verification jsonb NOT NULL DEFAULT 'null';

ALTER TABLE module_jobs ADD COLUMN IF NOT EXISTS networks jsonb NOT NULL DEFAULT 'null';

CREATE TABLE IF NOT EXISTS module_recoveries (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 module_id bigint NOT NULL REFERENCES modules(id),
 attempted_at timestamptz NOT NULL DEFAULT now(),
 result text NOT NULL DEFAULT 'unconfirmed'
);
CREATE INDEX IF NOT EXISTS module_recoveries_time ON module_recoveries(attempted_at);
CREATE INDEX IF NOT EXISTS module_recoveries_module ON module_recoveries(module_id,attempted_at);

CREATE TABLE IF NOT EXISTS module_wifi (
 module_id bigint PRIMARY KEY REFERENCES modules(id) ON DELETE CASCADE,
 iccid text NOT NULL CHECK (iccid ~ '^[0-9]{18,20}$'),
 enabled boolean NOT NULL DEFAULT false,
 request_id text NOT NULL
);

CREATE TABLE IF NOT EXISTS card_apn_profiles (
 iccid text NOT NULL CHECK (iccid ~ '^[0-9]{18,20}$'),
 id text NOT NULL,
 configuration jsonb NOT NULL CHECK (jsonb_typeof(configuration) = 'object'),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (iccid,id)
);

CREATE TABLE IF NOT EXISTS card_carrier_configs (
 iccid text PRIMARY KEY CHECK (iccid ~ '^[0-9]{18,20}$'),
 configuration jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS card_data_policies (
 iccid text PRIMARY KEY CHECK (iccid ~ '^[0-9]{18,20}$'),
 roaming boolean NOT NULL DEFAULT false,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS card_data_policy_requests (
 id text PRIMARY KEY,
 module_id bigint NOT NULL REFERENCES modules(id),
 iccid text NOT NULL CHECK (iccid ~ '^[0-9]{18,20}$'),
 roaming boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS messages (
 id text PRIMARY KEY,
 module_id bigint NOT NULL REFERENCES modules(id),
 iccid text NOT NULL CHECK (iccid ~ '^[0-9]{18,20}$'),
 line_id text NOT NULL,
 peer text NOT NULL,
 mine boolean NOT NULL,
 kind text NOT NULL CHECK (kind IN ('sms','mms')),
 body text NOT NULL DEFAULT '',
 image text NOT NULL DEFAULT '',
 state text NOT NULL,
 issue text NOT NULL DEFAULT '',
 request_hash text NOT NULL DEFAULT '',
 result jsonb NOT NULL DEFAULT '{}',
 metadata jsonb NOT NULL DEFAULT '{}',
 read_at timestamptz,
 deleted_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS messages_queue ON messages(state,created_at);
CREATE INDEX IF NOT EXISTS messages_card_peer ON messages(iccid,peer,created_at);
CREATE TABLE IF NOT EXISTS message_parts (
 iccid text NOT NULL, fingerprint text NOT NULL, message_id text NOT NULL REFERENCES messages(id),
 sequence integer NOT NULL, body text NOT NULL, raw_tpdu text NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(iccid,fingerprint), UNIQUE(message_id,sequence)
);
CREATE TABLE IF NOT EXISTS message_reports (
 iccid text NOT NULL, fingerprint text NOT NULL, peer text NOT NULL,
 reference integer NOT NULL, status integer NOT NULL,
 scts timestamptz, received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(iccid,fingerprint)
);
CREATE SEQUENCE IF NOT EXISTS message_revision_seq;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT nextval('message_revision_seq');
CREATE OR REPLACE FUNCTION message_revision_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM pg_advisory_xact_lock(-734901); NEW.revision=nextval('message_revision_seq'); NEW.updated_at=now(); RETURN NEW; END $$;
DROP TRIGGER IF EXISTS message_revision_trigger ON messages;
CREATE TRIGGER message_revision_trigger BEFORE INSERT OR UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION message_revision_update();
CREATE INDEX IF NOT EXISTS messages_revision ON messages(revision);

CREATE SEQUENCE IF NOT EXISTS message_contact_revision_seq;
CREATE TABLE IF NOT EXISTS message_contacts (
 line_id text NOT NULL,
 peer text NOT NULL,
 name text NOT NULL DEFAULT '' CHECK (char_length(name)<=24),
 revision bigint NOT NULL DEFAULT nextval('message_contact_revision_seq'),
 PRIMARY KEY(line_id,peer)
);

CREATE TABLE IF NOT EXISTS restart_requests (
 id text PRIMARY KEY,
 scope text NOT NULL,
 state text NOT NULL DEFAULT 'requested',
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sip_accounts (
 id text PRIMARY KEY,
 username text NOT NULL,
 port integer NOT NULL CHECK (port BETWEEN 1024 AND 65535),
 realm text NOT NULL DEFAULT 'rykvo',
 digest_md5 bytea NOT NULL CHECK (octet_length(digest_md5)=16),
 digest_sha256 bytea NOT NULL CHECK (octet_length(digest_sha256)=32),
 allocation text NOT NULL CHECK (allocation IN ('all','fixed')),
 receive_calls boolean NOT NULL DEFAULT false,
 credential_revision bigint NOT NULL DEFAULT 1 CHECK (credential_revision>0),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(username,port)
);
CREATE TABLE IF NOT EXISTS sip_account_modules (
 account_id text NOT NULL REFERENCES sip_accounts(id) ON DELETE CASCADE,
 module_id bigint NOT NULL REFERENCES modules(id),
 PRIMARY KEY(account_id,module_id)
);

CREATE TABLE IF NOT EXISTS sip_call_records (
 id text PRIMARY KEY,
 account_id text NOT NULL,
 module_id text NOT NULL,
 peer text NOT NULL,
 state text NOT NULL DEFAULT 'dialing',
 started_at timestamptz NOT NULL DEFAULT now(),
 answered_at timestamptz,
 ended_at timestamptz
);
CREATE INDEX IF NOT EXISTS sip_call_records_account ON sip_call_records(account_id,started_at DESC);
