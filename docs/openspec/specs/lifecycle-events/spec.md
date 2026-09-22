---
status: accepted
date: 2026-09-22
implements: [ADR-0022]
requires: [SPEC-0012, SPEC-0006]
related: [SPEC-0004, SPEC-0009, SPEC-0007]
---

# SPEC-0016: Annotation and Trace Lifecycle Events

## Graph Edges

- **Implements:** **ADR-0022**: annotation and trace lifecycle events with a
  server-derived actor kind.
- **Requires:** **SPEC-0012**: the outbound webhook envelope, signing and
  delivery this spec extends.
- **Requires:** **SPEC-0006**: the reactions and comments whose lifecycle is
  announced.
- **Related:** **SPEC-0004** (run lifecycle), **SPEC-0009** (actor provenance),
  **SPEC-0007** (MCP tool surface).

## Overview

Cairn announces `artifact.created` today (SPEC-0012) and nothing else. This
capability adds `comment.created`, `reaction.added`, `reaction.removed` and
`run.closed` on the same envelope, signer and queue. Every event also gains a
server-derived `actor_kind` (`human` or `agent`) and `auth` method.

A server-computed `approval` bit makes a human 👍 a fact that the human's own
agents cannot forge. It is backed by per-kind reaction storage, so an agent can
neither occupy nor withdraw a human's reaction. See ADR-0022.

Requirement IDs (`EV-n`) are stable. Plans and code cite them as
`SPEC-0016 EV-n`.

## Requirements

### Requirement: EV-1 Event Kind Registry

The system SHALL emit exactly these kinds, each named `<noun>.<past-tense verb>`:

- `artifact.created` (SPEC-0012, unchanged trigger);
- `comment.created`;
- `reaction.added`;
- `reaction.removed`;
- `run.closed`.

`comment.edited` and `comment.deleted` are RESERVED names. They MUST NOT be
emitted for any other meaning. The comment edit and delete routes (#158) SHALL
emit them when those routes exist. A new kind MUST be added to this registry
before it is emitted.

#### Scenario: Reserved kinds are not reused

- **WHEN** a contributor adds an event for a comment edit
- **THEN** it MUST use the kind `comment.edited` and follow EV-3's payload rules

#### Scenario: Unknown kind never emitted

- **WHEN** the emitter is asked to encode a kind absent from the registry
- **THEN** it MUST refuse to encode it, log an error, and deliver nothing

### Requirement: EV-2 Emission Points

Each event MUST be emitted only after its transaction commits, from the service
layer that owns the change, so every surface (REST, web, CLI, MCP) triggers it
through one choke point:

- `comment.created` when `AddComment` commits;
- `reaction.added` when `React` inserts a row (an idempotent duplicate MUST NOT
  emit);
- `reaction.removed` when `Unreact` or `UnreactByID` deletes a row (removing an
  absent reaction MUST NOT emit);
- `run.closed` when `CloseRun` commits, and when a batch run is created already
  closed. The batch case is emitted after that run's `artifact.created`.

Emission MUST NOT block, fail or alter the response of the request that caused
it (SPEC-0012 REQ "Event Emission on Artifact Creation").

#### Scenario: MCP reaction emits

- **WHEN** an agent calls `artifact_react` and a new row is inserted
- **THEN** a `reaction.added` event is emitted for the subject artifact

#### Scenario: Duplicate reaction is silent

- **WHEN** a caller reacts with an emoji it already reacted with, under the same
  actor kind
- **THEN** the response is 200 with the existing row and no event is emitted

#### Scenario: Batch run emits closed after created

- **WHEN** a client POSTs a complete batch run
- **THEN** the event queue receives `artifact.created` and then `run.closed` for
  that run

#### Scenario: Rolled-back write emits nothing

- **WHEN** a comment insert fails after validation and its transaction rolls back
- **THEN** no `comment.created` event is emitted

### Requirement: EV-3 Payload Shape

Every event SHALL use the SPEC-0012 envelope `{source, kind, event_id,
created_at, data}` unchanged. Within `data`:

1. `id`, `url`, `share_type`, `title`, `tags`, `expires_at` MUST describe the
   **subject artifact**, with the meanings SPEC-0012 REQ "Event Payload" defines.
2. `actor_id`, `channel`, `on_behalf_of` MUST describe the principal that caused
   the event.
3. `actor_kind` and `auth` MUST be present on every kind (EV-4).
4. Kind-specific fields MUST nest under exactly one object:
   - `comment` carries `id`, `parent_id`, `anchor_type`, `anchor_key`, `body`
     and `body_truncated`;
   - `reaction` carries `id`, `anchor_type`, `anchor_key`, `emoji`,
     `approval_class` and `approval`;
   - `run` carries `status`, `span_count`, `started_at`, `ended_at` and
     `duration_ms`.
5. `comment.body` MUST be at most 4096 bytes, cut on a UTF-8 boundary, with
   `body_truncated: true` when it was cut.

New fields MUST be appended after existing ones, and a field that does not apply
MUST be omitted rather than emitted empty. The exception is `approval_class` and
`approval`, which are always present on reaction events, because `false` is
meaningful there.

#### Scenario: Subject fields resolve the artifact for every kind

- **WHEN** a consumer reads `data.id` and `data.url` from a `reaction.added` event
- **THEN** they identify the artifact reacted to, exactly as they would on its
  `artifact.created`

#### Scenario: Long comment is truncated in the event only

- **WHEN** a 10 KiB comment is created
- **THEN** the event carries the first 4096 bytes (at most) with
  `body_truncated: true`, and the stored comment is unchanged

#### Scenario: artifact.created gains only appended keys

- **WHEN** an untagged REST artifact is created after this change
- **THEN** its body equals the SPEC-0012 golden body with `actor_kind` and `auth`
  appended to `data`, and no other byte differs

### Requirement: EV-4 Server-Derived Actor Kind

`actor_kind` MUST be derived from how the principal authenticated, never from
request content:

- `human` if and only if the principal authenticated with an ambient browser
  session (`Principal.Ambient`: Pocket ID, GitHub, or the development login);
- `agent` for every bearer credential: MCP OAuth access tokens, personal access
  tokens regardless of their `is_agent` flag, and `CAIRN_API_TOKENS` entries.

`auth` MUST be one of `session`, `oauth`, `pat`, `api_token`. The development
bearer shortcut (`CAIRN_DEV_INSECURE_BEARER_AUTH`) MUST report `api_token`. No
request header, body field, tool argument or `on_behalf_of` value may change
either field.

#### Scenario: Human PAT is an agent

- **WHEN** a human reacts from the CLI with a personal access token minted with
  `is_agent=false`
- **THEN** the event carries `actor_kind: "agent"` and `auth: "pat"`

#### Scenario: Browser session is human

- **WHEN** a signed-in human reacts from the web viewer
- **THEN** the event carries `actor_kind: "human"` and `auth: "session"`

#### Scenario: Client cannot assert the kind

- **WHEN** a REST comment body includes `"actor_kind": "human"` or an
  `on_behalf_of` value, sent with a bearer token
- **THEN** the stored comment and its event carry `actor_kind: "agent"`, and the
  request field is ignored

### Requirement: EV-5 Approval Class and the Approval Bit

The system SHALL define an approval class of emoji, configured by
`CAIRN_APPROVAL_REACTIONS` (comma-separated). The default is 👍 (U+1F44D),
✅ (U+2705) and ✔️ (U+2714). Membership MUST be tested after stripping skin-tone
modifiers (U+1F3FB..U+1F3FF) and variation selector-16 (U+FE0F).

On `reaction.added` and `reaction.removed`:

- `reaction.approval_class` MUST be true exactly when the emoji is in the class;
- `reaction.approval` MUST be true exactly when `approval_class` is true **and**
  `actor_kind` is `human`.

Approval-class reactions from agent credentials MUST be accepted and stored. They
MUST NOT be refused.

#### Scenario: Agent thumbs-up is not an approval

- **WHEN** an agent reacts 👍 over MCP
- **THEN** the reaction is stored, and its event carries `approval_class: true`
  and `approval: false`

#### Scenario: Human thumbs-up with skin tone is an approval

- **WHEN** a human in a browser session reacts 👍🏽
- **THEN** the event carries `approval_class: true` and `approval: true`

#### Scenario: Withdrawn approval is announced

- **WHEN** that human removes the 👍🏽
- **THEN** a `reaction.removed` event carries `approval: true`, so a consumer can
  revoke the approval it acted on

### Requirement: EV-6 Per-Kind Reaction and Comment Ownership

`reactions` and `comments` MUST store `actor_kind`. Reaction idempotency MUST be
keyed on `(artifact_id, anchor_type, anchor_key, emoji, actor_id, actor_kind)`.
Every path that removes a reaction MUST match the caller's `actor_kind` as well as
its `actor_id`. Every path that edits or deletes a comment (#158) MUST do the same.
`reactions` MUST also store `on_behalf_of`, populated exactly as for comments
(#159).

Rows written before this change carry an empty `actor_kind`. They MUST be treated
as not `human`: they never produce `approval: true`, and only a caller whose
`actor_id` matches may remove them.

#### Scenario: Agent cannot occupy the human's row

- **WHEN** an agent for alice reacts 👍, and then alice reacts 👍 in her browser
- **THEN** two reaction rows exist, and the second insert emits `reaction.added`
  with `approval: true`

#### Scenario: Agent cannot withdraw the human's approval

- **WHEN** alice's agent calls un-react for 👍 after alice reacted 👍 in her
  browser
- **THEN** only the agent's row (if any) is removed, and alice's row and
  approval stand

#### Scenario: Agent cannot remove by id

- **WHEN** an agent credential calls `DELETE /v1/artifacts/{id}/reactions/{rid}`
  for a row with `actor_kind = human`
- **THEN** the response is 403 and nothing is deleted

#### Scenario: Legacy row is never an approval

- **WHEN** a reaction row created before this change is removed by its actor
- **THEN** its `reaction.removed` event carries `approval: false`

### Requirement: EV-7 Recipient Selection and Tenancy

Every internal event MUST carry the subject artifact's owner, so that owner and
team subscriptions (Cairn ADR-0029) receive only events about artifacts their
owner or team owns. The owner MUST NOT appear on the wire.

The kinds this capability adds MUST be delivered only to owned subscriptions
(Cairn ADR-0029) of the workspace that owns the subject artifact, and only to
those whose event-type filter admits the kind. They MUST NOT be sent to the
instance env targets (`CAIRN_OUTBOUND_WEBHOOK_URLS`), which ADR-0029 removes in
the change that ships subscriptions; there is no kind allowlist for env targets
and no interim path to them. Until subscriptions exist, the new kinds MUST be
emitted and counted, and MUST NOT be delivered. A subscription filter naming a
kind that is not in the EV-1 registry MUST be refused with a validation error
naming it. Events MUST NOT be routed to subscriptions of the event's actor
unless that actor's workspace owns the subject artifact.

#### Scenario: No env target receives the new kinds

- **GIVEN** a build where `CAIRN_OUTBOUND_WEBHOOK_URLS` is still read
- **WHEN** a user reacts 👍 to an artifact
- **THEN** no request carrying `reaction.added` is made to any env target

#### Scenario: Subscription filter selects kinds

- **WHEN** alice's subscription filters to `artifact.created,reaction.added`
- **THEN** it receives creations and reactions on her artifacts, and no comments
  or run closures

#### Scenario: Misspelt kind in a filter is refused

- **WHEN** a subscription is created with the event type `reaction.add`
- **THEN** the request is refused with a validation error naming `reaction.add`
  as an unknown kind

#### Scenario: Another owner's subscription is not notified

- **WHEN** bob comments on alice's artifact, and bob has his own subscription
- **THEN** bob's subscription receives nothing for that comment. Alice's
  subscriptions (once ADR-0029 lands) receive `comment.created`.

### Requirement: EV-8 Signed, Bounded Delivery with Creation Priority

Delivery MUST reuse SPEC-0012's queue, three-attempt retry, redirect refusal and
`X-Cairn-Signature`. `X-Cairn-Event` MUST equal the body's `kind`. When the
bounded queue is more than half full, the emitter MUST drop new non-creation
events before enqueueing, logging and counting each drop. `artifact.created`
MUST still be enqueued while capacity remains.

#### Scenario: Header matches kind

- **WHEN** a `run.closed` event is delivered
- **THEN** `X-Cairn-Event` is `run.closed` and the signature verifies over the
  raw body

#### Scenario: Reaction burst does not starve creation

- **WHEN** the queue is more than half full of pending reaction events and an
  artifact is created
- **THEN** new reaction events are dropped and counted, and the
  `artifact.created` event is enqueued

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary.
- Sentinel errors MUST be defined for failure modes callers distinguish (an
  unknown event kind, and an un-react forbidden by actor kind).
- Silent error swallowing MUST NOT occur. An emit failure MUST be logged with the
  event id and kind, never the target URL.
- Structured logging MUST be used for error reporting.

#### Scenario: Emit failure is logged, not surfaced

- **WHEN** encoding an event fails
- **THEN** the originating request succeeds and a structured log line names the
  kind and event id

### Requirement: Concurrency Safety

- Context propagation MUST be used for cancellation across the emitter worker.
- The worker MUST keep SPEC-0012's clean startup and graceful shutdown.
- Queue-priority accounting MUST be race-free.
- Emitter tests MUST run under the race detector in CI (`make ci`).

#### Scenario: Concurrent reactions

- **WHEN** 50 concurrent reactions insert rows while an artifact is created
- **THEN** every insert emits exactly one event, and the race detector reports
  nothing

### Requirement: Database Operation Standards

- The `actor_kind` columns and the new uniqueness index MUST ship in one
  migration. The index MUST be built without blocking writes.
- Reaction and comment writes and their counters MUST stay in one transaction
  (SPEC-0006 REQ "Count Aggregation").
- Queries MUST be parameterized.

#### Scenario: Migration over existing rows

- **WHEN** the migration runs over a database that already holds reactions
- **THEN** existing rows gain `actor_kind = ''` and remain unique under the new
  key

## Security Requirements

- **Authentication**: No new inbound endpoint is added. Every annotation write
  keeps its existing `requireAuth` + `annotations:write` + CSRF chain.
  `actor_kind` and `auth` come from the authenticated principal only.
- **Authorization**: An agent credential MUST NOT remove, edit or delete an
  annotation whose `actor_kind` is `human` (EV-6). `approval` is computed
  server-side (EV-5).
- **Rate limiting**: Unchanged inbound. Outbound fan-out is bounded by the queue,
  with creation priority (EV-8).
- **Security headers**: Deliveries are JSON POSTs, and no HTML is rendered.
- **Request body size limits**: Event bodies are server-generated. Comment bodies
  in events are capped at 4096 bytes (EV-3). Inbound annotation limits are
  unchanged (SPEC-0006).
- **CSRF protection**: Unchanged. Cookie-session annotation writes stay
  CSRF-guarded, which is what makes a session-derived `human` unforgeable
  cross-site.
- **Redirect validation**: The delivery client MUST NOT follow redirects
  (SPEC-0012).
- **Untrusted fields**: `tags` and `on_behalf_of` remain asserted (ADR-0018,
  SPEC-0012). Consumers MUST gate trust on `actor_kind`, `auth` and `approval`,
  never on `on_behalf_of` or tags.
- **Privacy**: No event kind reaches a target the operator chose. The only
  recipients are subscriptions owned by the subject artifact's user or team
  (EV-7, Cairn ADR-0029).

## Accessibility Requirements

The viewer SHOULD visually distinguish agent reactions from human reactions in
approval-class tallies. When it does:

- **WCAG 2.1 AA** is the target.
- **ARIA landmarks**: the viewer's existing landmarks are unchanged.
- **Icon-only controls**: an agent marker MUST carry an accessible name (for
  example "reacted by an agent").
- **Dynamic content**: tally updates keep the viewer's existing `aria-live="polite"`
  behaviour.
- **Keyboard navigation**: reaction controls stay keyboard-operable (SPEC-0006).
- **Focus management**: not applicable; no modal is introduced.
