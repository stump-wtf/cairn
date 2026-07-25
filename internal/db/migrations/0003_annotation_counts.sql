-- Artifact-level annotation count rollups (ADR-0006 "Count aggregation",
-- SPEC-0006 REQ "Count Aggregation").
--
-- reaction_count and comment_count shipped in 0001_init; pin_count — the third
-- denormalized rollup ADR-0006 specifies (image-region reactions + pinned
-- comments) — was missing. All three counters are maintained by the annotation
-- core service in the SAME transaction as the annotation insert / delete /
-- soft-delete, so a committed count can never diverge from its rows.
ALTER TABLE artifacts ADD COLUMN pin_count INT NOT NULL DEFAULT 0;
