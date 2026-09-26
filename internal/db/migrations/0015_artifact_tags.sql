-- Artifact tags: short, flat, client-asserted strings ("handoff", "lane:auto",
-- "repo:stump.wtf/cairn") so a consumer of the artifact.created outbound event
-- (Switchboard) can route an artifact — tell a handoff work order from an
-- ordinary paste, pick a lane — without fetching and parsing its body.
--
-- A column beside provenance, not inside it. actor_id and channel are derived
-- server-side from the authenticated surface; a tag is whatever the caller sent. Keeping them apart in the schema keeps them apart in every
-- reader's head: nothing in this column is a trust signal (ADR-0018).
--
-- TEXT[] rather than JSONB or a side table: a tag list is written once at create
-- and read whole, and a native array gives the Bin its tag filter as a plain
-- containment predicate with no join. No GIN index yet: every Bin query is
-- already narrowed by owner_id, so the containment check runs over one owner's
-- rows; add the index when a cross-owner tag query exists. NOT NULL DEFAULT '{}'
-- backfills every existing row as untagged and is a metadata-only change on
-- Postgres 11+.
--
-- The per-tag rules (1-64 bytes of lowercase [a-z0-9._:/#-]) and deduplication
-- are enforced by artifact.NormalizeTags on every create path. The CHECK mirrors
-- only the count ceiling, so even a direct write cannot store an unbounded list;
-- TestArtifactTagsConstraintMatchesDomain asserts it equals artifact.MaxTags.
--
-- Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact
-- Tags"
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE artifacts ADD CONSTRAINT artifacts_tags_count_chk
    CHECK (cardinality(tags) <= 32);
