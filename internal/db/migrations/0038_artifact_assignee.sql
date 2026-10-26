-- Native artifact assignee (ADR-0032, SPEC-0025 AS-1, issue #441 / #472).
--
-- One nullable assignee per artifact: a foreign key to users, never an owner
-- column. NULL is "unassigned" and the only sentinel — a cleared assignment
-- writes NULL, it does not tombstone (SPEC-0025 AS-1 "Assignee Model and
-- Invariants"). No backfill: nothing before this migration assigned an
-- artifact, and an artifact's assignee is set at create or by an explicit
-- change, never inferred from its tags.
--
-- The column is uuid, not the BIGINT the design record's data-model sketch
-- shows: users.id is uuid (0017_users.sql, ADR-0029) and every other user
-- reference since 0023 is uuid. A BIGINT would not bind to the key.
--
-- The reference is a plain REFERENCES users(id) with no delete action, the
-- same shape comments.user_id and reactions.user_id carry since 0023: the
-- assignee names a worker, not an owner, so it never cascades to the
-- artifact, and a stale assignment is left for read authorization to gate
-- (SPEC-0025 AS-1: a stale assignment MUST NOT be retroactively cleared).
--
-- The partial index backs the Bin's assignee filters (SPEC-0025 AS-9) and
-- the "assigned to me" view; unassigned rows — the overwhelming majority —
-- stay out of it.

ALTER TABLE artifacts
    ADD COLUMN assignee_user_id uuid NULL REFERENCES users(id);

CREATE INDEX idx_artifacts_assignee
    ON artifacts (assignee_user_id)
    WHERE assignee_user_id IS NOT NULL;

COMMENT ON COLUMN artifacts.assignee_user_id IS
    'The artifact''s single assignee (ADR-0032, SPEC-0025 AS-1); NULL when unassigned. A named worker, never an owner.';
