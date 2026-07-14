-- MCP agent sessions (issue #76, SPEC-0007, ADR-0004): one row per MCP client
-- connection, recorded when the OAuth-authenticated client completes the
-- `initialize` handshake. A session is tied to the OAuth grant it authorized
-- with (grant_id, ON DELETE CASCADE — a client re-registration or a purged
-- grant takes its sessions with it); revoking the grant (RevokeGrant,
-- oauth/service.go) is what "ending" a session means, so this table carries
-- no revoked/ended flag of its own — GET /v1/mcp/sessions joins
-- oauth_grants.revoked_at to report whether a session's grant is still live.
--
-- id is the MCP transport's own session id (the streamable-HTTP
-- Mcp-Session-Id, crypto-random and globally unique per the SDK's contract)
-- rather than a server-minted one: it is already the exact natural key for
-- "one row per connection" and lets the initialize handler and the tool-call
-- activity middleware agree on identity with no side table.
CREATE TABLE mcp_sessions (
    id                 TEXT        PRIMARY KEY,      -- MCP transport session id
    owner_id           TEXT        NOT NULL,         -- human subject (grant's actor_id)
    grant_id           TEXT        NOT NULL REFERENCES oauth_grants(grant_id) ON DELETE CASCADE,
    client_id          TEXT        NOT NULL,         -- oauth_clients.client_id the grant authenticated with
    client_name        TEXT        NOT NULL DEFAULT '', -- MCP Implementation.Name from `initialize` ("claude-code")
    client_version     TEXT        NOT NULL DEFAULT '', -- MCP Implementation.Version from `initialize` ("1.2.3")
    connected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_activity_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    tool_calls         BIGINT      NOT NULL DEFAULT 0, -- every tools/call, success or failure
    artifacts_created  BIGINT      NOT NULL DEFAULT 0, -- successful artifact_create/bundle_create/run_create
    annotations_posted BIGINT      NOT NULL DEFAULT 0  -- successful artifact_comment/artifact_react
);

CREATE INDEX mcp_sessions_owner_id_idx ON mcp_sessions (owner_id);
CREATE INDEX mcp_sessions_grant_id_idx ON mcp_sessions (grant_id);
