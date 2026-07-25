-- Annotation anchor substrate (SPEC-0006 / ADR-0006).
--
-- Two tables — reactions and comments — share one embedded polymorphic anchor:
--   artifact_id  → the artifact being annotated (every query scopes to one)
--   anchor_type  → registry-owned discriminator (internal/sharetype anchors)
--   anchor_ref   → JSONB, a type-specific locator (schema owned by the registry)
--   anchor_key   → canonical (sorted-key, whitespace-free) text serialization of
--                  anchor_ref, computed by the service, so uniqueness and
--                  grouping never depend on JSON field ordering
--
-- actor_id is the same opaque TEXT identity artifacts carry (there is no
-- actors table yet); ADR-0006's actors(id) FK lands when identities do.

-- Reactions are idempotent by construction: the UNIQUE constraint makes a
-- repeated identical react an upsert no-op and un-react a delete of that row
-- (SPEC-0006 REQ "Idempotent Reactions").
CREATE TABLE reactions (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    artifact_id BIGINT      NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    anchor_type TEXT        NOT NULL,
    anchor_ref  JSONB       NOT NULL DEFAULT '{}',
    anchor_key  TEXT        NOT NULL,
    emoji       TEXT        NOT NULL,             -- single unicode grapheme, e.g. '🔥'
    actor_id    TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (artifact_id, anchor_type, anchor_key, emoji, actor_id)
);

-- Per-anchor tallies at view time GROUP BY (anchor_key, emoji) scoped to one
-- artifact; this composite index backs that and every anchor-scoped read.
CREATE INDEX reactions_anchor_idx ON reactions (artifact_id, anchor_type, anchor_key);

-- Comments thread one level deep: a root has parent_id NULL, a reply references
-- its root. Soft delete (deleted_at) preserves thread structure; edits stamp
-- edited_at (SPEC-0006 REQ "Threaded Comments").
CREATE TABLE comments (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    artifact_id  BIGINT      NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    anchor_type  TEXT        NOT NULL,
    anchor_ref   JSONB       NOT NULL DEFAULT '{}',
    anchor_key   TEXT        NOT NULL,
    parent_id    BIGINT      REFERENCES comments(id) ON DELETE CASCADE, -- thread root = NULL
    actor_id     TEXT        NOT NULL,
    on_behalf_of TEXT        NOT NULL DEFAULT '',  -- provenance parity with artifacts
    body         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at    TIMESTAMPTZ,
    deleted_at   TIMESTAMPTZ                       -- soft delete keeps the thread
);

CREATE INDEX comments_anchor_idx ON comments (artifact_id, anchor_type, anchor_key);
CREATE INDEX comments_parent_idx ON comments (parent_id);
