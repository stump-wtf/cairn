-- OAuth 2.1 authorization-server state (SPEC-0007, ADR-0004). Four tables:
-- dynamically-registered clients (RFC 7591), single-use authorization codes
-- bound to their client + redirect URI + PKCE challenge, per-connection grants
-- (the independently revocable "connection" a human sees in settings), and the
-- grant's token family (short audience-bound access + rotating refresh).
--
-- Every secret column stores only a SHA-256 hex digest — an authorization code,
-- access token, or refresh token is shown to the client exactly once and a
-- database disclosure yields nothing replayable (SPEC-0007 REQ "Database
-- Operation Standards": token/code lookups hashed-at-rest).

CREATE TABLE oauth_clients (
    client_id     TEXT        PRIMARY KEY,
    client_name   TEXT        NOT NULL DEFAULT '',
    redirect_uris TEXT[]      NOT NULL,       -- exact-match set (loopback port may vary, RFC 8252)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_auth_codes (
    code_hash      CHAR(64)    PRIMARY KEY,   -- hex SHA-256 of the single-use code
    client_id      TEXT        NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    actor_id       TEXT        NOT NULL,      -- the human who approved consent (subject)
    redirect_uri   TEXT        NOT NULL,      -- bound at issue; exchange must present it
    scope          TEXT        NOT NULL,      -- space-joined approved subset of the three
    code_challenge TEXT        NOT NULL,      -- PKCE S256 challenge (mandatory, no plain)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,      -- short-lived (minutes)
    redeemed_at    TIMESTAMPTZ,               -- non-null = spent; a replay revokes grant_id
    grant_id       TEXT                       -- the grant redemption minted (for replay revocation)
);

CREATE INDEX oauth_auth_codes_expires_at_idx ON oauth_auth_codes (expires_at);

CREATE TABLE oauth_grants (
    grant_id     TEXT        PRIMARY KEY,
    client_id    TEXT        NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    actor_id     TEXT        NOT NULL,        -- subject = the human; agents inherit their reach
    scope        TEXT        NOT NULL,        -- space-joined granted subset
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,                 -- surfaced in settings ("last-used")
    revoked_at   TIMESTAMPTZ                  -- RFC 7009 / settings revocation
);

CREATE TABLE oauth_tokens (
    token_hash CHAR(64)    PRIMARY KEY,       -- hex SHA-256 of the opaque token
    grant_id   TEXT        NOT NULL REFERENCES oauth_grants(grant_id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL CHECK (kind IN ('access', 'refresh')),
    audience   TEXT        NOT NULL,          -- RFC 8707 audience the token is bound to
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    rotated_at TIMESTAMPTZ,                   -- refresh only: rotated out; reuse revokes the family
    revoked_at TIMESTAMPTZ
);

CREATE INDEX oauth_tokens_grant_id_idx ON oauth_tokens (grant_id);
CREATE INDEX oauth_tokens_expires_at_idx ON oauth_tokens (expires_at);
