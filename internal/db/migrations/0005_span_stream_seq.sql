-- Live span streaming cursor (SPEC-0004 "Live Span Stream Delivery" / ADR-0012 SSE).
--
-- The span tree's persisted (depth, seq) is a rendering order, not an append
-- order: appending a child to an early parent inserts a row in the MIDDLE of
-- that order, so it cannot back a resumable stream cursor. The SSE endpoint
-- needs a per-run, append-monotonic sequence a reconnecting client can resume
-- after via Last-Event-ID with no loss or duplication. stream_seq is that
-- cursor: assigned strictly increasing in ingest order (gap-free per run,
-- serialized under the run-row lock that already serializes appends), it is the
-- id: of each SSE span event and the ">" key of the replay query.
--
-- This is additive infrastructure for a v1 requirement (live streaming), not one
-- of the "try next" columns SPEC-0004 keeps out of the schema (no error/status,
-- no token-cost lane, no diff state).

ALTER TABLE spans ADD COLUMN stream_seq BIGINT;

-- Backfill any rows that predate the column with a per-run order derived from
-- the persisted tree order, so the NOT NULL + uniqueness constraints below hold
-- regardless of existing data.
UPDATE spans SET stream_seq = sub.rn
FROM (
    SELECT run_id, span_id,
           row_number() OVER (PARTITION BY run_id ORDER BY depth, seq) AS rn
    FROM spans
) sub
WHERE spans.run_id = sub.run_id AND spans.span_id = sub.span_id;

ALTER TABLE spans ALTER COLUMN stream_seq SET NOT NULL;

-- Gap-free, collision-free per-run stream order: backstops the service's
-- serialized stream_seq assignment so two concurrent appends can never mint the
-- same cursor (SPEC-0004 "Concurrency Safety"). Also serves the replay query's
-- "stream_seq > $cursor ORDER BY stream_seq" range scan.
CREATE UNIQUE INDEX spans_stream_seq_idx ON spans (run_id, stream_seq);
