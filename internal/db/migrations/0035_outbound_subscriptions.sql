-- Owned outbound subscriptions (ADR-0029 section 6, SPEC-0023 REQ "Owned
-- Outbound Subscriptions", REQ "Events Go Only to the Artifact's Workspace").
--
-- A subscription is the only outbound delivery target: the instance-wide
-- target list and its shared secret are removed in the same change. Each row
-- is owned by exactly one user or one team, and an event about an artifact is
-- delivered only to the subscriptions of the workspace that owns it.
--
-- secret_enc is the delivery signing secret sealed with CAIRN_ENCRYPTION_KEY
-- (AES-256-GCM, the row id as associated data, so a ciphertext copied onto
-- another row does not open). The plaintext is shown once, at creation or
-- rotation, and never stored.
--
-- Health: consecutive_failures counts failed deliveries (after retries) and
-- resets on success; at 20 the delivery worker sets disabled_reason and the
-- subscription stops receiving until its owner resumes it. last_status is the
-- HTTP status of the last attempt, NULL when no response arrived; last_error
-- is a fixed-vocabulary reason and never contains the target URL.
--
-- owner_team_id has no foreign key yet: teams do not exist until #328, which
-- adds it (as #327 left artifacts.owner_team_id and #330
-- operator_audit.target_team).
--
-- Forward-safe against live data: one new, empty table and its indexes.

CREATE TABLE outbound_subscriptions (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id        uuid        REFERENCES users(id) ON DELETE CASCADE,
    owner_team_id        uuid,
    created_by_user_id   uuid        REFERENCES users(id) ON DELETE SET NULL,
    url                  text        NOT NULL,
    secret_enc           bytea       NOT NULL,
    event_types          text[]      NOT NULL DEFAULT '{}',
    share_types          text[]      NOT NULL DEFAULT '{}',
    tags                 text[]      NOT NULL DEFAULT '{}',
    paused               boolean     NOT NULL DEFAULT false,
    disabled_reason      text,
    consecutive_failures int         NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    last_attempt_at      timestamptz,
    last_status          int,
    last_error           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbound_subscriptions_one_owner
        CHECK (num_nonnulls(owner_user_id, owner_team_id) = 1)
);

-- The delivery worker's lookup: a workspace's subscriptions that can receive.
CREATE INDEX outbound_subscriptions_user_live_idx
    ON outbound_subscriptions (owner_user_id)
    WHERE owner_user_id IS NOT NULL AND NOT paused AND disabled_reason IS NULL;
CREATE INDEX outbound_subscriptions_team_live_idx
    ON outbound_subscriptions (owner_team_id)
    WHERE owner_team_id IS NOT NULL AND NOT paused AND disabled_reason IS NULL;

-- The owner's list and the per-owner ceiling count.
CREATE INDEX outbound_subscriptions_user_idx ON outbound_subscriptions (owner_user_id, created_at);
CREATE INDEX outbound_subscriptions_team_idx ON outbound_subscriptions (owner_team_id, created_at);
