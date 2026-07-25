-- Minimal web sessions (SPEC-0001 / ADR-0004). One row per authenticated
-- browser session. The session token itself is never stored; token_hash is its
-- SHA-256, so a database disclosure yields no usable cookie (the same treatment
-- a password hash receives). csrf_token is the double-submit secret bound to the
-- session and echoed in the readable cairn_csrf cookie.
--
-- This is the seam OAuth 2.1 (ADR-0004, #22) reuses: its authorization server
-- mints sessions into this same table; only the credential adapter differs.
CREATE TABLE sessions (
    token_hash CHAR(64)    PRIMARY KEY,        -- hex SHA-256 of the opaque token
    actor_id   TEXT        NOT NULL,           -- the human this session acts as
    csrf_token TEXT        NOT NULL,           -- double-submit secret for this session
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

-- Expired sessions are filtered on read and swept by expiry; index the sweep key.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
