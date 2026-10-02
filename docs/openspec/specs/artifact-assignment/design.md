# Design: Artifact Assignment

## Context

Artifacts carry no assignee. The only identity signal on a handoff is the
client-asserted `agent:<login>` tag (ADR-0018), set once at creation and
unvalidated. Re-routing a handoff today means re-sharing it as a new artifact,
which strands the annotation stream (ADR-0006).

Everything this design touches exists on `main`:

* the artifact aggregate and its create paths end in `store.CreateArtifact`
  (`internal/store/create.go`) and `store.CreateBundle`
  (`internal/store/bundle.go`);
* `authorizeRead`, teams and roles land with SPEC-0023
  (ADR-0029) — the assignment authorization reuses them rather than
  inventing a permission;
* outbound delivery is `internal/outboundhook`, with golden files in
  `internal/outboundhook/testdata/` pinning the event payloads;
* migrations live in `internal/db/migrations/`, next free number **0036**
  (the last is `0035_outbound_subscriptions.sql`).

SPEC-0023's `authorizeRead` gates the assignee-must-be-a-reader rule. Where it
is not yet merged when a given PR lands, the PR codes against the function
name and SPEC-0023 ships first in the plan below; the dependency order exists
so no PR ever codes against a placeholder.

## Goals / Non-Goals

### Goals

* One nullable assignee per artifact, reassignable in place, validated against
  `users` and readable by the assignee at assignment time (AS-1, AS-3).
* Two signed events: `assignee` in `artifact.created`, `artifact.assigned` on
  change — additive, byte-identical when unset (AS-6, AS-10).
* Assignment allowed for exactly the users who may change artifact metadata,
  agents included (AS-2).
* UI display and Bin filtering, index-backed (AS-8, AS-9).

### Non-Goals

* Multiple assignees, assignment history as a queryable table (the event stream
  is the history), assignment of bundles' members individually.
* A distinct "agent identity" object (ADR-0032: an agent is a user).
* Switchboard-side routing changes — filed separately as
  https://gitea.stump.rocks/stump.wtf/switchboard/issues/558.

## Data Model

```sql
-- internal/db/migrations/0036_artifact_assignee.sql
ALTER TABLE artifacts
    ADD COLUMN assignee_user_id BIGINT NULL REFERENCES users(id);

CREATE INDEX idx_artifacts_assignee
    ON artifacts (assignee_user_id)
    WHERE assignee_user_id IS NOT NULL;
```

No backfill; no default. `NULL` means "unassigned" and is the only sentinel —
cleared assignments write `NULL`, they do not tombstone.

## API Design

### REST

* Create: `assignee=<username>` query parameter, `X-Cairn-Assignee` header, or
  multipart field — merged with the same precedence rules as tags
  (SPEC-0002 REQ "Artifact Tags"). Validated before persistence.
* Change/clear: `PUT /v1/artifacts/{id}/assignee`, body
  `{"assignee": "<username>"}` or `{"assignee": null}`. Wired like
  `PATCH /v1/artifacts/{id}/policy` (`internal/httpapi/policy.go`): auth, CSRF,
  authorizeRead-then-authorize, service call, response is the artifact JSON.
* Artifact JSON (reads, bin, create, assign responses) gains `assignee`
  (the username) when set; omitted otherwise.

### MCP

* `artifact_create` and `bundle_create` gain optional `assignee` (string).
* New tool `artifact_assign`: `{id, assignee?}` — omit or null to clear.
  Input schema mirrors `artifact_comment`'s shape; errors follow SPEC-0019.

### Service layer

* `store.AssignArtifact(ctx, id, assigneeUserID *int64, actor)` runs in one
  transaction: read the artifact, authorize (AS-2), resolve and check the
  target (AS-3), write, and return a change record
  `{previous, new, changed bool}` so the service layer emits only effective
  changes (AS-5).

## Events

* `internal/outboundhook` builds `artifact.created` today from an `eventData`
  struct; add `assignee` there, omitted when nil.
* New `artifact.assigned` encoding beside it, reusing the envelope, signer and
  the EV-1 registry entry added to SPEC-0016.
* Delivery, signing and kind filtering are unchanged (SPEC-0012, SPEC-0023).

## UI

* Artifact page: assignee chip beside the provenance line (display handle).
* Bin: `assignee=<username>` filter and an "assigned to me" entry that applies
  the viewer's own username; both index-backed (AS-9).

## Golden Tests

* `artifact_created_assigned.golden.json` — create with `assignee=joestump`.
* `artifact_assigned.golden.json` — reassignment U → V.
* Existing `artifact_created_rest.golden.json` and
  `artifact_created_handoff.golden.json` MUST remain byte-identical; the
  handoff golden gains nothing (it has no assignee), which is the additivity
  proof AS-10 pins.

## Implementation Plan

PRs in dependency order, each sized to one ~1-hour agent run — one concern,
migration separate from feature work. Each PR names the requirements it
implements.

| # | PR | Contents | Size |
|---|----|----------|------|
| 1 | `feat/441-assignee-migration` | `0036_artifact_assignee.sql` (column + partial index) and nothing else. AS-9. | S |
| 2 | `feat/441-assignee-store` | Store: assignee on the artifact aggregate, `AssignArtifact` with authz + reader check + change detection, create-path wiring. AS-1, AS-2, AS-3, AS-4 (store half). | M |
| 3 | `feat/441-assignee-rest` | REST: create parameter, `PUT /v1/artifacts/{id}/assignee`, artifact JSON field, integration tests. AS-4, AS-7. | S |
| 4 | `feat/441-assignee-events` | `assignee` in `artifact.created`, `artifact.assigned` in the EV-1 registry and the emitter, both goldens, existing goldens untouched. AS-6, AS-10. | M |
| 5 | `feat/441-assignee-mcp` | `artifact_create`/`bundle_create` param, `artifact_assign` tool, schema + consent scope wiring, tests. AS-4, AS-7. | S |
| 6 | `feat/441-assignee-ui` | Artifact page chip, Bin filter, assigned-to-me view, web tests. AS-8. | M |
| 7 | `docs/441-assignee-docs` | `website/docs/guides/agent-handoffs.md` and the tags page: assignee joins the convention (tags stay hints); ADR-0032 flips to `accepted`; cross-link switchboard#558. | S |

Dependency notes: 2 needs 1; 3, 4, 5 need 2; 6 needs 3; 7 last. PR 4 is the
one reviewers should read against the goldens — it is where the additive
contract is enforced.
