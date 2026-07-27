---
status: draft
date: 2026-07-08
implements: [ADR-0009]
requires: [SPEC-0002]
---

# SPEC-0004: Trajectory Share

## Overview

The **trajectory** share type (badge `TRJ`, URL `cairn.sh/run/<id>`) captures a whole
agent run and lets humans view it. A run is a human prompt followed by an ordered,
nested tree of reasoning turns, tool calls, and sub-agent excursions. Cairn stores the
run as an OTel-*inspired* span tree, derives its stats from that tree, renders it as a
waterfall pinned above a readable activity stream, and links the run to the artifacts it
produced.

This spec realizes **ADR-0009** (trajectory capture and the span model). It builds on
**SPEC-0002** (artifact core + share-type registry) for the artifact envelope,
provenance, access, and expiry; it uses **ADR-0008** object storage for oversized span
outputs and produced-artifact bodies; it anchors reactions and comments through the
unified annotation layer (**ADR-0006 / SPEC-0006**); and it serves live spans over the
SSE transport of **ADR-0012**. It is **OTel-inspired, not OTLP-compliant**: Cairn borrows
the ordered nested-span-tree-with-timing shape and leaves behind sampling, trace-context
propagation, and the OTLP wire protocol.

### HTTP endpoints

All routes are backed by the one core service package (ADR-0012). `Auth` values:
`Required` = authenticated owner/actor (session for web, OAuth 2.1 bearer for API/MCP/CLI
per ADR-0004); `Link-cap` = the ADR-0007 read capability (possession of the unguessable
`cairn.sh/run/<id>` link grants read, no reader account). No endpoint is `Public`.

| Method & Path | Purpose | Auth |
|---|---|---|
| `POST /v1/runs` | Ingest a run — a complete run (batch) or open a live run | Required |
| `POST /v1/runs/{id}/spans` | Append one or more spans to an open run (incremental) | Required |
| `POST /v1/runs/{id}/close` | Close an open run (stamp `ended_at`, freeze derived stats) | Required |
| `GET /v1/runs/{id}` | Fetch run metadata, span tree, and derived stats | Link-cap |
| `GET /v1/runs/{id}/spans/{span_id}/output` | Lazily fetch a span's large (`output_ref`) output | Link-cap |
| `GET /v1/runs/{id}/stream` | SSE: live span-append events for an open run (ADR-0009/ADR-0012) | Link-cap |

Reactions and comments on a trajectory are served by the generic annotation endpoints of
SPEC-0002/SPEC-0006 (`/v1/artifacts/{id}/reactions`, `/v1/artifacts/{id}/comments`), gated
by the trajectory registry entry (below).

## Requirements

### Requirement: Span Model and Ordered Tree

A trajectory MUST store its run as an ordered tree of spans in PostgreSQL keyed by
`(run_id, span_id)`. Each span MUST carry: a `span_id` stable for the life of the run; a
`parent_span_id` (null for top-level spans); a derived `depth` and a monotonic sibling
`seq`; a `category` string drawn from either recommended vocabulary — by operation kind,
`reason · exec · read · net · write · search · plan · tool · analyze · test · fix · fail · meta`,
or by workflow phase,
`research · implementation · review · testing · debug · build · docs · delivery · deploy · wait · prompt`
(any non-empty string of at most 64 characters is accepted; a value outside both
vocabularies MUST still render, in a color derived deterministically from the category
name so one agent's own vocabulary reads as a palette rather than a single flat color); a
human-readable `name`; a `start_offset_ms` relative to the run's `started_at` and a
`duration_ms`; an optional `tool` name (null on a `reason` span, permitted on any other
category — with an open set the server cannot know which of an agent's own categories are
tool-shaped);
optional structured `args`; and an `output` (inline or by reference, per the Span Output
Storage requirement). A **sub-agent** MUST be represented as an ordinary span whose
children are its own tool spans — no separate entity. `span_id` MUST NOT be reissued once
assigned, because it is the anchor target for ADR-0006 annotations.

#### Scenario: Nested sub-agent renders as a span group

- **WHEN** a run is ingested with an `advisory lookup` span whose children are `web_search`, `web_fetch`, and `summarize` spans
- **THEN** the tree MUST reconstruct with those three spans nested under `advisory lookup` at `depth + 1`, ordered by `seq`

#### Scenario: Empty or missing category rejected

- **WHEN** an ingested span declares a `category` that is empty, missing, or whitespace-only
- **THEN** the server MUST reject the ingest with `validation_failed` and persist no span from that payload

#### Scenario: Non-recommended category accepted

- **WHEN** an ingested span declares a `category` in neither recommended vocabulary (e.g. `investigation`, `yak-shaving`)
- **THEN** the server MUST accept the span and persist it; the waterfall MUST render it with a text label carrying the category name, and a color derived from that name

#### Scenario: Two unrecognised categories are told apart

- **WHEN** one run uses two distinct categories that are in neither recommended vocabulary
- **THEN** each MUST render in a color derived from its own name, and the two MUST be either clearly distinct or identical — never merely similar, which would read as a meaningful difference that is not there
- **AND** the same category MUST resolve to the same color on every render of every run, so a reader learns it once

#### Scenario: Category length bounded

- **WHEN** an ingested span declares a `category` longer than 64 characters
- **THEN** the server MUST reject the ingest with `validation_failed`, because the value is
  unbounded agent-supplied text persisted in a column rendered in every legend. Both the
  service and the schema MUST enforce the same ceiling, so neither accepts what the other
  refuses.

#### Scenario: Waterfall legend covers the categories a run used

- **WHEN** a run's spans use categories outside the recommended set
- **THEN** the waterfall's category legend MUST list every category the run actually used —
  recommended ones first in their fixed order, the rest in a deterministic order — and MUST
  NOT list recommended categories the run never used. The time-by-category breakdown MUST
  cover every category that accumulated a non-zero duration, in that same order; a category
  whose spans all measured 0ms is omitted there, because a 0% segment renders as nothing and
  would only make the percentages read as though they did not sum.

#### Scenario: Stable span identity

- **WHEN** a span is persisted and later re-read
- **THEN** its `span_id`, `parent_span_id`, `depth`, and `seq` MUST be unchanged, so an annotation anchored to it still resolves

### Requirement: Run Model and Lifecycle

A run MUST record the human `prompt`, an absolute `started_at`, an optional `ended_at`
(absent while live), a `status` of `open` or `closed`, and an agent-reported `token`
count. A run MUST begin `open` (when opened incrementally) or be created already `closed`
(batch ingest of a finished run). A `closed` run MUST be immutable except for its
annotation stream: no span may be added, edited, or removed after close.

#### Scenario: Batch run is created closed

- **WHEN** a complete run is ingested in one call
- **THEN** the run MUST be persisted with `status = closed` and a stamped `ended_at`

#### Scenario: Append to a closed run refused

- **WHEN** a client appends spans to a run whose `status` is `closed`
- **THEN** the server MUST respond `conflict` and add no span

### Requirement: Run Ingestion — Batch and Incremental

The server MUST accept a run over both REST (ADR-0012) and MCP (ADR-0004) in two shapes:
**batch** (`POST /v1/runs` with the prompt and full span tree; the server assigns the
public id, validates tree structure, spills oversized outputs, and closes
the run) and **incremental** (`POST /v1/runs` to open a run and immediately return its id
and `cairn.sh/run/<id>` URL; `POST /v1/runs/{id}/spans` to append; `POST
/v1/runs/{id}/close` to close). Appends MUST be additive only. A batch ingest and the
equivalent open→append→close sequence MUST converge to the identical final run.

#### Scenario: Open returns a shareable link immediately

- **WHEN** a client opens a run
- **THEN** the server MUST return the run id and `cairn.sh/run/<id>` URL before any span is appended, so the human can be handed a link to a live run

#### Scenario: Batch and incremental converge

- **WHEN** the same prompt and span sequence are ingested once as a batch and once as open→append→close
- **THEN** the two resulting runs MUST have identical span trees and identical derived stats

#### Scenario: Malformed tree rejected atomically

- **WHEN** an append references a `parent_span_id` absent from the run
- **THEN** the server MUST reject the append with `validation_failed` and persist none of its spans

### Requirement: Span Output Storage and Content Addressing

A span `output` at or below a fixed inline threshold (e.g. ≤ 16 KB) MUST be stored inline
on the span row. An `output` above the threshold MUST be written to ADR-0008 object
storage, content-addressed by SHA-256, and referenced from the span by an `output_ref`
(hash + size + optional truncation flag) rather than inlined in PostgreSQL. Identical
large outputs across spans or runs MUST deduplicate to one blob. Large outputs MUST be
fetched lazily (only when a span is expanded), via `GET
/v1/runs/{id}/spans/{span_id}/output`.

#### Scenario: Megabyte stdout is not inlined

- **WHEN** a `bash npm ls --all` span reports multi-megabyte stdout
- **THEN** the output MUST be stored as a content-addressed blob and the span row MUST hold only an `output_ref`, never the bytes

#### Scenario: Identical outputs dedup

- **WHEN** two spans report byte-identical large outputs
- **THEN** both `output_ref`s MUST point at a single stored blob (one copy of the bytes)

### Requirement: Derived Run Statistics

The RUN-panel figures — wall time, span count, tool-call count, token count, and the
time-by-category breakdown — MUST be **computed from the span rows plus the run's token
count**, not stored as independent authoritative fields. Wall time MUST be `ended_at −
started_at` for a closed run and `now − started_at` while open; span count and tool-call
count MUST be row counts (tool calls = spans with a non-null `tool`); time-by-category
MUST sum `duration_ms` per category. Aggregates MAY be cached for a closed run, but the
span rows MUST remain the source of truth.

#### Scenario: Live stats tick as spans append

- **WHEN** a span is appended to an open run
- **THEN** the run's derived span count, tool-call count, and time-by-category MUST reflect it without a separately stored counter update

#### Scenario: Stats cannot disagree with the waterfall

- **WHEN** the RUN panel and the waterfall are rendered for the same run
- **THEN** span count, tool-call count, and per-category durations MUST be identical because both read the same span rows

### Requirement: Produced-Artifact Link

A `write` span MAY declare that it **produced an artifact**. The produced thing MUST be an
ordinary Cairn artifact (its own id, its own body in object storage), and the relationship
MUST be a directed `produced` edge from the span (and thus the run) to that artifact,
stored as a row in PostgreSQL. The edge MUST be resolvable from both directions: the run's
`write` span links to the produced artifact, and the produced artifact's provenance
(ADR-0007) MAY name the run that made it. The edge MUST be created either by the agent
naming the artifact id in the `write` span at ingest or by pushing the artifact and the
run in the same MCP session and letting Cairn resolve the reference.

#### Scenario: Write span links to its markdown share

- **WHEN** a `write checkout-web-audit.md` span declares a produced artifact id
- **THEN** the stream MUST render that span as a link to the markdown share, and a `produced` edge MUST resolve from both the run and the artifact

### Requirement: Span Waterfall Viewer

The trajectory viewer MUST render an OTel-style span **waterfall** pinned at the top, with
spans nested by `depth`, ordered by `seq`, positioned by `start_offset_ms`/`duration_ms`
against a time ruler (`0s … <wall time>`), and colored by the `category` legend
(known categories use their designated accent color; unknown categories use a neutral
default).
Clicking a span MUST **jump to and expand** that span's event in the activity stream
below.

#### Scenario: Click a span to jump

- **WHEN** a user clicks a span in the waterfall
- **THEN** the activity stream MUST scroll to that span's event and expand it

#### Scenario: Deterministic layout

- **WHEN** the same run is rendered twice
- **THEN** the waterfall MUST place every span identically, using the persisted `depth`/`seq` rather than re-deriving the tree on each draw

### Requirement: Activity Stream Viewer

Below the waterfall the viewer MUST render the run as a readable timeline: the human
prompt ("started the run"), each reasoning turn, each tool call with expandable `args` and
`output`, the nested sub-agent as a grouped section, and the `write` span rendered as a
link to the artifact it produced. Large outputs MUST load lazily on expand.

#### Scenario: Expand a tool call

- **WHEN** a user expands a `bash npm ls --all` tool call
- **THEN** the stream MUST show its `args` and (lazily loading if referenced) its `output`, e.g. `exit 0`

#### Scenario: Span carrying args but no output

- **WHEN** a span was ingested with structured `args` and no `output`
- **THEN** expanding it MUST still show those `args` — a span's `args` are rendered whenever present, not only alongside an `output`
- **AND** an `args` value that is absent, null, or an empty object/array MUST render nothing at all, rather than an empty labelled block

#### Scenario: Span with nothing to reveal

- **WHEN** a span was ingested with no `args`, no `output`, no produced artifact, and no children
- **THEN** expanding it MUST show an explicit statement that no output was captured for it, naming `output` as the field to populate — never an empty expanded region, which reads as a broken viewer rather than as a thin capture

### Requirement: Trajectory Annotation Anchors

Reactions and comments on a trajectory MUST flow through the unified annotation layer
(ADR-0006 / SPEC-0006), gated by the trajectory registry entry. Reactions MUST be accepted
on `artifact`, `trajectory_turn`, and `trajectory_toolcall` anchors; comments MUST be
accepted on `artifact`, `trajectory_span`, and `text_selection` anchors. Anchors MUST
target stable `span_id`s so they resolve for the life of the run.

#### Scenario: React on a tool call

- **WHEN** an authenticated actor reacts 🔥 to a tool-call span
- **THEN** the reaction MUST persist against a `trajectory_toolcall` anchor carrying that `span_id`

#### Scenario: Comment on a span

- **WHEN** an authenticated actor comments on a reasoning span
- **THEN** the comment MUST persist against a `trajectory_span` anchor and render in the RUN panel

### Requirement: Live Span Stream Delivery

While a run is `open`, each appended span MUST be delivered near-real-time to connected
viewers over SSE (`GET /v1/runs/{id}/stream`, ADR-0012) and be readable by agents over MCP
off the same ordered log. A newly connected viewer MUST first receive the run's existing
spans, then subscribe to tail appends, so it shows history and live arrivals without
gaps or duplicates. SSE reconnection MUST resume from `Last-Event-ID`.

#### Scenario: Late joiner sees history then live

- **WHEN** a viewer opens an already-running trajectory
- **THEN** it MUST load the spans captured so far and then receive subsequent appends live

#### Scenario: Resume after a dropped connection

- **WHEN** an SSE client reconnects with a `Last-Event-ID`
- **THEN** the stream MUST resume after that span with no loss or duplication

### Requirement: v1 Non-Goals Are Absent From the Schema

The v1 schema MUST NOT model a per-span error/status field, a token-cost lane, or run-vs-run
diff state. A failed step MUST be captured as an ordinary span (there is no red/errored-run
rendering in v1). These "try next" features MUST be additive when introduced and MUST NOT
require rewriting the span model.

#### Scenario: No span-level error field

- **WHEN** the span schema is inspected
- **THEN** it MUST NOT contain an error/status column, so a failed run is not schema-distinguishable from a successful one in v1

### Requirement: Error Handling Standards

Every layer boundary (transport adapter → core service → PostgreSQL / object storage) MUST
wrap errors with context preserving the underlying error, so a handler can map a domain
failure to a stable error `code` (ADR-0012) without string-matching. Sentinel errors MUST
be defined for domain failures callers distinguish — run-not-found, run-closed
(append-after-close), unknown-parent-span, and empty-category. Errors MUST NOT be
silently swallowed, and every failure MUST be recorded with structured (key-value) logging
carrying the `request_id`.

#### Scenario: Append-after-close is a distinct sentinel

- **WHEN** a span append targets a closed run
- **THEN** the service MUST return a distinct sentinel that the adapter maps to `conflict`, not a generic error

#### Scenario: Error context is preserved

- **WHEN** an object-storage write for a large span output fails
- **THEN** the returned error MUST wrap the storage error with span/run context and be logged with the `request_id`, not swallowed

### Requirement: Concurrency Safety

Every core service method MUST take a `context.Context` first argument, and cancellation
and timeout MUST propagate across all concurrent boundaries (ingest handler → persistence
→ SSE fan-out), so a cancelled request or a hung SSE client releases its resources. The
SSE fan-out for a live run MUST have an explicit worker lifecycle (clean subscribe on
connect, guaranteed unsubscribe on disconnect or run close) and MUST access shared
subscriber state race-safely. Concurrent appends to the same open run MUST assign `seq`
without gaps or collisions. Race detection MUST run in CI.

#### Scenario: Disconnected SSE client is reaped

- **WHEN** a browser watching a live run disconnects
- **THEN** its subscription MUST be torn down and its goroutine/resources released via context cancellation

#### Scenario: Concurrent appends keep seq monotonic

- **WHEN** two appends to the same open run race
- **THEN** each span MUST receive a distinct, gap-free `seq` under its parent

### Requirement: Database Operation Standards

Multi-step mutations — creating a run with its spans, appending spans while updating any
cached aggregate, and creating a `produced` edge alongside a `write` span — MUST execute
in a single transaction so a partial run is never visible. Database access MUST use an
explicit connection lifecycle with timeouts propagated from the request context. All SQL
MUST use bound parameters; no query may be assembled by concatenating caller input.

#### Scenario: Partial run never persists

- **WHEN** persisting a batch run fails midway through inserting its spans
- **THEN** the transaction MUST roll back so no partial span tree is visible

#### Scenario: Parameterized queries only

- **WHEN** a run is queried by public id or a span by `span_id`
- **THEN** the query MUST use bound parameters, never string interpolation of caller input

## Security Requirements

This capability is web-facing. The following are MANDATORY.

### Requirement: Authentication & Authorization

Mutating endpoints (`POST /v1/runs`, `POST /v1/runs/{id}/spans`, `POST /v1/runs/{id}/close`)
MUST require authentication (session for web, OAuth 2.1 bearer for API/MCP/CLI per
ADR-0004). Read endpoints (`GET /v1/runs/{id}` and its sub-resources) MUST enforce the
ADR-0007 link-capability access policy, returning a uniform 404 for unknown, unauthorized,
or expired ids alike. Only the run's owning human principal may append to or close a run.
Agents acting on a human's behalf MUST NOT exceed that human's permissions.

#### Scenario: Unauthenticated mutation

- **WHEN** an unauthenticated client calls `POST /v1/runs/{id}/spans`
- **THEN** the server MUST respond 401 and make no change

#### Scenario: Non-owner cannot close a run

- **WHEN** an authenticated actor who is not the run owner calls `POST /v1/runs/{id}/close`
- **THEN** the server MUST respond 403 and leave the run open

### Requirement: Rate Limiting

The ingestion endpoints MUST be rate-limited per identity so a runaway agent cannot flood
Cairn with span appends. Exceeding the configured rate MUST return 429 with a `Retry-After`
header and MUST NOT process the request.

#### Scenario: Burst of appends

- **WHEN** a client exceeds the configured append rate on an open run
- **THEN** the server MUST respond 429 with `Retry-After` and persist no span from the throttled request

### Requirement: Security Headers

Responses MUST set a strict Content-Security-Policy, `X-Content-Type-Options: nosniff`, a
`Referrer-Policy`, and (over HTTPS) HSTS. Span `args`, `output`, tool names, and the human
prompt are untrusted agent-supplied content and MUST be rendered as inert, escaped text so
they cannot execute in Cairn's origin.

#### Scenario: Active content in a span output

- **WHEN** a span output contains HTML or script
- **THEN** the viewer MUST render it as escaped inert text, and the CSP MUST prevent it executing in Cairn's origin

### Requirement: Request Body Size Limits

Every ingestion endpoint MUST enforce a maximum request size, and individual span outputs
MUST be bounded (oversize outputs are truncated with a flag or spilled per the Span Output
Storage requirement). Oversize requests MUST be rejected with 413 before the full body is
buffered, and no partial run or blob may be persisted.

#### Scenario: Oversize run payload

- **WHEN** a batch run payload exceeds the configured request-size limit
- **THEN** the server MUST reject it with 413 and persist neither run rows nor any blob

### Requirement: CSRF Protection

Session-authenticated state-changing requests (a run opened or closed from the web app)
MUST be CSRF-protected via token or SameSite strategy. Token-authenticated API/MCP/CLI
ingestion is exempt, as it carries no ambient cookie credential.

#### Scenario: Cross-site run creation

- **WHEN** a state-changing run request arrives on a session-auth route without a valid CSRF token
- **THEN** the server MUST reject it

### Requirement: Redirect & SSRF Validation

Cairn MUST NOT follow, fetch, or otherwise act on any URL contained in a span's `args` or
`output` — a captured `web_fetch` target is stored data, never a server-side fetch Cairn
performs. Any redirect (e.g. after a web-app action) MUST target an allow-listed internal
path; no user-supplied absolute URL is honored for redirects.

#### Scenario: Captured URL is not fetched

- **WHEN** a span records a `web_fetch nvd.nist.gov` argument
- **THEN** Cairn MUST store it as inert data and MUST NOT issue any server-side request to it

## Accessibility Requirements

This capability renders user-facing UI. WCAG 2.1 AA is the minimum target.

### Requirement: WCAG 2.1 AA & Semantics

All trajectory UI MUST meet WCAG 2.1 AA. Page structure MUST use ARIA landmarks (banner,
navigation, main, contentinfo). Span categories MUST NOT be conveyed by color alone: the
waterfall's category legend and the time-by-category breakdown MUST also carry a text
label or shape cue.

#### Scenario: Category is not color-only

- **WHEN** a span's category is shown by its legend color
- **THEN** an equivalent text label (e.g. `reason`) MUST also be present on or adjacent to the span

### Requirement: Icon-Only Controls

Every icon-only control (copy link, `◆ mcp` affordance, Share, react `＋`, panel toggle,
span expand/collapse) MUST have an `aria-label` describing its action.

#### Scenario: Icon button

- **WHEN** the react `＋` control has no visible text label
- **THEN** it MUST expose an `aria-label` describing its action

### Requirement: Dynamic Content Regions

The live activity stream and waterfall are HTMX/SSE-updated regions and MUST use
`aria-live` (polite for normal span arrivals) so new spans are announced to assistive
technology.

#### Scenario: New streamed span

- **WHEN** a new span streams into a live run
- **THEN** it MUST be announced via an `aria-live` region

### Requirement: Keyboard Navigation & Focus Management

All interactive elements — waterfall spans, stream expanders, the reaction picker, the
share dialog, the collapsible panel — MUST be keyboard-operable (logical tab order;
Enter/Space activate; Escape dismisses popovers/dialogs; arrow keys within the waterfall).
The reaction picker and share dialog MUST trap focus while open and restore it to the
trigger on close.

#### Scenario: Keyboard jump from waterfall to stream

- **WHEN** a keyboard user activates a waterfall span with Enter
- **THEN** focus MUST move to that span's expanded event in the stream

#### Scenario: Keyboard-only reaction

- **WHEN** a keyboard user opens the reaction picker
- **THEN** focus MUST move into it, cycle within it, and return to the trigger on close
