-- The operator profile (ADR-0029, SPEC-0023 REQ "Operator and User Profiles",
-- REQ "Operator Surfaces Bound Tenant Data and Never Read It").
--
-- The operator is configuration, not a column: a user is an operator while
-- their session's (issuer, subject) is listed in CAIRN_OPERATORS, or while
-- CAIRN_OPERATOR_GROUP is set and the OIDC groups claim at sign-in carried it.
-- Only that second fact needs storing, because the claim exists only at
-- sign-in: sessions.operator_group records the configured group the claim
-- carried then ('' when none). A session is an operator's only while the
-- recorded group still equals the configured one, so renaming or unsetting
-- CAIRN_OPERATOR_GROUP demotes every such session on its next request.
--
-- operator_audit is the trail every operator action that affects a tenant
-- writes in the same transaction as the action: who, whom, what, the
-- required reason, and when. The affected user reads their own rows on the
-- Settings page. detail carries counts, never artifact ids or content.
-- target_team has no foreign key yet: teams do not exist until #328, which
-- adds it (as #327 left artifacts.owner_team_id).
--
-- users.suspended_at already exists (0017).
--
-- Forward-safe against live data: one new table, one column with a constant
-- default (no rewrite), and one index on the small sessions table.

CREATE TABLE operator_audit (
    id          bigserial   PRIMARY KEY,
    operator_id uuid        REFERENCES users(id) ON DELETE SET NULL,
    target_user uuid        REFERENCES users(id) ON DELETE SET NULL,
    target_team uuid,
    action      text        NOT NULL,
    reason      text        NOT NULL CHECK (length(reason) > 0),
    detail      jsonb       NOT NULL DEFAULT '{}',
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX operator_audit_target_user_idx ON operator_audit (target_user, at DESC, id DESC);
CREATE INDEX operator_audit_at_idx ON operator_audit (at DESC, id DESC);

ALTER TABLE sessions ADD COLUMN operator_group text NOT NULL DEFAULT '';

-- Suspension deletes a user's sessions in one statement.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
