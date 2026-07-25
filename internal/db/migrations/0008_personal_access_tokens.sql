-- Personal access tokens (PATs): human-issued bearer credentials for the /v1
-- surface, minted from the web Settings UI (issue #74; ADR-0004 token seam,
-- SPEC-0007). Unlike the static CAIRN_API_TOKENS env credential (auth.go
-- APIToken), a PAT is owned by a signed-in human, scoped at creation to a
-- caller-chosen subset of the three ADR-0004 consent scopes
-- (artifacts:read, artifacts:write, annotations:write — never sharing:manage,
-- which no PAT can ever hold), and independently revocable from settings.
--
-- The plaintext secret is shown to its owner exactly once at creation; only
-- its SHA-256 digest is stored, so a database disclosure yields nothing
-- replayable (SPEC-0007 REQ "Database Operation Standards": token lookups
-- hashed-at-rest).

CREATE TABLE personal_access_tokens (
    id           TEXT        PRIMARY KEY,      -- opaque token id ("pat-...")
    owner_id     TEXT        NOT NULL,         -- the human who minted this token (subject)
    name         TEXT        NOT NULL,         -- caller-supplied label ("laptop agent")
    token_hash   CHAR(64)    NOT NULL UNIQUE,  -- hex SHA-256 of the opaque secret
    scope        TEXT        NOT NULL,         -- space-joined subset of the three ADR-0004 scopes
    is_agent     BOOLEAN     NOT NULL DEFAULT false, -- agent-minted tokens never get delete/sharing:manage (requireHuman)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,                  -- touched on every successful authentication
    revoked_at   TIMESTAMPTZ                   -- non-null = revoked; the row is kept for the owner's history
);

CREATE INDEX personal_access_tokens_owner_id_idx ON personal_access_tokens (owner_id);
