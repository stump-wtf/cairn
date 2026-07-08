-- Initial schema for the Artifact core (SPEC-0002 / ADR-0008).
--
-- Split store: bodies live content-addressed in object storage; this database
-- is the single queryable source of truth for all metadata.

-- The content registry: one row per distinct body, named by the lowercase hex
-- SHA-256 of its bytes. Identical bytes dedup to one row and one stored object.
CREATE TABLE blobs (
    sha256      CHAR(64)    PRIMARY KEY,          -- hex; the visible checksum
    size_bytes  BIGINT      NOT NULL,
    media_type  TEXT        NOT NULL,             -- sniffed + declared
    storage_key TEXT        NOT NULL,             -- sharded object key from the hash
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The Artifact aggregate root. public_id is the opaque base62 handle in URLs;
-- id is the never-exposed internal key. body_sha256 is null only for bundles,
-- whose members are modeled in bundle_members.
CREATE TABLE artifacts (
    id             BIGSERIAL   PRIMARY KEY,
    public_id      TEXT        NOT NULL UNIQUE,
    share_type     TEXT        NOT NULL,
    title          TEXT        NOT NULL DEFAULT '',
    body_sha256    CHAR(64)    REFERENCES blobs(sha256),
    size_bytes     BIGINT      NOT NULL DEFAULT 0,
    media_type     TEXT        NOT NULL DEFAULT 'application/octet-stream',
    previewable    BOOLEAN     NOT NULL DEFAULT false,
    -- Provenance (immutable, server-derived; ADR-0007).
    actor_id       TEXT        NOT NULL,
    on_behalf_of   TEXT        NOT NULL DEFAULT '',
    channel        TEXT        NOT NULL,
    captured_at    TIMESTAMPTZ NOT NULL,
    -- Access policy (ADR-0007).
    owner_id       TEXT        NOT NULL,
    visibility     TEXT        NOT NULL DEFAULT 'link',
    -- Denormalized annotation counts (SPEC-0006 maintains these).
    reaction_count INT         NOT NULL DEFAULT 0,
    comment_count  INT         NOT NULL DEFAULT 0,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The Bin is ordered by created_at (public ids are random, never time-ordered)
-- and keyset-paginated over (created_at, id).
CREATE INDEX artifacts_created_at_id_idx ON artifacts (created_at, id);

-- Reference-counting the shared blob for GC: how many live artifacts point at it.
CREATE INDEX artifacts_body_sha256_idx ON artifacts (body_sha256);

-- Bundle members: ordered, each pointing at a content-addressed blob so members
-- dedup like any other body. Members are addressable as <bundle_id>/<name> but
-- carry no public id of their own.
CREATE TABLE bundle_members (
    bundle_id   BIGINT   NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    ordinal     INT      NOT NULL,
    name        TEXT     NOT NULL,
    blob_sha256 CHAR(64) NOT NULL REFERENCES blobs(sha256),
    media_type  TEXT     NOT NULL,
    size_bytes  BIGINT   NOT NULL,
    PRIMARY KEY (bundle_id, ordinal)
);

CREATE INDEX bundle_members_blob_sha256_idx ON bundle_members (blob_sha256);
