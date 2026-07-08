---
status: accepted
date: 2026-07-08
decision-makers: joestump
extends: [ADR-0001]
related: [ADR-0005]
---

# ADR-0008: Storage & Content Model — Blob Bodies, Metadata, Content Addressing

## Context and Problem Statement

A Cairn artifact is two things at once: a **body** (markdown text, source code, an
image, a generic file up to tens of MB, the members of a bundle, a captured
trajectory payload, or a webhook request capture) and a set of **metadata**
(provenance, access policy, expiry, annotations, share type, title). These have
opposite storage profiles — bodies are large, immutable, and often duplicated across
artifacts; metadata is small, mutable, relational, and heavily queried. Where does
each live, how do we get integrity and the visible "checksum" affordance, how do we
stream a 42.7 MB file up with progress, how do bundles model N members, and how does
the content hash relate to the short public id from ADR-0005?

## Decision Drivers

* **Integrity and a visible checksum.** The File viewer surfaces a `checksum`
  affordance; agents and humans need to trust that the bytes they read are the bytes
  that were shared. A cryptographic content hash gives both.
* **Deduplication.** Re-sharing the same file, or many agents dropping identical
  receipts, should store the bytes once.
* **Large bodies.** Files reach tens of MB; those do not belong in Postgres rows,
  and must upload as a stream with CLI progress rather than one buffered request.
* **Relational metadata.** Provenance, access, expiry (ADR-0007), and the annotation
  layer (ADR-0006) are queried, filtered, and joined — they belong in a relational
  store, not next to the bytes.
* **Self-hostable.** The object store must be an S3-compatible target the operator
  already runs (Garage / MinIO), reachable privately.
* **Immutability.** Stable annotation anchors (ADR-0006), cacheable downloads, and
  trustworthy provenance all depend on a body never changing under an id.
* **Opaque public ids.** ADR-0005 requires short, non-enumerable public identifiers;
  the storage model must not force the content hash to become the URL.

## Considered Options

* **Option A — Split store: bodies in S3-compatible object storage,
  content-addressed by SHA-256; metadata in PostgreSQL.** Postgres holds a `blobs`
  registry (hash → size, media type, storage key) plus the `artifacts` rows and all
  relational metadata; the object store holds the bytes keyed by their hash.
* **Option B — Everything in PostgreSQL.** Bodies as `bytea` or Large Objects
  alongside metadata; one store to run.
* **Option C — Bodies on a content-addressed local filesystem; metadata in
  PostgreSQL.** A CAS directory tree (`blobs/aa/bb/<hash>`) on disk instead of an
  object store.

## Decision Outcome

Chosen option: **"Split store — bodies in S3-compatible object storage,
content-addressed by SHA-256; metadata in PostgreSQL" (Option A)**, because it puts
each kind of data on the substrate built for it: an object store streams and serves
tens-of-MB bodies and gives us content-addressed dedup and integrity for free, while
Postgres does what it is good at for the small, mutable, heavily-queried metadata.
Putting bodies in Postgres (Option B) bloats the database, ruins its cache locality,
and makes tens-of-MB uploads a buffered `bytea` liability. A local CAS filesystem
(Option C) is operationally simplest for a single node but forfeits horizontal
scale, off-node durability, and the S3 API the rest of the stack already speaks —
and a self-hoster can still point Cairn at MinIO or Garage on the same box, getting
Option C's simplicity without its ceiling.

### The split

* **PostgreSQL** owns: `artifacts` (public id, share type, title, provenance,
  access, expiry, denormalized annotation counts), `blobs` (the content registry),
  `bundle_members`, and the annotation tables from ADR-0006. This is the source of
  truth for everything you can query or list (the Bin).
* **S3-compatible object storage** owns: the raw body bytes, and nothing else. No
  metadata is authoritative in the object store; it is a content-addressed byte
  bucket. If the store were wiped and rebuilt from an off-site copy, Postgres would
  still describe every artifact.

### Content addressing

Every distinct body is a **blob**, named by the lowercase hex SHA-256 of its bytes:

```
CREATE TABLE blobs (
  sha256       CHAR(64)    PRIMARY KEY,        -- hex; the "checksum" affordance
  size_bytes   BIGINT      NOT NULL,
  media_type   TEXT        NOT NULL,           -- sniffed + declared, e.g. image/png
  storage_key  TEXT        NOT NULL,           -- object key, derived from sha256
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

The object key is derived from the hash and sharded to avoid hot prefixes, e.g.
`blobs/<aa>/<bb>/<sha256>`. Because the name *is* the hash:

* **Dedup** is automatic — a body whose hash already exists in `blobs` is never
  re-uploaded; the new artifact simply references the existing blob.
* **Integrity** is verifiable — the server computes SHA-256 while it streams the
  bytes in and rejects a body whose computed hash does not match, and any reader can
  re-verify by hashing what they downloaded.
* **The checksum affordance** in the File viewer is just this `sha256` (shown in full
  or as a short prefix), the same value the CLI can print.

An `artifacts` row references its body by `body_sha256` (a foreign key into `blobs`);
bundles reference many blobs through `bundle_members` (below). Blobs are shared,
immutable, and reference-counted for garbage collection.

### Bundles: N members

A **bundle** is an artifact of share type `bundle` whose body is a manifest rather
than a single blob. Members are modeled relationally, each pointing at a
content-addressed blob so mixed-media members dedup like any other body:

```
CREATE TABLE bundle_members (
  bundle_id    BIGINT   NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  ordinal      INT      NOT NULL,               -- tab order in the viewer
  name         TEXT     NOT NULL,               -- member filename / path
  blob_sha256  CHAR(64) NOT NULL REFERENCES blobs(sha256),
  media_type   TEXT     NOT NULL,
  size_bytes   BIGINT   NOT NULL,
  PRIMARY KEY (bundle_id, ordinal)
);
```

Members are **addressable within the bundle** — `<bundle_id>/<name>` — for both the
web tabbed viewer and MCP reads ("agents read the same files over MCP"), but they are
*not* top-level artifacts and carry no public base62 id of their own; the bundle owns
the short URL. `cairn add f1 f2 f3` creates one bundle artifact and one
`bundle_members` row per file, each file streamed and dedup'd independently.

### Streaming upload for large files

Uploads stream **through the API** (ADR-0012), not by presigned direct-to-store PUTs:

1. The CLI opens a chunked/multipart upload to the core service and streams bytes,
   rendering per-file progress (the `cairn add` progress bars).
2. The service streams those bytes to the object store — using S3 multipart for
   large bodies — while computing SHA-256 incrementally and enforcing the size/quota
   limit as it goes.
3. On completion it finalizes: verify the hash, upsert the `blobs` row (or discard
   the just-uploaded object if the hash already existed — dedup), and create the
   `artifacts` / `bundle_members` rows.

Streaming through the API (rather than handing the client a presigned URL) keeps a
single ingress endpoint, lets the server compute and *verify* the hash and enforce
quotas, and works when the object store is not publicly reachable — the common
self-host topology. Presigned direct upload is noted as a future optimization for
very large bodies, gated on the server still being able to verify the hash
post-upload.

### Previewability

Whether a body renders in a rich viewer or falls back to the File viewer's
"not previewable · size · checksum · download" path is decided at ingest and
recorded on the artifact:

* The share type plus the blob's `media_type` (sniffed from magic bytes and
  reconciled with the declared type) select a viewer via the registry (ADR-0002).
* If a viewer exists for that type/media pair **and** the body is within the
  preview size bound, the artifact is `previewable = true`; otherwise it is the
  generic File type (`FILE`/`GZ`), previewable = false — e.g. `staging-db-dump.sql.gz`
  at 42.7 MB.
* This keeps the "not previewable" decision a property of the stored artifact, so
  every surface agrees without re-sniffing on each request.

### Content hash vs. the public id

The SHA-256 and the public base62 id (ADR-0005) are **deliberately decoupled**:

* The **public id** is a short, opaque, randomly minted base62 handle, one per
  artifact, and is what appears in `cairn.sh/<id>`. It is non-enumerable and reveals
  nothing about the content.
* The **SHA-256** is the blob's integrity/dedup key and the visible checksum. It is
  *never* the URL.

They must stay separate because using the content hash as the URL would (a) make
identical bytes share a link, leaking that two shares are byte-equal and collapsing
their distinct provenance, access, and expiry; and (b) let anyone who can guess or
reproduce the content compute the URL, defeating the opacity ADR-0005 requires. So
two artifacts with identical bodies share one blob (one copy of the bytes, one
checksum) yet have two different public ids, two provenances, and two independent
TTLs.

### Lifecycle and garbage collection

Artifacts are ephemeral (ADR-0007). Expiry and deletion remove `artifacts` rows and
their `bundle_members`; a blob becomes eligible for GC only when its reference count
across all live artifacts and bundle members reaches zero. A reaper sweeps
zero-reference blobs from the object store on a delay (to tolerate races with
in-flight uploads and dedup lookups). Object-store lifecycle rules are a backstop,
not the primary mechanism, since Postgres is the authority on what is still
referenced.

### Consequences

* Good, because bodies get an integrity guarantee, free dedup, and the visible
  checksum from a single property — the SHA-256 content address.
* Good, because immutable, content-addressed bodies give ADR-0006 the byte-stable
  substrate its anchors depend on, and make downloads cacheable by hash.
* Good, because tens-of-MB files stream in and out of purpose-built object storage
  without touching Postgres row size or its buffer cache.
* Good, because the split is self-host friendly: any S3-compatible store (Garage,
  MinIO) works, privately reachable, with Postgres as the single queryable
  source of truth.
* Bad, because there are now two stores to operate and keep consistent; a crash
  between "object written" and "row committed" can orphan an object. Mitigated by
  making the DB the authority and letting the GC reaper collect unreferenced
  objects, so orphans are harmless and eventually swept.
* Bad, because streaming through the API puts upload bandwidth on the app tier rather
  than offloading to presigned PUTs; accepted for v1 in exchange for hash
  verification, quota enforcement, and a private object store.
* Neutral, because bundle members are addressable but not independently shareable —
  a member has no short id of its own; re-sharing a member means minting a new
  single-file artifact (which will dedup to the same blob).

### Confirmation

* A **round-trip integrity test** uploads a body, downloads it via the artifact, and
  asserts the returned bytes hash to the stored `sha256`.
* A **dedup test** shares identical bytes as two artifacts and asserts one `blobs`
  row / one stored object but two distinct public ids, provenances, and expiries.
* A **hash-mismatch test** injects a corrupted stream and asserts ingest rejects it.
* A **large-file streaming test** uploads a tens-of-MB body via multipart, asserts
  progress is reported and the object lands as a File-type, non-previewable artifact
  with a correct checksum.
* A **bundle test** runs `cairn add` on mixed media and asserts one bundle artifact,
  ordered `bundle_members`, per-member blobs, and member addressability over both the
  web tabs and MCP.
* A **GC test** expires the last artifact referencing a blob and asserts the reaper
  removes the object while a still-referenced blob is retained.
