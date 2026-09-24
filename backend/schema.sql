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
