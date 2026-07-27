---
status: draft
date: 2026-07-08
implements: [ADR-0007, ADR-0005]
requires: [SPEC-0002]
---

# SPEC-0009: Provenance, Link-Based Access, and Retention

## Overview

This capability formalizes the three intertwined policies that govern every Cairn
artifact and annotation: **provenance**, **link-based access control**, and
**retention/expiry**. It realizes ADR-0007 (provenance, capability-URL access,
default expiry) and ADR-0005 (short opaque ids as the capability handle), and builds
on SPEC-0002 (artifact core and share types).

1. **Provenance** — every artifact and annotation records an immutable,
   server-derived `actor` (human email or model, e.g. `claude · sonnet-4.6`),
   `channel` (`via Web` / `via CLI` / `via MCP`), and `captured_at`. The channel is
   derived from the authenticated surface and is **un-spoofable** by clients.
2. **Link-based access** — the unguessable capability-URL (ADR-0005) *is* the read
   token: anyone with the link reads, with no reader account. Annotating requires an
   authenticated identity. Changing sharing/expiry is an explicit owner-only action.
   Rotating the id is the revoke-a-leaked-link primitive.
3. **Retention** — artifacts are ephemeral by default (`expires_at = created_at + 7
   days`), owner-adjustable, with a visible countdown. Expiry **hard-deletes** body
   and metadata via a reference-counted reaper (blobs are content-addressed and may
   be deduped, so a blob is removed only when its refcount hits zero), backstopped by
   an object-storage lifecycle rule set longer than the maximum TTL.

## Requirements

### Requirement: Immutable Server-Derived Provenance

Every artifact and every annotation MUST persist a provenance record at creation
containing `actor`, `channel`, and `captured_at`. The record MUST be
append-only/immutable: no API path may edit an existing provenance record; a
correction MUST be a new record, never a mutation. `captured_at` MUST be a server
timestamp assigned at creation, not a client-supplied value.

#### Scenario: Provenance persisted on write

- **WHEN** any artifact or annotation is created on any surface
- **THEN** the server MUST persist `actor` + `channel` + `captured_at` derived server-side

#### Scenario: Provenance edit rejected

- **WHEN** any request attempts to modify an existing provenance record
- **THEN** the server MUST reject it; provenance is immutable

### Requirement: Un-Spoofable Channel Derivation

`channel` MUST be derived from the authenticated surface the write arrived on (`via
Web` for a web session, `via CLI` for a CLI OAuth token, `via MCP` for an MCP agent
token), never taken from a client claim. If a client asserts a `channel` in the
request body, the server MUST ignore it and record the derived value.

#### Scenario: Client asserts a false channel

- **WHEN** a client authenticated over MCP submits a create request claiming `channel = via CLI`
- **THEN** the server MUST override it and record `via MCP`

#### Scenario: Surface determines channel

- **WHEN** the same create operation arrives over web, CLI, and MCP
- **THEN** the recorded channels MUST be `via Web`, `via CLI`, and `via MCP` respectively, each server-assigned

### Requirement: Actor Captures Human and On-Behalf-Of Model

The `actor` MUST be a typed principal — `human:<email>` or `model:<vendor>/<model>`
(rendered `claude · sonnet-4.6`). For an on-behalf-of action (an agent over MCP), the
record MUST carry **both** the acting model actor and the human principal from the
OAuth grant (ADR-0004): the human owns the artifact while the display foregrounds the
model.

#### Scenario: Agent-created artifact provenance

- **WHEN** an agent creates an artifact over MCP for `sam@stump.rocks`
- **THEN** provenance MUST record the model actor and the human principal, with the human as owner and `channel = via MCP`

#### Scenario: Human-created artifact provenance

- **WHEN** a human creates an artifact from the CLI
- **THEN** provenance MUST record `actor = human:<email>` and `channel = via CLI`

#### Scenario: The producing model is recorded on any share type

- **WHEN** an agent creates an artifact of any share type and reports the model producing it
- **THEN** provenance MUST record that model and every viewer MUST show it, not only the trajectory viewer

The model is the one provenance fact the server cannot derive: an MCP client's
`initialize` identifies the HARNESS (which becomes on-behalf-of) and nothing on the
wire names the model. It is therefore reported by the creator, and the create surfaces
MUST offer a way to report it. This is the sole exception to server-derived provenance
above, and it is deliberately narrow: an unreported model is recorded as empty and
rendered as nothing, never guessed at or defaulted.

#### Scenario: No model reported

- **WHEN** an artifact is created without a model — a human piping a file from the CLI, or an agent that does not know it
- **THEN** provenance MUST record an empty model and the viewer MUST omit the model row entirely rather than rendering it blank

### Requirement: Link-Based Capability Read

Possession of a valid public capability-URL (ADR-0005) MUST grant **read** with no
reader account. Read is the only thing a bare link grants. The reader MUST NOT be
required to authenticate to view an artifact, a bundle file, or a webhook/trajectory
stream they hold the link for.

#### Scenario: Anonymous read via link

- **WHEN** an unauthenticated client requests a valid, unexpired artifact id
- **THEN** the server MUST return the artifact without requiring a reader account

#### Scenario: Link grants read only

- **WHEN** a bare link-holder (no authenticated identity) attempts to comment or react
- **THEN** the server MUST refuse — a link grants read, not annotation

### Requirement: Uniform 404 / No Enumeration

Resolution MUST return a **uniform 404** for unknown, unauthorized, and expired ids
alike, so probing leaks no signal about existence. Ids MUST NOT be enumerable and no
response may reveal counts, ordering, or whether two artifacts share content.

#### Scenario: Unknown vs expired indistinguishable

- **WHEN** a client requests an id that never existed and, separately, one that has expired
- **THEN** both MUST return an identical 404 response

#### Scenario: Unauthorized resolution

- **WHEN** a client requests an id outside its reach
- **THEN** the server MUST return the same uniform 404, disclosing nothing

### Requirement: Authenticated-Only Annotation

Commenting and reacting MUST require an authenticated identity (web login, or a
CLI/MCP OAuth token), because every annotation carries a provenance `actor` that must
be real. Annotation MUST never be anonymous. Whether an annotator must belong to the
artifact's workspace or may be any authenticated Cairn user is a workspace policy
knob; the invariant is that annotation is never anonymous.

#### Scenario: Unauthenticated annotation

- **WHEN** an unauthenticated client posts a comment or reaction
- **THEN** the server MUST respond 401 and create no annotation

#### Scenario: Authenticated annotation records real actor

- **WHEN** an authenticated user posts a comment
- **THEN** the annotation MUST carry that user's real provenance actor and derived channel

### Requirement: Owner-Only Policy Changes

Changing an artifact's **sharing** or **TTL** MUST require authenticated ownership,
not mere possession of the link. Only the creating human principal (owner) may mutate
policy. New artifacts MUST default to `you + anyone with link`. Agents MUST NOT be
able to change sharing or expiry (they hold no such scope; SPEC-0007).

#### Scenario: Non-owner changes sharing

- **WHEN** a non-owner (including a link-holder or an agent) attempts to change sharing or TTL
- **THEN** the server MUST refuse and leave the policy unchanged

#### Scenario: Owner restricts to owner-only

- **WHEN** the owner explicitly sets an artifact to owner-only
- **THEN** the server MUST unshare it so the capability-URL alone no longer grants read

### Requirement: Id Rotation as Revoke-a-Leaked-Link

The owner MUST be able to **rotate** an artifact's public id, minting a new
capability-URL and invalidating the old one — the revoke-a-leaked-link primitive. A
retired id MUST NOT be reused within TTL-plus-grace (ADR-0005), so an old link can
never later resolve to a different, newer artifact.

#### Scenario: Rotate invalidates the old link

- **WHEN** the owner rotates the id
- **THEN** the old link MUST return the uniform 404 while the new link resolves to the same artifact

#### Scenario: Retired id not reused

- **WHEN** the generator mints a new id after a rotation or expiry
- **THEN** it MUST NOT reuse a retired id within its TTL-plus-grace window

### Requirement: Default 7-Day TTL, Owner-Adjustable, Visible Countdown

Artifacts MUST be ephemeral by default with `expires_at = created_at + 7 days`. The
owner MAY extend, shorten, or set no-expiry, subject to any workspace cap; changing
the TTL MUST be an explicit owner action. The remaining time MUST be surfaced as a
visible countdown (`⧗ expires 7d`, `expires in 5d`) in headers and CLI output.
Annotations and streams MUST expire with their parent artifact.

#### Scenario: Default expiry applied

- **WHEN** an artifact is created without an explicit TTL
- **THEN** `expires_at` MUST be set to 7 days from creation and the countdown MUST be shown

#### Scenario: Owner adjusts TTL

- **WHEN** the owner sets a new TTL within the workspace cap
- **THEN** the server MUST update `expires_at` and reflect the new countdown everywhere

### Requirement: Hard Delete on Expiry via Reference-Counted Reaper

An application-authoritative reaper job MUST be the source of truth for expiry. Past
`expires_at`, it MUST hard-delete the artifact's metadata (artifact rows,
annotations, provenance) and issue the blob delete. Because bodies are
content-addressed and may be deduplicated across artifacts (ADR-0008), blob deletion
MUST be **reference-counted**: the artifact and its annotations are removed
immediately, but the underlying blob MUST be garbage-collected only when no live
artifact still references its content hash. Deletion MUST be hard, not soft — a
deleted id thereafter returns the same uniform 404 as a never-existed id.

#### Scenario: Expired artifact reaped

- **WHEN** the reaper runs and finds an artifact past `expires_at`
- **THEN** it MUST delete the artifact, its annotations, and its provenance, and the id MUST thereafter return 404

#### Scenario: Shared blob survives refcount

- **WHEN** an expired artifact's body content hash is still referenced by another live artifact
- **THEN** the reaper MUST decrement the refcount and NOT delete the blob until the refcount reaches zero

### Requirement: Object-Storage Lifecycle Backstop

The object storage (S3/Garage/MinIO, ADR-0008) MUST have lifecycle rules that sweep
truly orphaned objects (blobs whose refcount reached zero but whose delete failed,
plus upload debris). The lifecycle TTL MUST be set **longer than the maximum artifact
TTL** so it can never race ahead of a live reference; it reaps only orphans, never
live bodies. Application-driven refcounted deletion MUST remain the primary path.

#### Scenario: Lifecycle configured longer than max TTL

- **WHEN** the object-storage lifecycle rule is configured
- **THEN** its TTL MUST exceed the maximum permitted artifact TTL so a live body is never reaped by the backstop

#### Scenario: Orphan swept

- **WHEN** a blob's application delete failed and it is now orphaned (no live references)
- **THEN** the lifecycle backstop MUST eventually remove it without affecting referenced bodies

### Requirement: Error Handling Standards

Errors across the access, provenance, and reaper paths MUST be wrapped with context
at each layer boundary. Domain failures callers distinguish (unknown/expired id,
not-owner, refcount conflict, blob-delete failure) MUST be represented as
sentinel/typed errors; failures MUST NOT be silently swallowed. A failed blob delete
MUST be logged (structured key-value) and left for the lifecycle backstop, never
masked as success.

#### Scenario: Blob delete failure is not swallowed

- **WHEN** the reaper's blob delete fails
- **THEN** the failure MUST be logged with context and the object left to the lifecycle backstop, and MUST NOT be reported as a successful deletion

#### Scenario: Distinct not-owner error

- **WHEN** a policy change is refused for non-ownership
- **THEN** the server MUST return a distinct not-owner error rather than a generic failure

### Requirement: Concurrency Safety (Expiry Reaper)

The reaper MUST have an explicit worker lifecycle (clean startup and graceful
shutdown) and MUST propagate context for cancellation/timeout across its concurrent
boundaries. Refcount reads and decrements plus blob deletes MUST be race-safe so
concurrent reapers, or a reaper racing a new artifact that references the same
content hash, cannot delete a blob that just gained a live reference. Race detection
MUST run in CI.

#### Scenario: Concurrent create races the reaper

- **WHEN** a new artifact begins referencing a content hash while the reaper is evaluating that same hash for deletion
- **THEN** the refcount operation MUST be race-safe so the blob is not deleted out from under the new live reference

#### Scenario: Graceful shutdown mid-sweep

- **WHEN** the reaper receives a shutdown signal mid-sweep
- **THEN** it MUST stop cleanly via context cancellation without leaving a half-applied delete (metadata removed but blob orphaned with no refcount decrement)

### Requirement: Database Operation Standards

Multi-step mutations — reaping an artifact (delete rows + decrement refcount +
conditionally delete blob), id rotation, and TTL/sharing changes — MUST execute in
transactions so state is never left half-applied. All database access MUST use
parameterized queries only (no string interpolation) and explicit connection
lifecycle with timeouts.

#### Scenario: Atomic reap transaction

- **WHEN** the reaper deletes an expired artifact
- **THEN** the row deletions and refcount decrement MUST occur in a single transaction so a crash cannot leave annotations orphaned or a refcount wrong

#### Scenario: Parameterized id resolution

- **WHEN** the server resolves a public id
- **THEN** it MUST use a parameterized query and never string-interpolate the id

## Endpoint Table

Auth-by-default: every endpoint is `Auth: Required` unless explicitly justified.
Capability-URL reads are deliberately `Public` because the unguessable link *is* the
read token (ADR-0007) — this is the core design decision this spec formalizes, and it
is backstopped by unguessable ids, uniform 404s, and rate limiting (Security
Requirements).

| Endpoint | Method | Purpose | Auth |
|----------|--------|---------|------|
| `/{id}` | GET | Resolve + read an artifact/bundle by capability-URL | **Public** — the capability-URL is the read token (ADR-0007); unguessable id + uniform 404 + rate limit |
| `/run/{id}` | GET | Read a trajectory by capability-URL | **Public** — same capability-URL rationale |
| `/{id}/annotations` | POST | Post a comment/reaction | Required — annotation is never anonymous |
| `/{id}/policy` | PATCH | Change sharing (e.g. owner-only) | Required — owner only |
| `/{id}/ttl` | PATCH | Change expiry / extend / no-expiry | Required — owner only |
| `/{id}/rotate` | POST | Rotate the id (revoke a leaked link) | Required — owner only |

## Security Requirements

This capability is web-facing (the sharing/access/resolution endpoints). The
following are MANDATORY.

### Requirement: Authentication & Authorization

Mutating and workspace-scoped endpoints MUST require authentication (session for web,
OAuth 2.1 bearer for CLI/MCP per ADR-0004). Link-capability reads MUST enforce the
ADR-0007 access policy: a bare link grants read only; annotation requires
authentication; sharing/TTL/rotate require authenticated ownership. Agents MUST NOT
exceed the human's permissions and MUST NOT change sharing/expiry or delete others'
artifacts.

#### Scenario: Unauthenticated mutation

- **WHEN** an unauthenticated client calls a mutating endpoint (annotate, policy, ttl, rotate)
- **THEN** the server MUST respond 401 and make no change

#### Scenario: Non-owner policy change

- **WHEN** an authenticated non-owner attempts to change sharing or TTL
- **THEN** the server MUST respond 403 and leave the policy unchanged

### Requirement: Rate Limiting

All public and ingress endpoints — especially capability-URL resolution (`GET /{id}`,
`GET /run/{id}`) — MUST be rate-limited per-identity/per-IP; exceeding a limit MUST
return 429 with Retry-After. Resolution rate limiting is a primary defense-in-depth
against id scanning (ADR-0005).

#### Scenario: Burst on id resolution

- **WHEN** a client exceeds the configured rate scanning ids on the resolution endpoint
- **THEN** the server MUST respond 429 with Retry-After without processing the request

### Requirement: Security Headers

Responses MUST set a strict Content-Security-Policy, X-Content-Type-Options: nosniff,
Referrer-Policy, and (over HTTPS) HSTS. User-supplied artifact bodies MUST be
served/rendered so they cannot execute in the app origin (delegated to the viewer
rules in SPEC-0003, but access responses here MUST NOT weaken those headers).

#### Scenario: Untrusted artifact body

- **WHEN** an artifact body contains active content (script/HTML) and is served via a resolution response
- **THEN** it MUST be sanitized or isolated so it cannot run in Cairn's origin

### Requirement: Request Body Size Limits

Every mutating endpoint MUST enforce a maximum request size; oversize requests MUST
be rejected with 413 before buffering the full body. Annotation and policy payloads
MUST have bounded sizes.

#### Scenario: Oversize annotation

- **WHEN** an annotation payload exceeds the configured limit
- **THEN** the server MUST reject it with 413 and persist nothing

### Requirement: CSRF Protection

Cookie/session-authenticated state-changing requests (web annotate, policy, ttl,
rotate) MUST be CSRF-protected (token or SameSite strategy). Token-authenticated
CLI/MCP requests are exempt (no ambient credentials).

#### Scenario: Cross-site policy post

- **WHEN** a state-changing request arrives without a valid CSRF token on a session-auth route
- **THEN** the server MUST reject it and change nothing

### Requirement: Redirect & SSRF Validation

Any redirect target (e.g. after a share/rotate action) MUST be validated against an
allow-list of internal paths; no user-supplied absolute URL is honored for redirects.
Any server-side fetch of user-supplied URLs MUST be guarded against SSRF.

#### Scenario: Open-redirect attempt

- **WHEN** a request supplies an external redirect target
- **THEN** the server MUST ignore it and redirect only to a safe internal path
