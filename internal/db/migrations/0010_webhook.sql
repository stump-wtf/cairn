-- Webhook endpoint model & capped capture buffer (SPEC-0005 / ADR-0010).
--
-- A webhook endpoint is an ordinary artifact (share_type = 'webhook', no
-- single body): it gets its public id, provenance, link access, and TTL from
-- the artifacts table for free (SPEC-0002 / ADR-0002), exactly like a
-- trajectory run. The hook row hangs off that artifact and owns the
-- ring-buffer cap and the monotonic seq counter; captured requests hang off
-- the hook as a capped, seq-ordered log. This migration is the model +
-- management story only (issue #83) — it does NOT open the public ingress
-- endpoint that fills the buffer (that lands in the next story); it does add
-- the storage and a core-service Capture path integration tests use to
-- simulate captures.
--
-- Oversized captured bodies spill to the SAME content-addressed `blobs`
-- registry span outputs use (ADR-0008) — the request row then holds only a
-- body_ref (sha256 + size + truncation flag), never the bytes, exactly
-- mirroring 0004_trajectory.sql's span output split.

-- One hook per webhook artifact. next_seq is the per-endpoint monotonic
-- counter every capture increments (SPEC-0005 "a monotonic seq (stream
-- order)"); it is never reset or reused, even across ring-buffer eviction, so
-- a seq value uniquely and stably identifies a request for the life of the
-- endpoint (SPEC-0005 "a stable request id — the webhook_request anchor
-- target"). request_cap bounds the ring buffer (default N=500,
-- SPEC-0005 "Ring-Buffer Retention and Caps").
CREATE TABLE hooks (
    id          BIGSERIAL   PRIMARY KEY,
    artifact_id BIGINT      NOT NULL UNIQUE REFERENCES artifacts(id) ON DELETE CASCADE,
    request_cap INT         NOT NULL DEFAULT 500,
    next_seq    BIGINT      NOT NULL DEFAULT 0,
    CONSTRAINT hooks_request_cap_chk CHECK (request_cap > 0)
);

-- The capped, seq-ordered captured-request log. A row is one accepted inbound
-- request: method/path/query/headers/status/content_type/body_size are the
-- PostgreSQL-resident metadata the inspector queries for the status mix and
-- method counts WITHOUT ever reading a body (SPEC-0005 "Metadata queryable
-- without reading the body"); the body itself is inline (<= threshold) or
-- spilled to a content-addressed blob (SPEC-0005 "Body stored verbatim and
-- content-addressed"). headers are sanitized/normalized before storage
-- (SPEC-0005 "Header Hygiene") — this row is a sanitized projection, never a
-- replayable credential dump. status is the FIXED response Cairn returned,
-- recorded data the inbound payload can never steer (SPEC-0005 "Fixed Benign
-- Response").
--
-- On overflow (capture at the cap) the oldest row(s) by seq are deleted in
-- the SAME transaction as the insert (SPEC-0005 "Capture-and-evict is
-- atomic"); the blob a deleted row referenced is left for the ADR-0008
-- refcounted GC reaper rather than deleted inline here, matching how span
-- rows leave their output blobs for the same reaper.
CREATE TABLE hook_requests (
    hook_id         BIGINT      NOT NULL REFERENCES hooks(id) ON DELETE CASCADE,
    seq             BIGINT      NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    method          TEXT        NOT NULL,
    path            TEXT        NOT NULL DEFAULT '',
    query           TEXT        NOT NULL DEFAULT '',
    headers         JSONB       NOT NULL DEFAULT '{}',  -- sanitized header multi-map
    status          INT         NOT NULL,                -- the fixed status Cairn returned
    content_type    TEXT        NOT NULL DEFAULT '',
    body_size       BIGINT      NOT NULL DEFAULT 0,
    body_inline     BYTEA,                                -- inline body (<= threshold), else NULL
    body_ref_sha256 CHAR(64) REFERENCES blobs(sha256),    -- large-body blob, else NULL
    body_truncated  BOOLEAN     NOT NULL DEFAULT false,
    PRIMARY KEY (hook_id, seq)
);

-- Ring-buffer reads (newest first) and the eviction query both scan by
-- (hook_id, seq); DESC matches "newest prepended" list order directly.
CREATE INDEX hook_requests_hook_seq_idx ON hook_requests (hook_id, seq DESC);

-- Reference-count large captured-body blobs so the ADR-0008 GC reaper can
-- reclaim one once no request row (or artifact body, or span output) points
-- at it any more.
CREATE INDEX hook_requests_body_ref_idx ON hook_requests (body_ref_sha256);
