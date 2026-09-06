---
status: draft
date: 2026-09-06
implements: [ADR-0017]
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

The event body SHALL be a JSON object with at minimum: `source` (`"cairn"`), `kind`
(`"artifact.created"`), `event_id` (unique per event), `created_at` (RFC 3339), and
a `data` object carrying the artifact's `id`, `share_type`, `title`, web `url`,
`channel`, `model` (when present), and `expires_at` (when present). The `url` MUST
be the public web URL of the artifact.

#### Scenario: Payload shape

- **WHEN** any event is delivered
- **THEN** the body parses as JSON and contains the fields above with `source` equal
  to `cairn` and `kind` equal to `artifact.created`

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
  artifact metadata they carry (never artifact content); no inbound body limit
  changes.
- **CSRF protection**: Not applicable — no browser-facing state change; targets are
  machine consumers with token/HMAC verification.
- **Redirect validation**: The delivery client MUST NOT follow HTTP redirects to
  avoid leaking signed bodies or capability URLs to unconfigured hosts.

## Accessibility Requirements

Not applicable — no UI is introduced by this capability.
