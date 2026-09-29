# Design: Artifact Relations and Trace Context

## Context

Artifacts have no relation fields. The only artifact-to-artifact link is `bundle_members`.
`produced_edges` (`0004_trajectory.sql`) links a span to an artifact, and has no relation between
artifacts.

`artifact.created` is built in `internal/outboundhook/outboundhook.go`:

* `eventData` holds the payload fields.
* `EmitArtifactCreated` builds the event and calls `encode`.
* The golden file `testdata/artifact_created_rest.golden.json` pins the output.

Nothing in Cairn reads or stores W3C trace context. Every create ends in `store.CreateArtifact`
(`internal/store/create.go`) or `store.CreateBundle` (`internal/store/bundle.go`).

SPEC-0023 (teams and tenancy) is accepted but not implemented on `main`. Its `authorizeRead` does
not exist yet, and `GetByPublicID` checks only existence and expiry (#182). This design depends on
that check, so the relation-target check is written against a single function that SPEC-0023 later
replaces.

## Goals / Non-Goals

### Goals

* Typed, immutable relations with an authorization rule on create and on read.
* A standard `traceparent` on create, stored as `trace_id`.
* Both fields in `artifact.created`, additive, with the golden file unchanged when they are absent.

### Non-Goals

* Editing relations after creation.
* Linking artifacts to OTLP runs by `trace_id`. That follows ADR-0015's receiver.
* Fixing `produced_edges`' missing ownership check. That is tracked separately on the canonical
  tracker.

## Decisions

### Schema

```sql
CREATE TABLE artifact_relations (
    artifact_id         BIGINT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    kind                TEXT   NOT NULL CHECK (kind IN ('reply_to','derived_from','follows')),
    target_artifact_id  BIGINT REFERENCES artifacts(id) ON DELETE SET NULL,
    target_public_id    TEXT   NOT NULL,
    position            SMALLINT NOT NULL CHECK (position BETWEEN 0 AND 7),
    PRIMARY KEY (artifact_id, kind, target_public_id)
);
CREATE INDEX artifact_relations_target_idx ON artifact_relations (target_artifact_id)
    WHERE target_artifact_id IS NOT NULL;

ALTER TABLE artifacts ADD COLUMN trace_id       BYTEA CHECK (octet_length(trace_id) = 16);
ALTER TABLE artifacts ADD COLUMN parent_span_id BYTEA CHECK (octet_length(parent_span_id) = 8);
CREATE INDEX artifacts_trace_id_idx ON artifacts (trace_id) WHERE trace_id IS NOT NULL;
```

`target_public_id` is kept so an expired target can still be shown as "no longer available" in the
right position. It is never returned to a reader who cannot read the target.

### One function decides readability

`internal/store` gains `readableTargets(ctx, principal, publicIDs []string) (map[string]int64,
error)`. It returns the internal ids of the targets the principal can read. Today it applies
`GetByPublicID`'s rule, which is unexpired, looked up in one `ANY($1)` query. SPEC-0023's
implementation replaces its body with `authorizeRead`.

* **Create:** fails with `relation-target-not-found` if any requested id is missing from the map.
* **Read:** filters `relations` and `referenced_by` through the same function, run as the reader.

### Parsing `traceparent`

A small package, `internal/tracectx`, provides `Parse(s string) (traceID [16]byte, spanID [8]byte,
ok bool)`. It follows W3C strictly:

* 55 bytes, `00-<32 hex>-<16 hex>-<2 hex>`;
* lowercase hex only;
* neither id all zeros;
* unknown versions rejected.

It has table tests. The REST handler reads `r.Header.Get("traceparent")`. MCP gains a
`traceparent` string argument, and must be added to the per-tool argument allowlist in
`internal/httpapi/mcp.go`. The CLI reads `--traceparent`, or `os.Getenv("TRACEPARENT")`.

### Event payload

`eventData` gains:

```go
Relations []eventRelation `json:"relations,omitempty"`
TraceID   string          `json:"trace_id,omitempty"`
```

Both are appended after `tags`. `store.CreationEvent` carries the relations already filtered to the
artifact's workspace. Before SPEC-0023 exists, the workspace is the owner: include a relation only
when the target's `owner_id` equals the new artifact's `owner_id`.

### Viewer

Relations are fetched in the same read as the artifact:

* `artifact_relations LEFT JOIN artifacts` for the targets;
* a second query on `artifact_relations_target_idx` for `referenced_by`, with `LIMIT 51` to detect
  truncation.

Both pass through `readableTargets` as the reader.

## Where the pieces live

| Piece | Location |
| --- | --- |
| Migration | `internal/db/migrations/00NN_artifact_relations.sql` (the next free number; `feat/owner-columns` is also renumbering) |
| Relation validation | `internal/artifact/relations.go` (`NormalizeRelations`, like `NormalizeTags`) |
| Trace context | `internal/tracectx` |
| Store | `internal/store/create.go`, `bundle.go`, `read.go`, `relations.go` |
| MCP, REST, CLI | `internal/httpapi/mcp.go` (`mcpCreateInput`, `mcpBundleCreateInput`), `handlers.go`, `internal/httpapi/tags.go` (header and query parsing, mirrored), `cmd/cairn` |
| Event | `internal/outboundhook/outboundhook.go`, and a new golden file for the related and traced case |
| Viewer | the artifact view template, in both the bin and the viewer |

## Risks / Trade-offs

* **Pre-tenancy authorization is weak.** Until SPEC-0023 lands, "readable" means "exists and
  unexpired", because that is what reads enforce today (#182). Relations are no worse than reads,
  and they tighten automatically when `readableTargets` does.
* **The reverse lookup is a new query on every read.** It is bounded by the partial index and
  `LIMIT 51`.

## Migration Plan

The migration is additive, with no backfill. Existing artifacts have no relations and no trace id.
The golden payload for an unrelated artifact is unchanged.
