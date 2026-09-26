---
status: draft
date: 2026-07-08
implements: [ADR-0001, ADR-0002, ADR-0008, ADR-0005, ADR-0018]
---

# SPEC-0002: Artifact Core and Share Types

## Overview

This capability is the backbone every other Cairn spec builds on. It defines the
**Artifact aggregate** and its lifecycle (create / read / list / delete), the **share-type
registry** that classifies an artifact and selects its viewer / panel / anchor affordances,
the **content and storage model** (bodies content-addressed by SHA-256 in object storage,
metadata in PostgreSQL, bundles with N members, previewability decided at ingest, streaming
upload with checksum verification), and the **short opaque public identifiers and URL
scheme** that address every artifact.

It realizes ADR-0001 (one unified Artifact aggregate with a pluggable share type), ADR-0002
(a compile-time `ShareType` registry, open for extension, closed for modification, with
total resolution to the generic file handler), ADR-0008 (split store: SHA-256-addressed
blobs in S3-compatible storage + relational metadata in Postgres, dedup, streaming upload,
reference-counted GC), and ADR-0005 (random base62 public ids, decoupled from the content
hash and the internal key, with a path scheme per surface). It exposes the `/v1` REST/JSON
API that the web and CLI surfaces project and the MCP server adapts (ADR-0003/ADR-0012). It
is WEB(API)-facing and BACKEND; it is not itself UI, so it carries Security requirements and
backend-quality requirements (error handling, concurrency for streaming uploads, database
operations) but no accessibility section.

## Requirements

### Requirement: Artifact Aggregate and Invariants

An Artifact MUST be the single shared unit and the aggregate root. Every artifact MUST
carry: a short opaque public id; a share type; a body reference (or a bundle manifest);
metadata (title, size, media/language hints, type-specific fields, optional client-asserted
tags); provenance (actor,
channel, capture time); an access policy; an expiry; and an annotation stream. At creation
every artifact MUST have provenance, an access policy, and an expiry set — none is optional.
No competing top-level shareable entity MUST be introduced; Bundle and the Bin are shapes
and views of the Artifact, not separate aggregates.

#### Scenario: Create sets required invariants

- **WHEN** an artifact is created through any surface
- **THEN** it MUST persist a public id, share type, body reference, provenance, an access
  policy, and an expiry, and MUST expose an (initially empty) annotation stream

#### Scenario: No parallel aggregate

- **WHEN** a new kind (image, webhook, trajectory) is added
- **THEN** it MUST be modeled as a share type over the Artifact, not as a separate
  top-level entity

### Requirement: Artifact Lifecycle — Create

The core MUST provide a single create operation, invoked identically by web, CLI, and MCP
adapters (ADR-0003), that streams the body to storage (below), records server-derived
provenance and channel, assigns the default access policy and TTL (ADR-0007), and returns
the artifact with its public id and short URL. Provenance channel MUST be derived
server-side from the authenticated surface and MUST NOT be taken from a client claim.

#### Scenario: Create returns an addressable artifact

- **WHEN** a client creates an artifact
- **THEN** the response MUST include the minted public id and the `cairn.stump.wtf/<id>` short URL,
  and the artifact MUST be immediately resolvable

#### Scenario: Channel is server-derived

- **WHEN** a client authenticated over MCP asserts `via CLI` in the request
- **THEN** the recorded channel MUST be `via MCP`, overriding the client claim

### Requirement: Artifact Tags

An artifact MAY carry **tags**: a list of short strings. Tags are set once at creation
through any create surface (REST, CLI, or MCP; single-body and bundle alike),
persisted with the artifact, and returned wherever the artifact is read or listed.

Tags are **client-asserted metadata, not provenance**. The system MUST NOT derive,
rewrite, or interpret them. Every consumer, including outbound-event consumers
(SPEC-0012), MUST NOT base a trust or authorization decision on a tag; trust comes
only from the server-derived actor and channel (ADR-0018).

At creation the system MUST normalize tags:

- each tag MUST be 1–64 bytes drawn from lowercase `[a-z0-9._:/#-]`, and a create
  with any other tag MUST be rejected; nothing is truncated or case-folded;
- exact repeats MUST be dropped, keeping first-occurrence order;
- more than 32 distinct tags MUST be rejected.

Over REST, a create carries tags as repeated `tag` query parameters, repeated
`X-Cairn-Tags` headers, or both. Each value is a comma-separated list, and a multipart
create additionally accepts repeated `tag` form fields. All sources on one request
are merged. `GET /v1/bin` accepts the same `tag` parameters and MUST return only
artifacts that carry every given tag.

The system MUST NOT enforce a tag vocabulary. Conventions such as the handoff
convention (ADR-0018) are documentation that consumers match on.

#### Scenario: Tags round-trip

- **WHEN** a client creates an artifact with tags `handoff`, `lane:auto`, and
  `handoff` again
- **THEN** the create response, a later read, and the Bin listing MUST each return
  exactly `handoff`, `lane:auto`, and an artifact created without tags MUST return none

#### Scenario: Out-of-bounds tag rejected

- **WHEN** a create carries the tag `Handoff`, a 65-byte tag, or 33 distinct tags
- **THEN** the system MUST reject the request as `validation_failed`, persist nothing,
  and emit no creation event

#### Scenario: Bin filtered by tag

- **WHEN** the owner lists the Bin with `tag=handoff&tag=size:s`
- **THEN** only artifacts carrying both tags MUST be returned

#### Scenario: Tags confer no trust

- **WHEN** a create carries a tag such as `actor:someone-else` or `channel:cli`
- **THEN** the recorded provenance MUST still be the server-derived actor and channel,
  and the tag MUST remain an ordinary tag

### Requirement: Artifact Lifecycle — Read

The core MUST resolve an artifact by its public id, returning its metadata and a
rendered/preview payload for the resolved share type. Resolution MUST return a uniform
not-found result for unknown, unauthorized, and expired ids alike, so probing leaks no
signal (ADR-0005/ADR-0007). Raw body bytes MUST be downloadable in a way a reader can
re-verify against the stored SHA-256.

#### Scenario: Read by id

- **WHEN** a client reads a valid, unexpired id
- **THEN** the core MUST return the artifact metadata and the preview payload for its share
  type

#### Scenario: Uniform not-found

- **WHEN** a client reads an unknown, unauthorized, or expired id
- **THEN** the core MUST return an identical not-found (404) result in every case

### Requirement: Artifact Lifecycle — List (the Bin)

The core MUST provide a Bin listing over a workspace's artifacts, keyset-paginated over
`(created_at, id)` so it neither skips nor duplicates rows while artifacts are inserted and
expired mid-scroll. The listing MUST be ordered by `created_at`, never by public id (ids are
random and non-time-ordered per ADR-0005). This one query MUST back both the web Bin
(SPEC-0001) and the CLI TUI (ADR-0003).

#### Scenario: Stable keyset pagination

- **WHEN** the Bin is paginated while artifacts are created and expired
- **THEN** the keyset cursor MUST advance without skipping or duplicating rows

### Requirement: Artifact Lifecycle — Delete and Expiry

The core MUST provide a delete operation restricted to the owning principal, and MUST honor
expiry as hard deletion: past `expires_at` the artifact's metadata rows (artifact,
annotations, provenance) MUST be removed and its body dereferenced, after which the id MUST
return the same uniform 404 as a never-existed id (ADR-0007). A body blob MUST be
garbage-collected only when its content-hash reference count across all live artifacts and
bundle members reaches zero.

#### Scenario: Owner deletes

- **WHEN** the owner deletes an artifact
- **THEN** its metadata MUST be removed, its id MUST thereafter return a uniform 404, and its
  blob MUST be dereferenced

#### Scenario: Shared blob survives

- **WHEN** an artifact is deleted whose body blob is still referenced by another live
  artifact
- **THEN** the blob MUST be retained until its reference count reaches zero

### Requirement: Share-Type Registry and Total Resolution

Share types MUST be modeled as values satisfying a single `ShareType` interface, registered
into a central registry at compile time (ADR-0002). The core and the shell MUST resolve a
type's handler through the registry and MUST NOT `switch` on share type. Resolution MUST be
total: an unregistered or uninterpretable type MUST resolve to the built-in generic file
handler so the artifact stays viewable, downloadable, and annotatable at the whole-artifact
level. Adding a type MUST NOT require editing the Artifact aggregate, the storage layer, or
the shell chrome.

#### Scenario: Unknown type resolves to generic file

- **WHEN** an artifact of an unregistered share type is resolved
- **THEN** the registry MUST return the generic file handler and the artifact MUST remain
  viewable and downloadable

#### Scenario: No switch on type

- **WHEN** the core or shell selects a viewer, panel, or anchor rule for an artifact
- **THEN** it MUST resolve through the registry, not via a `switch` over the type set

### Requirement: Per-Type Anchor Affordances

Each registered type MUST declare the set of annotation anchors legal for it (e.g. markdown
block/bullet + selection, code line/selection, image region pin, webhook request as
reaction-only, trajectory span/turn/tool-call + selection, whole-artifact always legal). The
annotation layer (SPEC-0006) MUST validate anchors against this declared set as the single
source of truth, so a viewer and its validator cannot drift. The "webhook requests are
reactable but not comment-threaded" rule MUST be expressed as a property of the type.

#### Scenario: Illegal anchor rejected

- **WHEN** an annotation is submitted against an anchor a type does not declare (e.g. a
  comment thread on a webhook request)
- **THEN** it MUST be rejected using the type's declared affordances

### Requirement: Content Addressing and Blobs

Every distinct body MUST be stored as a blob named by the lowercase hex SHA-256 of its
bytes, registered in a Postgres `blobs` table (`sha256` primary key, size, media type,
storage key) with the bytes in S3-compatible object storage under a sharded key derived from
the hash. Identical bytes MUST store once (dedup): a body whose hash already exists MUST NOT
be re-uploaded, and the new artifact MUST reference the existing blob. The SHA-256 MUST be
the visible checksum affordance and MUST be re-verifiable by any reader.

#### Scenario: Dedup identical bytes

- **WHEN** two artifacts are created from byte-identical bodies
- **THEN** there MUST be exactly one `blobs` row and one stored object, referenced by two
  distinct artifacts with two public ids

#### Scenario: Round-trip integrity

- **WHEN** a reader downloads an artifact body and hashes it
- **THEN** the result MUST equal the stored `sha256` checksum

### Requirement: Streaming Upload with Checksum Verification

Bodies MUST be uploaded by streaming **through the API** (not presigned direct-to-store),
computing SHA-256 incrementally as bytes arrive, enforcing the configured size/quota limit
during the stream, and finalizing by verifying the computed hash, upserting the `blobs` row
(or discarding the just-uploaded object on a dedup hit), and creating the artifact /
bundle-member rows. A body whose computed hash does not match, or which exceeds the limit,
MUST be rejected and MUST NOT leave a persisted partial artifact.

#### Scenario: Hash mismatch rejected

- **WHEN** an uploaded stream's computed SHA-256 does not match on finalize
- **THEN** ingest MUST reject it and MUST NOT create an artifact or a `blobs` row

#### Scenario: Oversize stream rejected mid-stream

- **WHEN** an upload exceeds the configured size/quota limit
- **THEN** the server MUST reject it (413) without persisting a partial blob or artifact

### Requirement: Bundles with N Members

A bundle MUST be an artifact of share type `bundle` whose body is a manifest of ordered
members, each modeled in `bundle_members` (ordinal, name, `blob_sha256`, media type, size)
and each pointing at a content-addressed blob so members dedup like any other body. Members
MUST be addressable within the bundle as `<bundle_id>/<name>` for both the web tabbed viewer
and MCP reads, and MUST NOT carry a public base62 id of their own — the bundle owns the
single short URL.

#### Scenario: cairn add creates one bundle

- **WHEN** `cairn add f1 f2 f3` is invoked on mixed media
- **THEN** exactly one bundle artifact MUST be created with one ordered `bundle_members` row
  per file, each member streamed and dedup'd independently

#### Scenario: Member addressable, not independently shareable

- **WHEN** a bundle member is read
- **THEN** it MUST be addressable as `<bundle_id>/<name>` over web and MCP but MUST NOT have
  its own public id

### Requirement: Previewability Detection at Ingest

Whether a body renders in a rich viewer or falls back to the generic file path MUST be
decided at ingest and recorded on the artifact. The share type plus the blob's sniffed +
declared media type MUST select a viewer via the registry; if a viewer exists for that
type/media pair and the body is within the preview size bound, `previewable` MUST be true,
otherwise the artifact MUST be the generic file type (`FILE`/`GZ`), `previewable = false`.
This decision MUST be a stored property so every surface agrees without re-sniffing.

#### Scenario: Large blob is non-previewable

- **WHEN** a tens-of-MB body outside the preview bound is ingested
- **THEN** the artifact MUST be recorded as a generic file, `previewable = false`, with a
  correct checksum

### Requirement: Short Opaque Public Identifiers and URL Scheme

Public ids MUST be random base62 (`0-9 A-Z a-z`, case-sensitive) of a standardized default
length of 8 characters (~47.6 bits), minted independently of the body's SHA-256 and the
internal primary key. Generation MUST be generate → atomic unique insert → regenerate on the
rare conflict; a retired id MUST NOT be reused within its TTL-plus-grace window. Reserved
route words (`run`, `hook`, `api`, `settings`, `.well-known`, …) MUST be excluded from the
generator. The path scheme MUST be: `cairn.stump.wtf/<id>` for default artifacts, `cairn.stump.wtf/run/<id>`
for trajectories, `mcp://cairn/<id>` (and `mcp://cairn/hook/<id>`, `mcp://cairn/run/<id>`)
for agent handles; the same id token MUST be reused verbatim across all surfaces.

#### Scenario: Id is opaque and decoupled

- **WHEN** a public id is minted
- **THEN** it MUST be base62 of the default length, MUST NOT equal the artifact's content
  hash or internal key, and MUST reveal no order, count, or content

#### Scenario: Collision retry

- **WHEN** a generated id collides with an existing `public_id`
- **THEN** the server MUST regenerate and retry until the atomic unique insert succeeds

#### Scenario: Reserved prefix excluded

- **WHEN** the generator produces a candidate matching a reserved route word
- **THEN** it MUST be rejected so an id can never collide with a route prefix

### Requirement: Error Handling Standards

Errors MUST be wrapped with context at each layer boundary (e.g. `fmt.Errorf("load
artifact %s: %w", id, err)`), preserving the chain with `%w` so handlers map a domain error
to a stable machine `code` (`not_found`, `unauthorized`, `forbidden`, `validation_failed`,
`conflict`, `payload_too_large`, `rate_limited`, `internal`, …) without string matching.
Sentinel/domain errors MUST be defined for failures callers distinguish (e.g. not-found vs.
dedup-conflict). No error MUST be silently swallowed; failures MUST be logged with structured
key-value context including a `request_id`. Every non-2xx API response MUST be the single
structured error envelope of ADR-0012.

#### Scenario: Domain error maps to a stable code

- **WHEN** a read misses because the artifact does not exist or has expired
- **THEN** the core MUST return a distinguishable not-found domain error that the adapter
  renders as the `not_found` envelope with an aligned HTTP status and a `request_id`

### Requirement: Concurrency Safety for Streaming Uploads

Every core method MUST take `context.Context` as its first argument, and the request context
(deadline, cancellation, request-scoped identity, `request_id`) MUST propagate through the
service to the database and object-store calls, so a cancelled request or a hung upload
releases its resources. Streaming ingest MUST have an explicit lifecycle: a cancelled or
disconnected upload MUST abort the in-flight object write and MUST NOT commit an artifact
row; shared state touched during concurrent ingest MUST be race-safe and exercised under the
race detector in CI.

#### Scenario: Cancelled upload releases resources

- **WHEN** the client disconnects mid-upload (context cancelled)
- **THEN** the in-flight object write MUST be aborted, no artifact row MUST be committed, and
  any partial object MUST be left only as GC-collectable debris

### Requirement: Database Operation Standards

Multi-step mutations (finalize upload → upsert blob → insert artifact / bundle members) MUST
run in a single transaction so a failure leaves no half-created artifact. Database access
MUST use an explicit connection lifecycle with timeouts driven by the request context. All
SQL MUST use bound parameters (`$1`, `$2`, …); no query MUST be assembled by string
concatenation of caller input.

#### Scenario: Atomic finalize

- **WHEN** creating a bundle fails after some member rows are written
- **THEN** the enclosing transaction MUST roll back so no partial bundle is visible

#### Scenario: Parameterized queries only

- **WHEN** any query incorporates caller-supplied input
- **THEN** it MUST bind that input as a parameter and MUST NOT interpolate it into SQL text

## Security Requirements

This capability is web-facing (the `/v1` REST/JSON API). The following are MANDATORY.

### Requirement: Authentication & Authorization

Mutating and workspace-scoped endpoints (`POST /v1/artifacts`, `DELETE /v1/artifacts/{id}`,
`POST /v1/artifacts/{id}/share`, `GET /v1/bin`) MUST require authentication (session for
web, OAuth 2.1 bearer for API/MCP/CLI per ADR-0004). Link-capability reads (`GET
/v1/artifacts/{id}`, `GET /v1/artifacts/{id}/body`, bundle-member reads) MUST enforce the
ADR-0007 access policy: a valid id grants read, and unknown/unauthorized/expired ids return
a uniform 404. Agents MUST NOT exceed the human principal's permissions; artifacts an agent
creates are owned by the human and default to `you + anyone with link`, and agents get no
`sharing:manage` scope.

#### Scenario: Unauthenticated mutation

- **WHEN** an unauthenticated client calls a mutating endpoint (create, delete, share)
- **THEN** the server MUST respond 401 and make no change

#### Scenario: Agent cannot broaden sharing

- **WHEN** an agent without `sharing:manage` calls the share endpoint
- **THEN** the server MUST respond 403 and leave the access policy unchanged

### Requirement: Rate Limiting

All public and ingress endpoints — chiefly id resolution (`GET /v1/artifacts/{id}`) and
create — MUST be rate-limited per-identity/per-IP; limits MUST return 429 with `Retry-After`.
Rate limiting on id resolution is part of the ADR-0005 defense-in-depth against enumeration.

#### Scenario: Burst on id resolution

- **WHEN** a client exceeds the configured rate resolving artifact ids
- **THEN** the server MUST respond 429 with `Retry-After` without processing the request

### Requirement: Security Headers

API responses MUST set `X-Content-Type-Options: nosniff` and (over HTTPS) HSTS, and body
downloads MUST be served with a content type and disposition that prevent the bytes from
executing in Cairn's app origin (e.g. non-sniffable download, isolated/download disposition
for untrusted bodies). A strict Content-Security-Policy MUST apply to any HTML the API emits.

#### Scenario: Untrusted body cannot execute

- **WHEN** a body containing active content (script/HTML) is downloaded
- **THEN** it MUST be served so it cannot execute in Cairn's origin (non-sniffable,
  isolated/download disposition)

### Requirement: Request Body Size Limits

Every endpoint that accepts a body MUST enforce a maximum request/upload size; oversize
requests MUST be rejected with 413 **before** the full body is buffered, and streaming
ingest MUST enforce the limit incrementally so no partial blob or artifact is persisted
(see Streaming Upload above).

#### Scenario: Oversize upload

- **WHEN** an upload exceeds the configured limit
- **THEN** the server MUST reject it with 413 and MUST NOT persist a partial blob or
  artifact

### Requirement: CSRF Protection

Cookie/session-authenticated state-changing requests (create/delete/share from the web
surface) MUST be CSRF-protected via token and/or SameSite strategy. Token-authenticated
API/MCP/CLI requests presenting an OAuth bearer are exempt (no ambient credentials).

#### Scenario: Cross-site state change

- **WHEN** a session-authenticated create/delete/share arrives without a valid CSRF token
- **THEN** the server MUST reject it and make no change

### Requirement: Redirect & SSRF Validation

The API MUST NOT honor a user-supplied absolute URL for any redirect; redirect targets MUST
be validated against an allow-list of internal paths. Any server-side fetch of a
user-supplied URL MUST be guarded against SSRF (block internal/link-local ranges and
metadata endpoints).

#### Scenario: SSRF attempt on a user-supplied URL

- **WHEN** a request causes the server to fetch a user-supplied URL pointing at an internal
  or link-local address
- **THEN** the server MUST refuse the fetch

## REST Endpoints

The `/v1` REST/JSON API (ADR-0012) backed by the core service; the web and CLI surfaces
project it and the MCP server adapts it (ADR-0003). Auth-by-default: every endpoint is
`Auth: Required` unless explicitly `Public` with a justification. Annotation endpoints
(reactions, comments) and the live SSE streams are governed by SPEC-0006, SPEC-0005, and
SPEC-0004 respectively and are not owned here.

| Method | Path | Purpose | Auth |
|--------|------|---------|------|
| POST | `/v1/artifacts` | Create an artifact; streams the body to storage with checksum verification (ADR-0008) | Required |
| GET | `/v1/artifacts/{id}` | Fetch metadata + preview payload for the resolved share type | Public — link-based capability read (ADR-0007); unknown/unauthorized/expired ids return a uniform 404 |
| GET | `/v1/artifacts/{id}/body` | Download raw body bytes (re-verifiable against the stored SHA-256) | Public — same link-capability justification |
| GET | `/v1/artifacts/{id}/members/{name}` | Read a bundle member `<bundle_id>/<name>` (web tabs / MCP) | Public — same link-capability justification |
| DELETE | `/v1/artifacts/{id}` | Delete an artifact (owner only; also honors expiry) | Required — owner only |
| POST | `/v1/artifacts/{id}/share` | Set/adjust link access policy or rotate the id (ADR-0007) | Required — owner only; no `sharing:manage` for agents |
| GET | `/v1/bin` | List the Bin, keyset-paginated over `(created_at, id)`, optionally narrowed to artifacts carrying every `?tag=` | Required — workspace-scoped |
