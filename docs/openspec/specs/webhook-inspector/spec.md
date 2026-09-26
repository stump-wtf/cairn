---
status: draft
date: 2026-07-08
implements: [ADR-0010]
requires: [SPEC-0002]
---

# SPEC-0005: Webhook Inspector

## Overview

The **webhook** share type (badge `HK`, URL `cairn.stump.wtf/<id>`) is a **live** requestbin: an
endpoint an agent or a service points at, whose inbound HTTP requests stream into a web
inspector (highlighted JSON, method/status, a status mix) and are readable by agents over
MCP as the *same* stream (`mcp://cairn/hook/<id>`). Unlike every other share type its body
is not pushed by its owner — it is filled by **unauthenticated third parties hitting an open
ingress URL**. Captured requests land in a capped, `seq`-ordered ring buffer: metadata in
PostgreSQL, bodies in object storage. One capture log fans out to browsers over SSE and to
agents over MCP. Requests are **reactable but NOT comment-threaded** — discussion moves to
the artifacts they produce.

This spec realizes **ADR-0010** (live webhook endpoints and real-time stream capture). It
builds on **SPEC-0002** for the artifact envelope, provenance, access, and expiry; stores
request bodies in **ADR-0008** object storage; constrains annotations per **ADR-0006 /
SPEC-0006** (reactions only, no threads); and delivers live over the SSE transport of
**ADR-0012**.

> **Security note.** The ingress endpoint is an anonymous-write, internet-facing attack
> surface. Its Security Requirements below are CRITICAL, not routine: no code execution ever,
> strict size limits, rate limiting, an unguessable id, and retention caps are load-bearing
> parts of the design.

### HTTP endpoints

`Auth` values: `Required` = authenticated owner/actor (session for web, OAuth 2.1 bearer for
API/MCP/CLI per ADR-0004); `Link-cap` = the ADR-0007 read capability (possession of the
unguessable `cairn.stump.wtf/<id>` link grants read, no reader account); `Public` = open ingress,
justified per-row.

| Method & Path | Purpose | Auth |
|---|---|---|
| `POST /v1/hooks` | Create a webhook endpoint (owner); returns id, ingress URL, `mcp://cairn/hook/<id>` | Required |
| `GET /v1/hooks/{id}` | Endpoint metadata + recent request buffer | Link-cap |
| `GET /v1/hooks/{id}/requests` | List captured requests (keyset paginated, `seq` order) | Link-cap |
| `GET /v1/hooks/{id}/requests/{seq}` | Full detail of one captured request (metadata + body ref) | Link-cap |
| `GET /v1/hooks/{id}/requests/{seq}/body` | Raw captured body bytes (lazy, checksum-verifiable) | Link-cap |
| `GET /v1/hooks/{id}/stream` | SSE: live captured requests (ADR-0010/ADR-0012) | Link-cap |
| `ANY /h/{id}` (also `hook.cairn.stump.wtf/{id}`) | **Ingress**: capture an inbound request of any method | **Public** |

The ingress row is the single `Public` endpoint in Cairn. Justification: a requestbin's
entire purpose is to receive requests from anonymous third parties; it is defended not by
authentication but by an unguessable id, strict rate/size limits, header hygiene, no payload
execution, and retention caps (see Security Requirements). Reactions on a captured request
are served by the generic annotation endpoint of SPEC-0002/SPEC-0006
(`/v1/artifacts/{id}/reactions`); comments are structurally rejected (below).

## Requirements

### Requirement: Webhook Endpoint and Two Addresses

A webhook endpoint MUST be created by its owner over the web, CLI, or MCP and MUST be an
ordinary Cairn artifact (share type `webhook`) with provenance, a link-based access policy,
and a default TTL (ADR-0007). It MUST expose two addresses for the *same* endpoint: a public
**HTTP ingress** URL (`hook.cairn.stump.wtf/<id>` or `cairn.stump.wtf/h/<id>`) that accepts inbound
requests of any method, and an **`mcp://cairn/hook/<id>`** handle agents use to read the
captured stream. The `<id>` MUST be the short, opaque, unguessable base62 id of ADR-0005.

#### Scenario: Endpoint exposes both addresses

- **WHEN** an owner creates a webhook endpoint
- **THEN** the response MUST include the human `cairn.stump.wtf/<id>` URL, the HTTP ingress URL, and the `mcp://cairn/hook/<id>` handle for the same endpoint

#### Scenario: Expired endpoint stops capturing

- **WHEN** an endpoint passes its `expires_at`
- **THEN** the ingress MUST stop accepting requests and the captured stream, records, and bodies MUST be dropped with the artifact

### Requirement: Request Capture and Storage

Each accepted inbound request MUST become one **request record** with metadata in
PostgreSQL — `webhook_id`, a monotonic `seq` (stream order), `received_at`, `method`,
`path`, `query`, selected/normalized `headers`, the fixed `status` Cairn returned,
`content_type`, and `body_size` — and its raw body written to ADR-0008 object storage,
content-addressed by SHA-256 and referenced from the record by a `body_ref` (hash + size).
Bodies MUST be stored **verbatim** (no transformation on ingest; syntax highlighting is a
render-time concern) and MUST dedup by hash. Bodies MUST be fetched lazily when a request is
expanded.

#### Scenario: Metadata queryable without reading the body

- **WHEN** the inspector computes the status mix and method counts
- **THEN** it MUST read only PostgreSQL request rows, never the object-storage bodies

#### Scenario: Body stored verbatim and content-addressed

- **WHEN** a JSON payload is captured
- **THEN** the bytes MUST be stored unchanged, referenced by SHA-256 `body_ref`, and an identical body MUST reference the same blob

### Requirement: Fixed Benign Response

The ingress MUST always return a fixed, benign response (a `200` with a short
acknowledgment by default). The response Cairn returns MUST be **data it records**, never
behavior the payload can steer. Cairn MUST NOT evaluate, deserialize-to-execute, follow, or
otherwise act on the payload.

#### Scenario: Response is not payload-controlled

- **WHEN** a payload attempts to specify a response status or redirect
- **THEN** the ingress MUST ignore it and return the fixed configured response, recording that status on the record

### Requirement: Ring-Buffer Retention and Caps

A webhook endpoint MUST be a bounded live buffer enforced two ways: a **per-endpoint request
cap** (a ring buffer of at most the last N requests, e.g. N = 500) whose overflow evicts the
oldest record and dereferences its body blob, and the **endpoint TTL** (ADR-0007) that
bounds lifetime. On overflow the oldest record MUST be evicted so an anonymous flood can at
worst churn one endpoint's buffer, never exhaust global storage.

#### Scenario: Overflow evicts the oldest

- **WHEN** an endpoint at its N-request cap captures request N+1
- **THEN** the oldest record MUST be evicted and its body blob dereferenced (subject to ADR-0008 refcounted GC)

#### Scenario: Bounded footprint under flood

- **WHEN** one endpoint receives far more than N requests
- **THEN** its retained record and body footprint MUST stay bounded by N

### Requirement: One Stream, Two Transports (Live Fan-out)

Capture MUST write the record, then fan the new `seq` out to live subscribers off **one**
`seq`-ordered log: to browsers over **SSE** (`GET /v1/hooks/{id}/stream`, ADR-0012) and to
agents over **MCP** (`mcp://cairn/hook/<id>`), yielding the identical ordered records. A new
subscriber MUST first receive the recent buffer, then subscribe to tail updates, so a freshly
opened inspector shows history and live arrivals. SSE reconnection MUST resume from
`Last-Event-ID`.

#### Scenario: Human and agent see the same order

- **WHEN** a browser (SSE) and an agent (MCP) watch the same endpoint while requests arrive
- **THEN** both MUST observe the identical `seq`-ordered stream

#### Scenario: Late joiner sees history then tail

- **WHEN** an inspector opens on an endpoint that already captured requests
- **THEN** it MUST load the recent buffer and then receive subsequent captures live

### Requirement: Inspector Viewer

The web inspector MUST render a **live request list** (newest prepended), a **full request
inspect** view (method, path, the fixed response status, headers, and the body with
render-time JSON syntax highlighting), and a **status mix** summarizing the response-status
distribution across the buffer. Captured payloads MUST be displayed as inert escaped text
(see Security). Large bodies MUST load lazily on expand.

#### Scenario: Inspect a captured request

- **WHEN** a user opens a captured request
- **THEN** the inspector MUST show its method, status, headers, and highlighted body, loading the body lazily

#### Scenario: Status mix reflects the buffer

- **WHEN** the buffer contains a distribution of response statuses
- **THEN** the status-mix summary MUST reflect that distribution, computed from request rows

### Requirement: Reactions-Only Annotation

A captured request MUST be **reactable but NOT comment-threaded**, enforced structurally by
the webhook registry entry (ADR-0006 / SPEC-0006): the reaction capability set is
`{ artifact, webhook_request }` and the comment capability set is empty. A reaction on a
`webhook_request` anchor MUST be accepted; any comment on any webhook anchor MUST be refused.
This is a deliberate steer — durable discussion happens on the artifacts a stream produces,
not on the live buffer.

#### Scenario: React on a single request

- **WHEN** an authenticated actor reacts 👀 to a captured request
- **THEN** the reaction MUST persist against a `webhook_request` anchor carrying that request's id

#### Scenario: Comment on a request refused

- **WHEN** any actor attempts to comment on a webhook request or the webhook artifact
- **THEN** the server MUST refuse it (the comment capability set is empty), exposing no comment-thread affordance

### Requirement: v1 Non-Goals Are Absent

The v1 inspector MUST NOT implement a first-request empty-state affordance or stream
filtering by status/event. These "try next" features MUST be additive read-side features and
MUST NOT change the capture model.

#### Scenario: No stream filtering in v1

- **WHEN** the inspector is inspected for filter controls
- **THEN** it MUST NOT offer status/event filtering of the stream in v1

### Requirement: Error Handling Standards

Every layer boundary (ingress/adapter → core service → PostgreSQL / object storage) MUST wrap
errors with context preserving the underlying error, so a handler maps a domain failure to a
stable error `code` (ADR-0012) without string-matching. Sentinel errors MUST be defined for
domain failures callers distinguish — endpoint-not-found, endpoint-expired, body-too-large,
and rate-limited. Errors MUST NOT be silently swallowed; every rejection (oversize, throttle,
expired) MUST be recorded with structured (key-value) logging carrying the `request_id`. A
capture failure MUST NOT leak an internal error or stack to the anonymous ingress caller.

#### Scenario: Ingress error is opaque to the caller

- **WHEN** capture fails internally (e.g. object-storage write error)
- **THEN** the ingress MUST return a benign fixed response, and the wrapped error MUST be logged with the `request_id`, never surfaced to the anonymous caller

#### Scenario: Distinct sentinel for oversize

- **WHEN** a body exceeds the hard size cap
- **THEN** the service MUST return a distinct body-too-large sentinel that maps to `payload_too_large`, not a generic error

### Requirement: Concurrency Safety

Every core service method MUST take a `context.Context` first argument, and cancellation and
timeout MUST propagate across all concurrent boundaries (ingress → capture → SSE fan-out). The
per-endpoint fan-out MUST have an explicit worker lifecycle (subscribe on connect, guaranteed
unsubscribe on disconnect or endpoint expiry) and MUST access shared subscriber state
race-safely. Concurrent captures on one endpoint MUST assign `seq` monotonically without gaps
or collisions, and ring-buffer eviction MUST be race-safe against concurrent captures and
reads. Race detection MUST run in CI.

#### Scenario: Concurrent captures keep seq monotonic

- **WHEN** many requests hit one endpoint concurrently
- **THEN** each captured record MUST receive a distinct, gap-free `seq`

#### Scenario: Disconnected inspector is reaped

- **WHEN** an inspector's SSE connection drops
- **THEN** its subscription MUST be torn down and its resources released via context cancellation

### Requirement: Database Operation Standards

Multi-step mutations — inserting a request record while evicting an overflow record and
adjusting counts — MUST execute in a single transaction so the buffer is never observed in a
partial state. Database access MUST use an explicit connection lifecycle with timeouts
propagated from the request context. All SQL MUST use bound parameters; no query may be
assembled by concatenating caller input (especially the anonymous ingress path, whose
method/path/headers are attacker-controlled).

#### Scenario: Capture-and-evict is atomic

- **WHEN** a capture at the cap inserts a new record and evicts the oldest
- **THEN** both changes MUST commit in one transaction so the buffer never exceeds N nor loses ordering

#### Scenario: Attacker-controlled fields are parameterized

- **WHEN** an inbound request's method, path, or headers are persisted
- **THEN** they MUST be bound as query parameters, never interpolated into SQL

## Security Requirements

This capability is web-facing **and exposes an open, anonymous-write ingress endpoint**. The
following are MANDATORY and CRITICAL.

### Requirement: No Payload Execution / Inert Capture

Cairn MUST NEVER execute, evaluate, deserialize-to-execute, shell out on, follow, or
otherwise *act on* a captured payload. The ingress MUST parse only enough to store and index
(content-type, size); the payload is stored bytes and indexed metadata, nothing more. The
inspector MUST render payloads as inert escaped text.

#### Scenario: Payload is never executed

- **WHEN** a request body contains code, a script, or a serialized object
- **THEN** Cairn MUST store it as inert bytes, index only its metadata, and never execute or deserialize-to-execute it

### Requirement: Authentication & Authorization

Endpoint creation and management (`POST /v1/hooks`, TTL/sharing changes) MUST require
authentication (session for web, OAuth 2.1 bearer for API/MCP/CLI per ADR-0004). Read
endpoints MUST enforce the ADR-0007 link-capability policy, returning a uniform 404 for
unknown, unauthorized, or expired ids alike. The ingress endpoint is intentionally
anonymous-write (justified above) but confers no read access and no ability to enumerate or
manage endpoints. Agents MUST NOT exceed their human's permissions and MUST NOT receive a
sharing-management scope.

#### Scenario: Unauthenticated management call

- **WHEN** an unauthenticated client calls `POST /v1/hooks` or changes an endpoint's TTL
- **THEN** the server MUST respond 401 and make no change

#### Scenario: Ingress grants no read

- **WHEN** an anonymous caller posts to the ingress
- **THEN** it MUST be able to submit a request but MUST NOT thereby read the captured stream or list any endpoint

### Requirement: Rate Limiting

The ingress endpoint MUST be rate-limited **per-endpoint and per-source-IP**; excess requests
MUST receive a `429` (with `Retry-After`) and MUST NOT be captured, protecting the ring
buffer and the fan-out. The read/management endpoints MUST be rate-limited per identity/IP as
well.

#### Scenario: Flood on the open ingress

- **WHEN** a source exceeds the configured ingress rate on an endpoint
- **THEN** the server MUST respond 429 and capture nothing from the throttled requests

### Requirement: Security Headers

Responses MUST set a strict Content-Security-Policy, `X-Content-Type-Options: nosniff`, a
`Referrer-Policy`, and (over HTTPS) HSTS. Captured request bodies and headers are untrusted
attacker-supplied content and MUST be served/rendered so they cannot execute in Cairn's
origin (inert, escaped, and served with a non-executable content-type / isolated context).

#### Scenario: Untrusted HTML/script body

- **WHEN** a captured body contains active content (script/HTML)
- **THEN** the inspector MUST render it as inert escaped text and the CSP MUST prevent it running in Cairn's origin

### Requirement: Request Body Size Limits

Every captured request MUST be bounded by a hard body-size cap (e.g. a few MB). A request
above the cap MUST be rejected with `413`, or truncated with a truncation flag set on the
record, **before** the full body is buffered — so one request cannot blow past object-storage
tiering or memory, and no partial blob is persisted on rejection.

#### Scenario: Oversize inbound body

- **WHEN** an inbound request exceeds the configured body-size cap
- **THEN** the ingress MUST reject it with 413 (or truncate and flag it) without buffering the whole body or persisting a partial blob

### Requirement: CSRF Protection

Session-authenticated state-changing requests (creating or managing an endpoint from the web
app) MUST be CSRF-protected via token or SameSite strategy. Token-authenticated API/MCP/CLI
management is exempt (no ambient cookie credential). The anonymous ingress is deliberately
cross-origin (it receives webhooks from anywhere) and MUST NOT rely on cookies or ambient
credentials, so it is not — and must never become — a CSRF-sensitive, cookie-authenticated
surface.

#### Scenario: Cross-site management post

- **WHEN** a state-changing management request arrives on a session-auth route without a valid CSRF token
- **THEN** the server MUST reject it

### Requirement: Redirect & SSRF Validation

Cairn MUST NOT follow or fetch any URL contained in a captured request (headers, body, or
query) — a captured payload is stored data, never a server-side fetch Cairn performs. Any
redirect in the web app MUST target an allow-listed internal path; no user- or
attacker-supplied absolute URL is honored for redirects.

#### Scenario: Captured URL is not fetched

- **WHEN** a captured body or header contains a URL
- **THEN** Cairn MUST store it as inert data and MUST NOT issue any server-side request to it

### Requirement: Unguessable ID & No Enumeration

Endpoint ids MUST be the short, opaque, unguessable base62 ids of ADR-0005 — unguessability
is the ingress endpoint's first line of defense. There MUST be no listing of endpoints one
does not own, and id resolution MUST return a uniform 404 for unknown ids so probing leaks
nothing.

#### Scenario: Enumeration attempt

- **WHEN** a client probes sequential or guessed ids
- **THEN** each unknown id MUST return a uniform 404 and no listing of others' endpoints MUST be available

### Requirement: Header Hygiene & Ephemerality as Containment

Sensitive and hop-by-hop headers MUST be normalized or dropped on capture, so a stored record
is a sanitized projection rather than a replayable credential dump. The endpoint's default TTL
(ADR-0007) MUST bound its lifetime so an abused endpoint self-heals on expiry, removing all
its records and bodies.

#### Scenario: Hop-by-hop headers dropped

- **WHEN** an inbound request carries hop-by-hop or sensitive headers
- **THEN** the stored record MUST normalize or drop them, not persist a replayable credential set

#### Scenario: Abused endpoint self-heals

- **WHEN** an endpoint is flooded and then reaches its TTL
- **THEN** it MUST stop capturing and all its records and bodies MUST be removed

## Accessibility Requirements

This capability renders user-facing UI. WCAG 2.1 AA is the minimum target.

### Requirement: WCAG 2.1 AA & Semantics

All inspector UI MUST meet WCAG 2.1 AA. Page structure MUST use ARIA landmarks (banner,
navigation, main, contentinfo). Request status and method MUST NOT be conveyed by color
alone: status badges (2xx/4xx/5xx) and method badges (GET/POST/…) and the status mix MUST
also carry a text or shape cue.

#### Scenario: Status is not color-only

- **WHEN** a request's response status is shown by badge color
- **THEN** an equivalent text/shape cue (e.g. the numeric status or a labeled shape) MUST also be present

### Requirement: Icon-Only Controls

Every icon-only control (copy link, `◆ mcp` affordance, Share, react `＋`, panel toggle,
request expand/collapse) MUST have an `aria-label` describing its action.

#### Scenario: Icon button

- **WHEN** the react `＋` control has no visible text label
- **THEN** it MUST expose an `aria-label` describing its action

### Requirement: Dynamic Content Regions

The live request list is an SSE/HTMX-updated region and MUST use `aria-live` (polite for
normal arrivals) so a newly captured request is announced to assistive technology.

#### Scenario: New streamed request

- **WHEN** a new webhook request streams into the inspector
- **THEN** it MUST be announced via an `aria-live` region

### Requirement: Keyboard Navigation & Focus Management

All interactive elements — request rows, the inspect/expand control, the reaction picker, the
share dialog, the collapsible panel — MUST be keyboard-operable (logical tab order;
Enter/Space activate; Escape dismisses popovers/dialogs; arrow keys within the request list).
The reaction picker and share dialog MUST trap focus while open and restore it to the trigger
on close.

#### Scenario: Keyboard-only reaction

- **WHEN** a keyboard user opens the reaction picker on a request
- **THEN** focus MUST move into it, cycle within it, and return to the trigger on close
