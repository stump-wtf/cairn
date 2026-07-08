---
status: draft
date: 2026-07-08
implements: [ADR-0006]
requires: [SPEC-0002]
---

# SPEC-0006: Annotations — Reactions & Comments

## Overview

Every Cairn share type carries the *same* two social affordances: emoji **reactions**
and threaded **comments**. This capability formalizes the unified annotation
subsystem that serves all share types over one reactions path and one comments path,
across the web, CLI, and MCP surfaces. It realizes **ADR-0006** (the polymorphic
`{artifact_id, anchor_type, anchor_ref}` anchor model, idempotent reactions, shallow
threaded comments, registry-gated anchor capabilities, and two-tier count
aggregation) and depends on **SPEC-0002** for the artifact core, the share-type
registry that owns each type's anchor capability sets, and content-addressed immutable
bodies.

Annotations attach to type-specific **anchors** whose legal set is declared per share
type by the ADR-0002 registry: a markdown block or bullet, a code line or selection,
an image region pin, a webhook request, a trajectory turn / tool-call / span, or the
whole artifact. A deliberate asymmetry is enforced structurally: **webhook requests
are reactable but not comment-threaded**. Counts roll up into the Bin and artifact
headers. The REST API is **auth-by-default**: posting a reaction or comment requires
authentication, while reading annotations follows the annotated artifact's ADR-0007
link-capability.

The per-viewer anchor *affordances* (how a viewer surfaces and locates an anchor) live
in the viewer specs (SPEC-0003, SPEC-0004, SPEC-0005). This spec owns the anchor
*model*, validation, persistence, idempotency, threading, aggregation, and the API.

## Requirements

### Requirement: Polymorphic Anchor Model

Reactions and comments MUST share one embedded anchor of three fields:
`artifact_id` (the annotated artifact), `anchor_type` (a registry-owned discriminator
string), and `anchor_ref` (a JSONB locator whose schema is defined per `anchor_type`).
The service MUST compute a canonical `anchor_key` — the sorted-key, whitespace-free
serialization of `anchor_ref` — and use it for uniqueness and indexing so ordering of
JSON fields never affects identity. Every annotation query MUST be scoped to a single
`artifact_id`.

#### Scenario: Anchor persisted with canonical key

- **WHEN** an annotation is created with a given `anchor_ref`
- **THEN** the service MUST store `anchor_type`, `anchor_ref`, and a canonical
  `anchor_key`, and two `anchor_ref`s differing only in JSON key order MUST produce the
  same `anchor_key`

#### Scenario: Whole-artifact anchor

- **WHEN** an annotation targets the whole artifact
- **THEN** its `anchor_type` MUST be `artifact` and its `anchor_ref` MUST be `{}`

### Requirement: Registry-Gated Anchor Capabilities

Each share type's ADR-0002 registry entry MUST declare two capability sets: which
`anchor_type` values accept **reactions** and which accept **comments**. On every
write the service MUST reject a reaction or comment whose `anchor_type` is not in the
corresponding capability set for the artifact's share type, and MUST reject an
`anchor_ref` that fails that `anchor_type`'s locator schema. Adding a share type
(SPEC-0003 registration) MUST define its full annotation surface as registry data with
no schema change to this subsystem.

#### Scenario: Illegal anchor type for a type

- **WHEN** a client comments with an `anchor_type` outside the share type's comment
  capability set
- **THEN** the server MUST reject the write with `validation_failed` and persist nothing

#### Scenario: Malformed locator

- **WHEN** an `anchor_ref` does not match its `anchor_type`'s locator schema
- **THEN** the server MUST reject the write with `validation_failed`

### Requirement: Webhook Reaction-Only Asymmetry

The webhook share type's registry entry MUST accept reactions on `artifact` and
`webhook_request` anchors and MUST accept comments on **no** anchor type. The service
MUST therefore reject any comment on any webhook artifact while accepting reactions on
a `webhook_request`. This rule MUST be enforced as registry data, not special-cased
code.

#### Scenario: Comment on a webhook request refused

- **WHEN** a client posts a comment anchored to a `webhook_request`
- **THEN** the server MUST reject it with `validation_failed` and persist nothing

#### Scenario: Reaction on a webhook request accepted

- **WHEN** a client reacts to a `webhook_request`
- **THEN** the server MUST accept and persist the reaction

### Requirement: Idempotent Reactions

A reaction MUST be uniquely identified by `(artifact_id, anchor_type, anchor_key,
emoji, actor_id)`. Reacting with the same emoji to the same anchor twice by the same
actor MUST be a no-op (a single stored row), and un-reacting MUST delete exactly that
row. The `emoji` MUST be a single Unicode grapheme.

#### Scenario: Duplicate reaction

- **WHEN** an actor reacts 🔥 to an anchor they have already reacted 🔥 to
- **THEN** the result MUST remain a single reaction row (upsert no-op)

#### Scenario: Un-react

- **WHEN** an actor removes their 🔥 reaction from an anchor
- **THEN** exactly that reaction row MUST be deleted and others MUST be unaffected

### Requirement: Threaded Comments

Comments MUST support one level of reply nesting via a nullable `parent_id` (a thread
root has `parent_id = NULL`; a reply references its root). A reply's anchor MUST match
its root's anchor. Deletion MUST be a **soft delete** (`deleted_at`) that preserves
thread structure; edits MUST record `edited_at`. Deeper-than-one-level nesting MUST NOT
be created.

#### Scenario: Reply to a comment

- **WHEN** a client replies to a root comment
- **THEN** the reply MUST reference that root via `parent_id` and share its anchor

#### Scenario: Soft-deleted comment keeps the thread

- **WHEN** a root comment with replies is deleted
- **THEN** it MUST be soft-deleted (tombstoned) and its replies MUST remain resolvable

### Requirement: Count Aggregation

The subsystem MUST maintain two tiers of counts. (1) **Artifact-level rollups** —
`comment_count`, `reaction_count`, and `pin_count` denormalized on the artifact and
updated in the **same transaction** as the annotation insert/soft-delete — so the Bin
row (`💬 2 · 👀 3`) and artifact headers (`6 comments · 16 reactions`, `2 pins · 8
reactions`) render with no per-row subquery. (2) **Per-anchor tallies at view time** —
emoji tallies, per-anchor grouping, and the viewer's "did I already react" flag —
computed with a `GROUP BY (anchor_key, emoji)` scoped to the one opened artifact.

#### Scenario: Bin count without subquery

- **WHEN** the Bin lists artifacts
- **THEN** each row's reaction/comment counts MUST read from the artifact's
  denormalized counters, not a per-row scan of the annotation tables

#### Scenario: Per-anchor emoji tally

- **WHEN** a single artifact is opened
- **THEN** its per-anchor emoji tallies (e.g. `🔥 7 · 🙏 3`) and the current actor's
  "already reacted" flags MUST be computed over that one artifact's reactions

### Requirement: Anchor Stability

Anchors MUST resolve reliably because their substrate is immutable: artifact bodies are
content-addressed (ADR-0008) and editing mints a new artifact, never mutates one.
Render identifiers MUST be deterministic — markdown `block_id`s hashed from structural
position + content, code `line` numbers intrinsic to the body (optionally with a line-
text hash), image regions in normalized fractional coordinates, and webhook
`request_id`s / trajectory `span_id`s assigned once at capture. A `text_selection`
anchor MUST store character offsets **and** the quoted substring so it can be
re-highlighted and flagged "context changed" if the quote no longer matches.

#### Scenario: Anchor resolves on re-render

- **WHEN** an anchored annotation's artifact is re-rendered on any surface
- **THEN** the anchor MUST resolve to the same location it was created against

#### Scenario: Text selection quote mismatch

- **WHEN** a `text_selection` anchor's stored quote no longer matches the body at its
  offsets
- **THEN** the annotation MUST be surfaced as "context changed" rather than mis-anchored

### Requirement: Cross-Surface Parity

Reactions and comments MUST behave identically whether created over the REST API, the
CLI, or MCP (ADR-0003); all three surfaces MUST be thin adapters over the same core
service methods. An annotation created on one surface MUST render on the others against
the same anchor.

#### Scenario: MCP reaction visible on the web

- **WHEN** an agent reacts over MCP and a human comments over REST against the same
  anchor
- **THEN** both MUST render identically in the web viewer against that anchor

### Requirement: Error Handling Standards

Domain failures MUST use sentinel errors that callers distinguish (e.g. anchor-not-
allowed, locator-invalid, comment-on-non-commentable-type, parent-not-found), and the
transport adapters MUST map each to a stable error `code` (ADR-0012:
`validation_failed`, `not_found`, `forbidden`, `conflict`, …) without string matching.
Errors MUST be wrapped with context at each layer boundary (preserving the chain), MUST
NOT be silently swallowed, and MUST be logged with structured key-value fields
including `request_id`, `artifact_id`, and `anchor_type`.

#### Scenario: Domain error mapped to a stable code

- **WHEN** an annotation write fails registry validation
- **THEN** the service MUST return a distinguishable sentinel error that the adapter
  maps to `validation_failed` in the structured error envelope with a `request_id`

#### Scenario: No silent swallow

- **WHEN** a lower-layer error occurs during an annotation write
- **THEN** it MUST be wrapped with context and surfaced, not discarded

### Requirement: Database Operation Standards

A mutation that both writes an annotation and updates the artifact's denormalized
counters MUST occur in a **single transaction** so a count can never diverge from its
rows on the committed path. All SQL MUST use **parameterized queries** (bound
parameters); no query SHALL be assembled by string concatenation of caller input.
Database calls MUST honor the request `context.Context` deadline/cancellation and use
explicit connection lifecycle with timeouts.

#### Scenario: Atomic write + counter update

- **WHEN** a comment is inserted
- **THEN** the insert and the artifact `comment_count` increment MUST commit in one
  transaction, or neither is applied

#### Scenario: Parameterized query only

- **WHEN** any annotation query includes caller-supplied values (emoji, body,
  `anchor_ref`)
- **THEN** they MUST be passed as bound parameters, never string-concatenated into SQL

## Security Requirements

This capability is web-facing. The following are MANDATORY.

### Requirement: Authentication & Authorization

Posting a reaction or comment (and un-reacting, editing, deleting) MUST require
authentication (session for web, OAuth 2.1 bearer for API/MCP/CLI per ADR-0004).
Reading annotations MUST enforce the annotated artifact's ADR-0007 link-capability:
only a client that can read the artifact MAY read its annotations. Editing or deleting
a comment MUST be limited to its author (or an authorized workspace role). Agents MUST
NOT exceed the human's permissions.

#### Scenario: Unauthenticated post

- **WHEN** an unauthenticated client posts a reaction or comment
- **THEN** the server MUST respond 401 and persist nothing

#### Scenario: Read without artifact capability

- **WHEN** a client without the artifact's link capability requests its annotations
- **THEN** the server MUST deny the read per the access policy and return no annotations

### Requirement: Rate Limiting

All annotation endpoints MUST be rate-limited per-identity/per-IP; exceeding the limit
MUST return 429 with `Retry-After`. Reaction toggling and comment posting MUST be
throttled to resist spam/flooding.

#### Scenario: Reaction flood

- **WHEN** a client exceeds the configured reaction/comment rate
- **THEN** the server MUST respond 429 with `Retry-After` without processing the request

### Requirement: Security Headers

Annotation responses (including HTMX comment fragments) MUST set a strict
`Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, and
(over HTTPS) HSTS. User-supplied comment bodies MUST be sanitized/escaped on render so
they cannot execute in Cairn's app origin.

#### Scenario: Comment body with active content

- **WHEN** a comment body contains script or HTML
- **THEN** it MUST be sanitized/escaped so it cannot run when rendered in the viewer

### Requirement: Request Body Size Limits

Every annotation endpoint MUST enforce a maximum request size — bounding comment body
length and rejecting oversize payloads with 413 before buffering the full body.

#### Scenario: Oversize comment

- **WHEN** a comment body exceeds the configured maximum
- **THEN** the server MUST reject it with 413 and persist nothing

### Requirement: CSRF Protection

Cookie/session-authenticated annotation writes MUST be CSRF-protected (token or
SameSite strategy). Token-authenticated API/MCP requests are exempt (no ambient
credentials).

#### Scenario: Cross-site comment post

- **WHEN** a session-authenticated comment post arrives without a valid CSRF token
- **THEN** the server MUST reject it

### Requirement: Redirect & SSRF Validation

Any redirect issued after an annotation action MUST target only an allow-listed
internal path; no user-supplied absolute URL SHALL be honored. No annotation input
(comment body, `anchor_ref`) SHALL trigger a server-side fetch of a user-supplied URL;
if one is ever added it MUST be SSRF-guarded.

#### Scenario: Open-redirect after posting

- **WHEN** an annotation request supplies an external redirect target
- **THEN** the server MUST ignore it and redirect only to a safe internal path

## Accessibility Requirements

This capability renders user-facing UI. WCAG 2.1 AA is the minimum target.

### Requirement: WCAG 2.1 AA & Semantics

Reaction clusters and comment threads MUST meet WCAG 2.1 AA. Reactions MUST NOT be
conveyed by color/emoji alone — each cluster MUST expose an accessible name and count
(e.g. "🔥 fire, 7 reactions"). Comment threads MUST use correct list/heading semantics
and associate each comment with its author and timestamp.

#### Scenario: Reaction not color-only

- **WHEN** a reaction cluster is shown
- **THEN** it MUST expose a text label and count to assistive technology, not rely on
  the emoji glyph alone

### Requirement: Icon-Only Controls

Every icon-only annotation control — the react `＋` picker trigger, an emoji button, a
reply/edit/delete affordance, an image pin — MUST have an `aria-label` describing its
action.

#### Scenario: React plus button

- **WHEN** the react `＋` control has no visible text label
- **THEN** it MUST expose an `aria-label` describing its action

### Requirement: Dynamic Content Regions

Newly posted comments and reaction updates (HTMX swaps) MUST land in an `aria-live`
region (`polite`) so assistive technology announces them without stealing focus.

#### Scenario: New comment announced

- **WHEN** a comment is posted and swapped into the thread
- **THEN** it MUST be announced via an `aria-live` region

### Requirement: Keyboard Navigation & Focus Management

All annotation controls MUST be keyboard-operable with a logical tab order: Enter/Space
activate, Escape dismisses the picker/popover, arrow keys move within the emoji picker.
The reaction picker and comment popover MUST trap focus while open and restore focus to
the trigger on close.

#### Scenario: Keyboard-only reaction

- **WHEN** a keyboard user opens the reaction picker
- **THEN** focus MUST move into it, cycle within it, and return to the trigger on close

## HTTP Endpoints

REST/JSON under `/v1` (ADR-0012), all backed by the one core service. Auth-by-default:
reads are `Auth: Public` only because they are gated by the annotated artifact's
ADR-0007 **link capability**; every mutation is `Auth: Required`.

| Method & Path | Purpose | Auth |
|---------------|---------|------|
| `GET /v1/artifacts/{id}/reactions` | List reactions (grouped tallies + "did I react") | Public — gated by artifact link capability (ADR-0007) |
| `POST /v1/artifacts/{id}/reactions` | React (idempotent; registry-gated anchor) | Required |
| `DELETE /v1/artifacts/{id}/reactions/{rid}` | Un-react (delete own reaction) | Required |
| `GET /v1/artifacts/{id}/comments` | List comment threads for the artifact | Public — gated by artifact link capability (ADR-0007) |
| `POST /v1/artifacts/{id}/comments` | Comment (registry-gated anchor; refused on non-commentable types) | Required |

The MCP tools (ADR-0004) and the HTMX handlers (ADR-0011) are thin adapters over the
same core methods these routes call, mechanically guaranteeing cross-surface parity.
