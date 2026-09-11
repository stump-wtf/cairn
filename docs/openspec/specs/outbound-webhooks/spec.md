---
status: draft
date: 2026-09-06
implements: [ADR-0017, ADR-0018]
related: [SPEC-0002, SPEC-0005]
---

# SPEC-0012: Outbound Webhooks

## Graph Edges

- **Implements:** **ADR-0017** — in-process async fan-out of `artifact.created` events
- **Related:** **SPEC-0002** — artifact envelope, provenance, and expiry
- **Related:** **SPEC-0005** — inbound webhook capture; naming neighbor, not a dependency

(Rendered edges are derived from front-matter; body links to repo paths
would break on the generated site page.)

## Overview

When an artifact (single-body or bundle) is created through any surface — REST API,
web upload, CLI, or MCP — Cairn emits an `artifact.created` event over HTTP to each
configured outbound webhook target. The initial consumer is Switchboard, whose
generic-trust ingest URLs turn the event into a todo on a scoped queue, ringing a
doorbell on a live agent session. See ADR-0017 for the decision record.

The event also carries the artifact's client-asserted tags, so a consumer can route it
without reading the body. The main case is an agent handoff: a work order tagged
`handoff` that a Switchboard rule sends to a worker lane (ADR-0018).

## Requirements

### Requirement: Event Emission on Artifact Creation

The system SHALL emit an `artifact.created` event after a single-body artifact or a
bundle is durably created, regardless of the creating surface (REST, web, CLI, MCP).
Emission MUST NOT block or fail the creation request: an emitter failure MUST NOT
change the API response.

#### Scenario: REST creation emits

- **WHEN** a client POSTs `/v1/artifacts` and receives 201
- **THEN** an `artifact.created` event for the new artifact is delivered to every
  configured target

#### Scenario: MCP creation emits

- **WHEN** an agent calls the `artifact_create` MCP tool successfully
- **THEN** the same `artifact.created` event shape is emitted as for REST creation

#### Scenario: Bundle creation emits

- **WHEN** a bundle is created via the multipart endpoint
- **THEN** an `artifact.created` event with `share_type: "bundle"` is emitted

#### Scenario: Emitter failure is isolated

- **WHEN** a target is unreachable or the emitter errors
- **THEN** the artifact creation response is unaffected and the error is logged

### Requirement: Event Payload

The event body SHALL be a JSON object with at minimum:

- `source` (`"cairn"`);
- `kind` (`"artifact.created"`);
- `event_id` (unique per event);
- `created_at` (RFC 3339);
- a `data` object carrying the artifact's `id`, `share_type`, `title`, web `url`,
  `channel`, `model` (when present), `actor_id` (when present), `expires_at` (when
  present), `on_behalf_of` (when present), and `tags` (when the artifact has any).

`on_behalf_of` is the connected MCP client's `initialize` name/version. The server
records it from the session, never from a request field, but the client reports it
about itself, and it is empty for REST/CLI creates (SPEC-0007). `tags` is the
artifact's normalized tag list, in stored order (SPEC-0002 REQ "Artifact Tags"). The
`url` MUST be the public web URL of the artifact.

Payload changes MUST be additive. An existing field's name, meaning, and encoding MUST
NOT change. A field that does not apply to an artifact MUST be omitted rather than
emitted empty, so the event for an artifact without the newer fields is byte-identical
to the event from before those fields were introduced.

`tags` are client-asserted. A consumer MUST NOT base a trust or authorization
decision on `data.tags`. `data.actor_id` (the authenticated principal) and
`data.channel` are the server-derived identity fields; `data.on_behalf_of` is
self-reported harness context.

#### Scenario: Payload shape

- **WHEN** any event is delivered
- **THEN** the body parses as JSON and contains the fields above with `source` equal
  to `cairn` and `kind` equal to `artifact.created`

#### Scenario: Tags and on-behalf-of carried

- **WHEN** an agent creates an artifact or a bundle over MCP with tags
- **THEN** `data.tags` MUST equal the stored tags and `data.on_behalf_of` MUST equal
  the connected MCP client's `initialize` name/version

#### Scenario: Untagged payload unchanged

- **WHEN** an artifact with no tags and no on-behalf-of is created
- **THEN** the body MUST contain neither a `tags` nor an `on_behalf_of` key, and MUST
  be byte-identical to the pre-tags payload for the same field values

#### Scenario: Signature covers tags

- **WHEN** a tagged event is delivered with `CAIRN_OUTBOUND_WEBHOOK_SECRET` set
- **THEN** `X-Cairn-Signature` MUST verify over the raw body, `data.tags` included

### Requirement: Delivery Targets from Configuration

Targets SHALL be configured via the comma-separated environment variable
`CAIRN_OUTBOUND_WEBHOOK_URLS`. When the variable is unset or empty, the system MUST
NOT emit anything (feature inert by default). Target URLs MUST NOT be logged.

#### Scenario: Inert by default

- **WHEN** `CAIRN_OUTBOUND_WEBHOOK_URLS` is empty
- **THEN** artifact creation behaves exactly as before and no outbound HTTP is made

#### Scenario: Multiple targets

- **WHEN** two URLs are configured
- **THEN** each receives its own copy of the event

### Requirement: Signed Delivery

Each delivery SHALL carry `X-Cairn-Event: artifact.created`, `X-Cairn-Event-Id`
(matching `event_id`), and, when `CAIRN_OUTBOUND_WEBHOOK_SECRET` is set,
`X-Cairn-Signature: sha256=<hex>` — the HMAC-SHA256 of the raw request body. The
signature comparison at consumers MUST be constant-time. When the secret is unset,
the signature header MUST be omitted.

#### Scenario: Signature present with secret

- **WHEN** the secret is configured and an event is delivered
- **THEN** the `X-Cairn-Signature` header verifies against the raw body with the
  configured secret

#### Scenario: No secret, no header

- **WHEN** no secret is configured
- **THEN** deliveries carry no `X-Cairn-Signature` header

### Requirement: Bounded Async Delivery with Retry

Delivery SHALL be asynchronous to the creation request, through a bounded in-memory
queue. The system MUST attempt each delivery up to 3 times with short backoff before
dropping the event and logging the failure. When the queue is full, the event MUST
be dropped and logged, never block the caller.

#### Scenario: Retry then succeed

- **WHEN** a target fails twice then succeeds
- **THEN** the event is delivered exactly once to that target and not dropped

#### Scenario: Exhausted retries

- **WHEN** a target fails 3 times
- **THEN** the event is dropped for that target and a failure is logged

### Requirement: Graceful Lifecycle

The emitter worker SHALL start with the server and shut down gracefully on context
cancellation, draining or abandoning in-flight attempts within a bounded shutdown
window. Restart behavior follows ADR-0017: queued events are not persisted.

#### Scenario: Shutdown

- **WHEN** the server receives SIGTERM mid-delivery
- **THEN** the process exits within the shutdown window without deadlocking

## Security Requirements

- **Authentication**: Emission is server-initiated to operator-configured targets;
  no new unauthenticated inbound endpoint is introduced. Target URLs act as bearer
  capabilities and MUST NOT be logged or echoed in errors.
- **Rate limiting**: Outbound fan-out is bounded by the in-memory queue cap and one
  worker; no inbound rate surface changes. Consumers apply their own limits.
- **Security headers**: Deliveries are POSTs with `Content-Type: application/json`;
  no HTML rendering is involved. TLS is required for non-localhost targets.
- **Request body size limits**: Event bodies are server-generated and bounded by the
  artifact metadata they carry (never artifact content). Tags add at most 32 × 64
  bytes plus JSON punctuation. No inbound body limit changes.
- **Untrusted fields**: `data.tags` is client-asserted and MUST NOT drive a
  consumer's trust or authorization decision (ADR-0018). A receiver of a handoff
  treats it as semi-trusted and guards against prompt injection in the artifact body.
- **CSRF protection**: Not applicable — no browser-facing state change; targets are
  machine consumers with token/HMAC verification.
- **Redirect validation**: The delivery client MUST NOT follow HTTP redirects to
  avoid leaking signed bodies or capability URLs to unconfigured hosts.

## Accessibility Requirements

Not applicable — no UI is introduced by this capability.
