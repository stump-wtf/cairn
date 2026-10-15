-- Artifact relations and trace context (ADR-0030, SPEC-0024 REQ "Database
-- Operation Standards").
--
-- An artifact can name other artifacts it replies to, derives from or follows.
-- The edge is immutable after create: there is no update path, and a delete of
-- either end takes the edge with it — CASCADE when the artifact holding the
-- declaration goes, SET NULL when the target goes, so the declaring artifact
-- keeps the public id it named and can still render "no longer available" in
-- the right position instead of losing the relation's shape.
--
-- target_public_id is NOT NULL and part of the primary key: the declaration is
-- kept verbatim even when its target's row is gone. The internal target id is
-- resolved at create and is only ever a join key — a reader who cannot open the
-- target is shown neither the id nor the title, so nothing here is a
-- readability signal on its own (SPEC-0024 REQ "Relations on Read").
--
-- position records the declaration order, which the read surfaces preserve.
-- The bound is artifact.MaxRelations, so a row past 7 means the service and the
-- schema have drifted (same pinning style as 0013's category length).
--
-- artifacts.trace_id / parent_span_id carry the W3C trace context of the run
-- that produced the artifact (SPEC-0024 REQ "Trace Context on Create"). Both
-- are nullable: nothing before this migration recorded one, and a create with a
-- missing or malformed traceparent stores NULL rather than failing. The fixed
-- octet_length checks keep a truncated id from being stored as if it were a
-- trace.

CREATE TABLE artifact_relations (
    artifact_id        BIGINT      NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    kind               TEXT        NOT NULL CHECK (kind IN ('reply_to','derived_from','follows')),
    target_artifact_id BIGINT      REFERENCES artifacts(id) ON DELETE SET NULL,
    target_public_id   TEXT        NOT NULL,
    position           SMALLINT    NOT NULL CHECK (position BETWEEN 0 AND 7),
    PRIMARY KEY (artifact_id, kind, target_public_id)
);

CREATE INDEX artifact_relations_target_idx ON artifact_relations (target_artifact_id)
    WHERE target_artifact_id IS NOT NULL;

ALTER TABLE artifacts ADD COLUMN trace_id       BYTEA CHECK (octet_length(trace_id) = 16);
ALTER TABLE artifacts ADD COLUMN parent_span_id BYTEA CHECK (octet_length(parent_span_id) = 8);

CREATE INDEX artifacts_trace_id_idx ON artifacts (trace_id) WHERE trace_id IS NOT NULL;

COMMENT ON TABLE artifact_relations IS
    'Typed immutable relations from one artifact to another (ADR-0030, SPEC-0024).';
COMMENT ON COLUMN artifact_relations.target_artifact_id IS
    'Resolved at create; SET NULL when the target is deleted, so the public id in target_public_id stays rendered as unavailable.';
COMMENT ON COLUMN artifacts.trace_id IS
    'W3C trace id (16 raw bytes) from the traceparent the create carried; NULL when absent or malformed.';
