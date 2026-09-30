---
status: draft
date: 2026-09-27
implements: [ADR-0030]
requires: [SPEC-0002, SPEC-0012, SPEC-0023]
related: [SPEC-0004, SPEC-0008, SPEC-0011, SPEC-0021]
---

# SPEC-0024: Artifact Relations and Trace Context

## Graph Edges

- **Implements:** **ADR-0030**. An artifact can declare typed relations to other artifacts, and can
  carry the trace that produced it.
- **Requires:** **SPEC-0002**. The artifact core and its create surfaces gain `relations` and
  `traceparent`.
- **Requires:** **SPEC-0012**. `artifact.created` gains `relations` and `trace_id`.
- **Requires:** **SPEC-0023**. `authorizeRead` governs relation targets on create and on read, and
  events stay in the artifact's workspace.
- **Related:** **SPEC-0004** (produced edges), **SPEC-0008** (the CLI), **SPEC-0011** (OTLP ingest,
  keyed by trace id), **SPEC-0021** (the receipt `follows` field, which becomes a `follows` relation).

## Overview

An artifact MAY declare up to 8 **relations** at creation. Each relation is typed, as `reply_to`,
`derived_from` or `follows`, and each target must be an artifact the creator can read. Relations are immutable.

Relations appear in four places:

* on read, without revealing targets the reader cannot open;
* as reverse `referenced_by` lists;
* in `artifact.created`, limited to the artifact's own workspace;
* in the viewer.

An artifact MAY also carry the W3C **trace context** of the run that produced it. The
`traceparent` supplies it; its `trace_id` is returned on read and in `artifact.created`. See
ADR-0030.

## Requirements

### Requirement: Relation Kinds and Limits

A relation SHALL be `{kind, id}`:

* `kind` is exactly `reply_to`, `derived_from` or `follows`.
* `id` is an artifact public id.

An artifact SHALL declare at most 8 relations. Duplicate `(kind, id)` pairs SHALL be dropped,
keeping first-occurrence order. A relation to the artifact itself is impossible, because ids are
minted at creation. Any other kind, or more than 8 relations, SHALL be rejected with
`validation_failed`. Relations SHALL be immutable after creation.

### Requirement: Declaring Relations on Create

Every create surface SHALL accept relations:

* MCP: `artifact_create` and `bundle_create` gain the `relations` argument.
* REST: `POST /v1/artifacts` accepts repeatable `X-Cairn-Relation: <kind>=<id>` headers and
  `relation=<kind>:<id>` query parameters. A multipart create also accepts `relation` form fields.
  All sources are merged.
* CLI: `--reply-to <id>`, `--derived-from <id>` and `--follows <id>`, each repeatable.

#### Scenario: A reply over MCP

- **WHEN** a client calls `artifact_create` with
  `relations: [{"kind":"reply_to","id":"a1b2c3"}]`, and the caller can read `a1b2c3`
- **THEN** the new artifact stores the relation, and the create response returns it

### Requirement: Relation Targets Must Be Readable by the Creator

Each target SHALL pass the same read authorization as a read by the creating principal. That is
`authorizeRead` (SPEC-0023), or, before SPEC-0023 is implemented, the unexpired-lookup semantics of
`GetByPublicID`.

If any target fails, the whole create SHALL be rejected with `validation_failed` and the sentinel
`relation-target-not-found`. Nothing is persisted. The response SHALL be byte-identical for an
unknown id, an expired id and an id the creator cannot read, apart from request ids.

#### Scenario: Probing with relations

- **WHEN** user U creates an artifact with a relation to V's private artifact, and again with a
  relation to a random id
- **THEN** both creates fail with identical `relation-target-not-found` errors

### Requirement: Receipt `follows` Is a Relation

When a receipt (SPEC-0021) is created with `follows`, and SPEC-0021 REQ-6 accepts it, the system
SHALL also store a `follows` relation to that receipt, in the same transaction. SPEC-0021's stricter
rule (a live receipt with the same owner, and `invalid_follows` otherwise) SHALL continue to govern
the receipt field. A receipt MAY carry both `follows` and explicit `relations`. A duplicate `follows`
pair is stored once.

### Requirement: Relations on Read

Every read surface SHALL return `relations`: web, `GET /v1/artifacts/{id}`, MCP `artifact_read`,
and the CLI. How each relation is shown depends on the reader:

* **The reader can read the target.** The relation carries `{kind, id, title}`.
* **The reader cannot read the target, or it has expired or been deleted.** The relation carries
  `{kind, available: false}`, with no id and no title.

Reads SHALL also return `referenced_by`: up to 50 `{kind, id, title}` entries, one for each
artifact that relates to this one and that the reader can read, newest first, with a
`referenced_by_truncated` flag.

#### Scenario: Hidden target

- **WHEN** reader R can open B, B replies to A, and R cannot open A
- **THEN** B's relations show one `reply_to` with `available: false`, and A's id appears nowhere
  in the response

### Requirement: Relations in artifact.created

`artifact.created` SHALL gain `relations: [{kind, id}]`, serialized with `omitempty`. A relation
SHALL be included only when its target belongs to the same owner workspace as the new artifact: the
same user, or the same team. The untagged, unrelated golden payload SHALL stay byte-identical.

#### Scenario: Cross-workspace relation omitted

- **WHEN** a team artifact is created with a relation to its creator's personal artifact
- **THEN** its `artifact.created` event carries no `relations` field

### Requirement: Trace Context on Create

Every create surface SHALL accept an optional W3C `traceparent`:

* MCP: the `traceparent` argument.
* REST: the `traceparent` request header.
* CLI: `--traceparent`, defaulting to the `TRACEPARENT` environment variable.

A valid version-`00` value SHALL store `trace_id` (16 bytes) and `parent_span_id` (8 bytes) on the
artifact. An all-zero id is invalid. A missing or invalid value SHALL be ignored and SHALL NOT fail
the create. `tracestate` SHALL be ignored.

### Requirement: Trace Id on Read and in Events

Reads SHALL return `trace_id`, as 32 lowercase hex characters, when it is set.
`artifact.created` SHALL gain `trace_id`, serialized with `omitempty`.

#### Scenario: Trace id round-trips

- **WHEN** an artifact is created with
  `traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`
- **THEN** its read and its `artifact.created` both carry
  `trace_id: 4bf92f3577b34da6a3ce929d0e0e4736`

### Requirement: Viewer Rendering

The artifact viewer SHALL render relations above the body:

* "In reply to <title>", "Derived from <title>" or "Follows <title>", linked, when available;
* "an artifact you can't open", unlinked, when not available.

It SHALL render a "Replies and derivatives" list, from `referenced_by`, below the body. All titles
SHALL be rendered escaped.

### Requirement: Database Operation Standards

Relations SHALL be stored in a new `artifact_relations` table with these columns:

* `artifact_id` (a foreign key, `ON DELETE CASCADE`);
* `kind`;
* `target_artifact_id` (a foreign key, `ON DELETE SET NULL`);
* `target_public_id`;
* `position`.

The primary key is `(artifact_id, kind, target_public_id)`, and there is an index on
`target_artifact_id`. `artifacts` SHALL gain nullable `trace_id bytea` (16 bytes) and
`parent_span_id bytea` (8 bytes), with a partial index on `trace_id`. The relations SHALL be written
in the same transaction as the artifact. The migration SHALL take the next free number at merge
time.

## Security Requirements

### Authentication

| Surface | Auth | Description |
| --- | --- | --- |
| Create (REST, MCP, CLI) with `relations` / `traceparent` | Required | Existing create auth; targets checked with the creator's read authorization |
| Reads returning `relations` / `referenced_by` / `trace_id` | Per existing read rules | Target visibility checked per reader |
| `artifact.created` | Existing HMAC-signed delivery | Relations are limited to the artifact's workspace |

### Rate Limiting

There are no new routes. The existing create and read limiters apply.

### Request Body Size Limits

The limits are unchanged. There are at most 8 relations. A `traceparent` over 55 bytes is ignored.

### Redirect Validation

There are no redirects. Relation links are internal artifact URLs.

### Input Handling

Relation ids SHALL be validated against the public-id grammar before lookup. `traceparent` SHALL be
parsed strictly: lowercase hex, fixed widths, and version `00`.

## Accessibility Requirements

- **WCAG 2.1 AA Compliance.** The relation header and the "Replies and derivatives" list SHALL meet
  WCAG 2.1 AA in both themes.
- **Structure.** The list SHALL be a list with a visible heading. An unavailable relation SHALL be
  plain text, not a disabled link.
- **Keyboard Navigation.** Relation links SHALL be in tab order with visible focus.
