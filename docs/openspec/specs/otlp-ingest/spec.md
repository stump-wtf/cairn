---
status: draft
date: 2026-07-29
implements: [ADR-0015]
requires: [SPEC-0002, SPEC-0004]
---

# SPEC-0011: OTLP Trace Ingestion

## Overview

Cairn accepts standard **OTLP/HTTP trace exports** and translates them, at ingest, into
the native run/span tree of SPEC-0004. This realizes **ADR-0015**: agents and harnesses
already think and instrument in OpenTelemetry, so the OTel→Cairn translation moves from
every capturing agent (where it was observed to garble) into one tested server-side
adapter. The native model remains canonical; OTLP is a dialect spoken at the border. A
stock OTLP/HTTP exporter pointed at Cairn with a bearer token yields a shareable run.

The adapter is a translation layer in the ADR-0012 sense: it feeds the same core ingest
methods the REST surface uses, so every SPEC-0004 rule — temporal containment, timing
validity, ingest bounds, output spill — applies to translated spans identically.

### HTTP endpoints

| Method & Path | Purpose | Auth |
|---|---|---|
| `POST /v1/otlp/v1/traces` | OTLP/HTTP trace export → native run/span ingest | Required |

The path mirrors the OTLP/HTTP convention (`/v1/traces`) under Cairn's `/v1/otlp` mount so
stock exporters need only an endpoint base URL and an `Authorization` header.

## Requirements

### Requirement: OTLP/HTTP Endpoint and Encodings

The server MUST accept OTLP/HTTP trace export requests at `POST /v1/otlp/v1/traces` in
both standard encodings — `application/x-protobuf` and `application/json` — and MUST
respond with the corresponding OTLP/HTTP response encoding. A request that decodes but
fails Cairn validation MUST be rejected whole (atomically, per the SPEC-0004 ingest
rules) with an OTLP-conformant error response carrying Cairn's sentinel in the message;
the adapter MUST NOT report partial success for a payload it did not persist. A request
that does not decode as OTLP MUST be rejected with `validation_failed` semantics in the
OTLP error shape. The endpoint MUST NOT be exposed as an MCP tool: OTLP payloads are
exactly the oversized captures SPEC-0004's transport steering keeps off that surface.

#### Scenario: Stock exporter round-trip

- **WHEN** an unmodified OTLP/HTTP exporter posts a protobuf-encoded trace export with a valid bearer token
- **THEN** the server MUST translate and persist it as a native run and respond with an OTLP/HTTP success response

#### Scenario: JSON encoding accepted

- **WHEN** the same trace is posted with `content-type: application/json` in OTLP/JSON form
- **THEN** the resulting run MUST be identical to the protobuf ingest of that trace

#### Scenario: No MCP surface

- **WHEN** the MCP tool list is enumerated
- **THEN** it MUST NOT contain an OTLP ingestion tool

### Requirement: Trace-to-Run Lifecycle

The adapter MUST group incoming spans by `trace_id`. The first spans seen for a
`trace_id` MUST open a native run via the SPEC-0004 incremental path (recording
provenance as OTLP-ingested); subsequent exports carrying the same `trace_id` MUST
append to that run while it is open. A single export request carrying spans from
multiple `trace_id`s MUST ingest each trace into its own run. The run's `started_at`
MUST be the earliest span start seen for the trace, and span offsets MUST be recomputed
relative to it (an append that arrives with an earlier start than the run's current
`started_at` MUST be rejected with `validation_failed`, because offsets already
persisted would become wrong). The run MUST close via the native close call or the
deployment's abandoned-run policy; a closed run refuses further OTLP appends exactly as
it refuses native ones.

#### Scenario: Batched live export appends

- **WHEN** an exporter posts three successive batches for one `trace_id` while the run is open
- **THEN** all spans MUST land in one run, in the order given by their timings, with no duplicate run created

#### Scenario: Multi-trace batch fans out

- **WHEN** one export request carries spans for two `trace_id`s
- **THEN** the server MUST persist two runs, each containing only its own trace's spans

#### Scenario: Append predating the run start rejected

- **WHEN** an append for an open OTLP run carries a span starting earlier than the run's recorded `started_at`
- **THEN** the server MUST reject that request with `validation_failed` and leave the run unchanged

### Requirement: Span Translation

For each OTLP span the adapter MUST carry over `span_id` and `parent_span_id`
(hex-encoded), translate the span `name` verbatim, and derive `start_offset_ms` from the
span's start relative to the run's `started_at` and `duration_ms` from `end − start`,
rounding a positive sub-millisecond duration UP to 1 ms — a measured instant is 1 ms by
resolution, never a rejected zero. A span whose `end` precedes its `start` MUST be
rejected. Tool identity and arguments MUST be derived from semantic-convention
attributes where present (e.g. `gen_ai.tool.name` → `tool`; tool-call argument
attributes → `args`); attributes with no mapping MUST be dropped, not guessed at.
Span events, span links, trace state, sampling flags, and span `status` MUST NOT be
persisted in v1; resource attributes MUST be dropped except `service.name`, which MUST
inform the run's provenance description.

#### Scenario: Sub-millisecond operation survives

- **WHEN** an OTLP span measures 400 µs
- **THEN** it MUST persist with `duration_ms: 1`, not be rejected as a zero-duration span

#### Scenario: Inverted timestamps rejected

- **WHEN** an OTLP span's end timestamp precedes its start
- **THEN** the server MUST reject the request with `validation_failed`

#### Scenario: Status is not smuggled in

- **WHEN** an OTLP span carries `status: ERROR`
- **THEN** the persisted span MUST NOT carry any error/status field (per SPEC-0004's v1 non-goals); the status is dropped

### Requirement: Category Mapping

The adapter MUST derive each span's `category` by the first matching rule, in order:

1. An explicit `cairn.category` span attribute MUST win, verbatim (subject to
   SPEC-0004's length/emptiness rules).
2. `gen_ai.operation.name` values denoting model inference (chat, text completion,
   content generation) MUST map to `reason`; values denoting tool execution (or the
   presence of `gen_ai.tool.name`) MUST map to `tool`.
3. Spans carrying HTTP or RPC client semantics (`http.request.method`, `rpc.system`)
   MUST map to `net`; spans carrying `db.system` MUST map to `read`.
4. Otherwise the category MUST be the span's OTel `SpanKind` name lowercased
   (`internal`, `client`, `server`, `producer`, `consumer`) — a deterministic landing
   in SPEC-0004's open category set that renders via its hashed-color path and is
   honest about carrying no better signal.

The mapping table MUST be maintained in one place in the implementation, and a span
MUST NEVER be rejected for its vocabulary — rejection is reserved for the structural
and timing rules of SPEC-0004.

#### Scenario: Explicit attribute wins

- **WHEN** a span carries `cairn.category: debug` alongside `http.request.method: GET`
- **THEN** it MUST persist with category `debug`

#### Scenario: gen_ai tool call

- **WHEN** a span carries `gen_ai.tool.name: web_search`
- **THEN** it MUST persist with category `tool` and tool `web_search`

#### Scenario: Unmapped span lands in the open set

- **WHEN** a span carries no mapped attributes and `SpanKind: INTERNAL`
- **THEN** it MUST persist with category `internal` and render through the open-set hashed-color path

### Requirement: Validation Parity With Native Ingest

Translated spans MUST pass through the same core ingest as native spans, and every
SPEC-0004 rule MUST apply identically: temporal containment of children, `duration_ms`
≥ 1, `start_offset_ms` ≥ 0, category length, per-request and per-run span bounds,
oversized-output spill to object storage. Violations MUST surface the same sentinel
errors as native ingest (`invalid-duration`, `child-outside-parent`,
`span-bound-exceeded`, …) inside the OTLP error response. The adapter MUST NOT
implement a second, laxer validation standard.

#### Scenario: Containment enforced through the side door

- **WHEN** an OTLP trace contains a child span whose interval extends beyond its parent's
- **THEN** the request MUST be rejected atomically with the `child-outside-parent` sentinel, exactly as native ingest would

#### Scenario: Span bounds apply

- **WHEN** one OTLP export request carries more spans than the SPEC-0004 per-request bound
- **THEN** the request MUST be rejected with the `span-bound-exceeded` sentinel and its message MUST name the paging path

### Requirement: Error Handling Standards

The adapter MUST wrap decode and translation errors with context (trace_id, span count,
encoding) so the handler maps them to stable error codes without string-matching, and
MUST define an `otlp-decode-failed` sentinel distinct from the core ingest sentinels it
passes through. Errors MUST NOT be silently swallowed; every rejected export MUST be
recorded with structured logging carrying the `request_id`, and the OTLP error response
MUST carry a message a human can act on.

#### Scenario: Decode failure is distinct from validation failure

- **WHEN** a request body is not decodable OTLP
- **THEN** the failure MUST surface the `otlp-decode-failed` sentinel, distinct from core validation sentinels, and be logged with the `request_id`

### Requirement: Concurrency Safety

Concurrent export requests for the same `trace_id` MUST be safe: the trace_id→run
mapping MUST be created exactly once under concurrency (no duplicate runs for one
trace), and concurrent appends MUST receive gap-free `seq` assignment per SPEC-0004's
concurrency rules. Context cancellation MUST propagate through decode, translation, and
persistence. Race detection MUST run in CI over the adapter's concurrent paths.

#### Scenario: Racing first batches create one run

- **WHEN** two export requests for the same previously-unseen `trace_id` race
- **THEN** exactly one run MUST be created and both requests' spans MUST land in it (or one request MUST fail atomically and be retryable) — never two runs for one trace

## Security Requirements

This capability is web-facing. The following are MANDATORY.

### Requirement: Authentication & Authorization

`POST /v1/otlp/v1/traces` MUST require OAuth 2.1 bearer authentication (ADR-0004) — the
header a stock OTLP exporter is configured with. Unauthenticated requests MUST receive
401 with no ingestion side effects. Runs created via OTLP MUST be owned by the
authenticated principal, and appends to an existing OTLP run MUST be refused for any
other principal exactly as native appends are.

#### Scenario: Missing bearer token

- **WHEN** an exporter posts a trace without an `Authorization` header
- **THEN** the server MUST respond 401 and persist nothing

#### Scenario: Cross-principal append refused

- **WHEN** a second authenticated principal exports spans for a `trace_id` whose run is owned by another principal
- **THEN** the server MUST refuse the append (403 or a fresh run under the second principal — never a write into the first principal's run)

### Requirement: Rate Limiting

The OTLP endpoint MUST share the ingestion rate-limit regime of SPEC-0004: per-identity
limits, 429 with `Retry-After` on excess, and no processing of throttled requests.

#### Scenario: Export flood throttled

- **WHEN** an exporter exceeds the configured rate
- **THEN** the server MUST respond 429 with `Retry-After` and persist no span from the throttled request

### Requirement: Security Headers & Content Handling

Responses MUST carry the standard security headers (nosniff, referrer policy, HSTS over
HTTPS). All translated content — span names, attribute-derived args and outputs,
`service.name` — is untrusted agent-supplied data and MUST flow into the same escaped
rendering paths SPEC-0004 mandates; the adapter MUST NOT introduce any path where OTLP
attribute content is treated as markup or executed.

#### Scenario: Hostile span name

- **WHEN** an OTLP span name contains HTML
- **THEN** it MUST render in the viewer as escaped inert text, exactly as a native span name would

### Requirement: Request Body Size Limits

The OTLP endpoint MUST enforce the same maximum request size as native ingestion,
rejecting oversize bodies with 413 before buffering the full payload, and MUST apply
SPEC-0004's per-span output bounds (spill or truncate) to attribute-derived outputs.

#### Scenario: Oversize export

- **WHEN** an export exceeds the configured body-size limit
- **THEN** the server MUST reject it with 413 and persist nothing

### Requirement: CSRF Protection

The endpoint is bearer-token-only — it MUST NOT accept session-cookie authentication,
which makes CSRF structurally inapplicable (no ambient credential). This exemption MUST
be enforced by rejecting cookie-authenticated requests rather than by assumption.

#### Scenario: Session cookie is not enough

- **WHEN** a browser session posts to the OTLP endpoint with cookies but no bearer token
- **THEN** the server MUST respond 401

### Requirement: Redirect & SSRF Validation

The adapter MUST NOT fetch, follow, or resolve any URL found in OTLP attributes —
endpoint URLs, `http.url` values, and link targets are stored (or dropped) as inert
data. The endpoint MUST NOT issue redirects.

#### Scenario: Captured URL is data

- **WHEN** a span attribute carries `http.url: http://169.254.169.254/latest/meta-data`
- **THEN** Cairn MUST NOT issue any request to it; the value is inert captured data at most
