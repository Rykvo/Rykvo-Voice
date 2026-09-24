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
