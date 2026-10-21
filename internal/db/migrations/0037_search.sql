-- Search index foundation (ADR-0028, SPEC-0022 REQ-2, REQ-10, issue #306).
--
-- Two tables: the lexical index itself, and the queue the indexer drains. Both
-- are keyed by the internal artifact id with ON DELETE CASCADE, so the index's
-- lifetime is the artifact's: the reaper's expiry sweep, an owner delete, and
-- every tombstoning removal drop the document row in the same transaction as
-- the artifact row (REQ-10), with no cleanup job to run.
--
-- Bodies are not in this database (ADR-0008), so the indexed text is a copy
-- the indexer writes from object storage, capped and already redacted at
-- ingest (SPEC-0017). Nothing here touches `artifacts`.
--
-- The owner columns follow ADR-0029's split (migration 0023), which is why
-- SPEC-0022's design note "becomes owner_user_id / owner_team_id with
-- ADR-0029" is already satisfied: ADR-0029 landed before this story. The
-- search query the next story ships filters with
-- (owner_user_id = ANY($1) OR owner_team_id = ANY($2)), and the two B-tree
-- indexes below back exactly that, newest first with the keyset tie-break.
--
-- `schema` and `retention` are reserved for the filters SPEC-0022 REQ-4 names:
-- no write path stores an artifact metadata schema or a retention mode today,
-- so both stay NULL / 'ephemeral' until a story introduces one. They are here
-- so that story does not need a second table rebuild.

-- One row per indexed artifact.
CREATE TABLE IF NOT EXISTS search_documents (
    artifact_id   BIGINT      PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
    owner_user_id UUID        REFERENCES users(id) ON DELETE CASCADE,
    owner_team_id UUID,
    share_type    TEXT        NOT NULL,
    tags          TEXT[]      NOT NULL DEFAULT '{}',
    schema        TEXT,
    retention     TEXT        NOT NULL DEFAULT 'ephemeral',
    created_at    TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    doc           TSVECTOR    NOT NULL,
    content       TEXT        NOT NULL DEFAULT '',
    indexed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT search_documents_one_owner CHECK (num_nonnulls(owner_user_id, owner_team_id) = 1)
);

COMMENT ON COLUMN search_documents.doc IS
    'Weighted tsvector: A=title, B=tags, C=body text, D=comment text (ADR-0028).';
COMMENT ON COLUMN search_documents.content IS
    'Capped, ingest-redacted text kept for ts_headline snippets; a copy, deleted by cascade.';
COMMENT ON COLUMN search_documents.schema IS
    'Metadata schema id when an artifact carries one (e.g. cairn.receipt/v1). NULL: no write path stores one yet.';

CREATE INDEX IF NOT EXISTS search_documents_doc_idx
    ON search_documents USING GIN (doc);
CREATE INDEX IF NOT EXISTS search_documents_owner_user_idx
    ON search_documents (owner_user_id, created_at DESC, artifact_id DESC)
    WHERE owner_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS search_documents_owner_team_idx
    ON search_documents (owner_team_id, created_at DESC, artifact_id DESC)
    WHERE owner_team_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS search_documents_tags_idx
    ON search_documents USING GIN (tags);

-- The indexer's work queue (SPEC-0022 REQ-2 "Indexing MUST run asynchronously
-- after the creating transaction commits"). One row per artifact waiting to be
-- (re)indexed: a second enqueue for the same artifact folds into the first, so
-- a burst of comments costs one index pass, not one per comment. A row that
-- dies with its artifact dies by cascade, so a delete during indexing cannot
-- leave work behind.
--
-- The queue is deliberately NOT polled by a trigger: the write paths enqueue
-- themselves after their commit, which is what keeps a create from ever
-- failing because the indexer is down.
CREATE TABLE IF NOT EXISTS search_queue (
    artifact_id BIGINT      PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
    reason      TEXT        NOT NULL,
    attempts    INT         NOT NULL DEFAULT 0,
    not_before  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error  TEXT        NOT NULL DEFAULT '',
    CONSTRAINT search_queue_reason_check CHECK (reason IN ('created', 'comment', 'metadata', 'reindex'))
);

COMMENT ON COLUMN search_queue.reason IS
    'created | comment | metadata | reindex — why this artifact needs indexing.';
COMMENT ON COLUMN search_queue.not_before IS
    'Earliest claim time: now for a fresh enqueue, later for a backoff retry.';

-- The indexer claims due rows with FOR UPDATE SKIP LOCKED, so N workers drain
-- one queue without serializing on each other.
CREATE INDEX IF NOT EXISTS search_queue_claim_idx ON search_queue (not_before, artifact_id);
