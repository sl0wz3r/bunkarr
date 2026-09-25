-- Phase 0: settings (key/value, secrets sealed by internal/config), the single UI user, and
-- login sessions. Times are RFC3339 UTC text.

CREATE TABLE settings (
    key        TEXT    PRIMARY KEY NOT NULL,
    value      TEXT    NOT NULL,
    encrypted  INTEGER NOT NULL DEFAULT 0 CHECK (encrypted IN (0, 1)),
    updated_at TEXT    NOT NULL
) STRICT;

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
) STRICT;

-- token_hash is the SHA-256 (hex) of the cookie value; the raw token is never stored.
CREATE TABLE sessions (
    token_hash   TEXT    PRIMARY KEY NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL,
    remote_addr  TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);
