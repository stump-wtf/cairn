-- Trajectory span & run model (SPEC-0004 / ADR-0009).
--
-- A trajectory is an ordinary artifact (share_type = 'trajectory', no single
-- body): it gets its public id, provenance, link access, TTL, and annotation
-- stream from the artifacts table for free (SPEC-0002 / ADR-0002). The run row
-- hangs off that artifact and owns the run-level facts; spans hang off the run
-- as an ordered nested tree keyed by (run_id, span_id). Oversized span outputs
-- spill to the SAME content-addressed `blobs` registry that artifact bodies use
-- (ADR-0008) — the span row then holds only an output_ref (sha256 + size +
-- truncation flag), never the bytes.

-- One run per trajectory artifact. Wall time, span/tool counts, and
-- time-by-category are NOT stored here — they are derived from the span rows so
-- they can never disagree with the waterfall (SPEC-0004 "Derived Run
-- Statistics"). token_count is the one agent-reported figure the ADR treats as
-- authoritative. ended_at is NULL while the run is open and stamped on close.
CREATE TABLE runs (
    id          BIGSERIAL   PRIMARY KEY,
    artifact_id BIGINT      NOT NULL UNIQUE REFERENCES artifacts(id) ON DELETE CASCADE,
    prompt      TEXT        NOT NULL DEFAULT '',
    model       TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'open',   -- open | closed
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ,                            -- NULL while open
    token_count BIGINT      NOT NULL DEFAULT 0,
    CONSTRAINT runs_status_chk CHECK (status IN ('open', 'closed'))
);

-- The ordered span tree. span_id is agent-assigned and stable for the life of
-- the run (it is the anchor target for ADR-0006 annotations, so it MUST NOT be
-- reissued). parent_span_id is NULL for a top-level span; depth and seq are
-- derived by the service (depth from the parent chain, seq a gap-free monotonic
-- sibling order) and persisted so the waterfall lays out deterministically
-- without re-deriving the tree on each draw. category is a closed enum so the
-- color legend stays total. There is deliberately NO per-span error/status
-- field in v1 (SPEC-0004 "v1 Non-Goals Are Absent From the Schema").
--
-- SUPERSEDED (issue #5): the closed category enum below is replaced by 0013 with
-- an open set — non-empty and length-bounded, but otherwise unconstrained. The
-- DDL here is left as-is because it is the historical schema; read 0013 for the
-- current constraint.
--
-- Output split (SPEC-0004 "Span Output Storage and Content Addressing"):
--   * small output  -> output_inline holds the text, output_ref_sha256 NULL
--   * large output  -> output_ref_sha256 references a content-addressed blob,
--                      output_inline NULL, bytes fetched lazily on expand
-- output_size is the byte length either way; output_truncated flags a capped
-- output.
CREATE TABLE spans (
    run_id            BIGINT   NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    span_id           TEXT     NOT NULL,
    parent_span_id    TEXT,                              -- NULL => top-level
    depth             INT      NOT NULL,                 -- 0 at top level
    seq               INT      NOT NULL,                 -- sibling order under parent
    category          TEXT     NOT NULL,                 -- reason|exec|read|net|write
    name              TEXT     NOT NULL DEFAULT '',
    tool              TEXT,                              -- non-null only for exec/read/net/write
    args              JSONB    NOT NULL DEFAULT '{}',
    output_inline     TEXT,                              -- inline body (<= threshold), else NULL
    output_ref_sha256 CHAR(64) REFERENCES blobs(sha256),-- large-output blob, else NULL
    output_size       BIGINT   NOT NULL DEFAULT 0,
    output_truncated  BOOLEAN  NOT NULL DEFAULT false,
    start_offset_ms   INT      NOT NULL,                 -- relative to run.started_at
    duration_ms       INT      NOT NULL,
    PRIMARY KEY (run_id, span_id),
    CONSTRAINT spans_category_chk CHECK (category IN ('reason', 'exec', 'read', 'net', 'write'))
);

-- Ordered-tree reads: children of a parent in seq order, and the whole-run scan
-- that builds the waterfall.
CREATE INDEX spans_tree_idx ON spans (run_id, parent_span_id, seq);

-- Reference-count large span-output blobs so the ADR-0008 GC reaper can reclaim
-- a blob once no span (or artifact body) points at it.
CREATE INDEX spans_output_ref_idx ON spans (output_ref_sha256);

-- Gap-free, collision-free sibling sequence: no two spans under the same parent
-- (top-level parents collapse to '' so NULL parents are compared too) share a
-- seq. Backstops the service's serialized seq assignment under concurrent
-- appends (SPEC-0004 "Concurrency Safety").
CREATE UNIQUE INDEX spans_sibling_seq_idx ON spans (run_id, COALESCE(parent_span_id, ''), seq);

-- The run -> produced artifact provenance edge. A `write` span may declare it
-- produced an ordinary Cairn artifact; the edge is resolvable from both
-- directions — from the run (join spans) and from the artifact
-- (produced_edges_artifact_idx). The composite FK to spans keeps an edge from
-- outliving its span. (SPEC-0004 "Produced-Artifact Link".)
CREATE TABLE produced_edges (
    run_id      BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    span_id     TEXT   NOT NULL,
    artifact_id BIGINT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    PRIMARY KEY (run_id, span_id, artifact_id),
    FOREIGN KEY (run_id, span_id) REFERENCES spans (run_id, span_id) ON DELETE CASCADE
);

CREATE INDEX produced_edges_artifact_idx ON produced_edges (artifact_id);
