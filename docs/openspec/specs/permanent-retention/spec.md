---
status: accepted
date: 2026-09-22
implements: [ADR-0026]
requires: [SPEC-0002, SPEC-0009]
related: [SPEC-0007, SPEC-0008, SPEC-0012, SPEC-0014, SPEC-0016, SPEC-0017, SPEC-0019, SPEC-0023]
---

# SPEC-0020: Opt-In Permanent Retention

## Overview

This capability lets an artifact's owner mark it **permanent**: it never expires, its id
never changes, its checksum is published, and removing it leaves an audit tombstone.
Every artifact is still ephemeral by default, and the feature itself is off until the
operator enables it. The operator may bound it per artifact, per user and per team; a
quota the operator does not set imposes no limit.

It realizes ADR-0026, which amends ADR-0007. It builds on SPEC-0002 (artifact core and
content addressing) and SPEC-0009 (provenance, link access and retention), and it amends
these requirements:

* SPEC-0009 REQ "Id Rotation as Revoke-a-Leaked-Link": rotation is refused while an
  artifact is permanent (REQ-8).
* SPEC-0009 REQ "Default 7-Day TTL, Owner-Adjustable, Visible Countdown": no-expiry is a
  retention mode, not a TTL value (REQ-1).
* SPEC-0009 REQ "Uniform 404 / No Enumeration": an id that was ever permanent resolves to a
  `410` tombstone (REQ-9).
* SPEC-0009 REQ "Object-Storage Lifecycle Backstop": no age rule may apply to committed
  blobs (REQ-13).
* SPEC-0007 REQ "Exactly Three Consent Scopes": adds the opt-in `retention:write` scope
  (REQ-6).
* SPEC-0008 REQ "Command Surface and REST-Client Boundary": adds `cairn retain`,
  `cairn release` and `cairn retention` (REQ-11).

This spec also leans on these records, linked as front-matter edges: ADR-0022/SPEC-0016
(the event envelope), ADR-0023/SPEC-0017 (redaction at ingest), ADR-0025/SPEC-0019
(validation error details) and ADR-0029/SPEC-0023 (teams). It ships after the release that
enforces `private` and re-identifies existing private artifacts (SPEC-0023 REQ
"Re-Identifying Existing Private Artifacts"), so no permanent id is ever changed by that step.

"Pin" already means an image-region annotation in Cairn (ADR-0006, `pin_count`). Nothing
in this capability uses that word.

## Requirements

### Requirement: REQ-1 Retention Mode and the Ephemeral Default

Every artifact MUST have exactly one retention mode: `ephemeral` or `permanent`. Every
artifact MUST be created `ephemeral`. No create surface MAY offer a retention input.

Every artifact read (REST, MCP `artifact_read`, and the Bin listing) MUST return a
`retention` object. It MUST contain `mode`, and MUST add `checksum`, `retained_at` and
`ever_retained` when they apply.

A permanent artifact MUST be returned with `expires_at` null, `expires_in` set to `never`
and `expires_in_seconds` null. The internal expiry sentinel (see design) MUST NOT appear on
any surface.

No-expiry MUST NOT be reachable as a TTL value. `PATCH /v1/artifacts/{id}/ttl` and
`X-Cairn-Ttl-Seconds` keep their existing bounds. On a permanent artifact, the TTL endpoint
MUST return `409 conflict` with reason `artifact_is_permanent`.

#### Scenario: New artifacts are ephemeral

- **WHEN** any surface creates an artifact
- **THEN** its `retention.mode` MUST be `ephemeral` and its `expires_at` MUST be the default or requested bounded TTL

#### Scenario: No create surface offers retention

- **WHEN** a client inspects the `artifact_create` and `bundle_create` tool schemas, and the REST create headers
- **THEN** none of them MUST offer a retention input, so retention can only ever change through retain

#### Scenario: Permanent read shape

- **WHEN** a client reads a permanent artifact
- **THEN** the response MUST carry `retention.mode = "permanent"`, the retained checksum, `retained_at`, `expires_at: null`, and `expires_in: "never"`

#### Scenario: TTL change on a permanent artifact

- **WHEN** the owner sends `PATCH /v1/artifacts/{id}/ttl` for a permanent artifact
- **THEN** the server MUST return `409 conflict` with reason `artifact_is_permanent` and change nothing

### Requirement: REQ-2 Operator Enablement

Permanent retention MUST be disabled unless the operator enables it
(`CAIRN_PERMANENT_RETENTION=true`). While it is disabled, every retain MUST be refused with
`403 forbidden` and reason `retention_disabled`.

Disabling the feature MUST NOT change the mode of any artifact that is already permanent.
Release and delete MUST keep working on those artifacts.

#### Scenario: Disabled by default

- **WHEN** an instance starts with no retention configuration and an owner retains an artifact
- **THEN** the server MUST refuse with `403` and reason `retention_disabled`, and the artifact MUST stay ephemeral

#### Scenario: Disabling never releases

- **WHEN** the operator disables retention while 3 artifacts are permanent, and the reaper then runs past their original expiry
- **THEN** all 3 MUST still be permanent and readable

### Requirement: REQ-3 Eligibility

Only single-body share types and bundles MAY be retained. A retain of a trace or a webhook
endpoint MUST be refused with `400 validation_failed` and reason
`share_type_not_retainable`. An unknown, expired or unreachable id MUST return the uniform
`404`.

#### Scenario: Markdown and bundles are eligible

- **WHEN** the owner retains a markdown artifact, and separately a bundle
- **THEN** both MUST become permanent

#### Scenario: Traces and webhooks are refused

- **WHEN** the owner retains a trace or a webhook endpoint
- **THEN** the server MUST refuse with reason `share_type_not_retainable` and change nothing

### Requirement: REQ-4 The Retained Checksum

Retaining MUST fix a checksum on the artifact:

- for a single-body artifact, the body's SHA-256 (`body_sha256`, ADR-0008);
- for a bundle, the lowercase hex SHA-256 of the manifest: one line per member, in ordinal
  order, each `<member sha256>  <member name>\n` (two spaces; the `sha256sum` output
  format).

The checksum MUST be persisted at retain time. It MUST be returned as
`retention.checksum`, shown in the web panel, carried on every retention event, and
recorded on any tombstone. It MUST NOT change while the artifact exists.

#### Scenario: Single-body checksum

- **WHEN** a markdown artifact is retained
- **THEN** `retention.checksum` MUST equal the artifact's existing `checksum`

#### Scenario: Bundle checksum is reproducible

- **WHEN** a three-member bundle is retained, and a reader downloads the members and hashes the manifest lines in ordinal order
- **THEN** the reader's SHA-256 MUST equal `retention.checksum`

### Requirement: REQ-5 Size Limit and Quotas

A retain MUST be refused, with no state change, when any of these holds:

- the artifact's logical size (for a bundle, the sum of its members) exceeds
  `CAIRN_PERMANENT_MAX_ARTIFACT_BYTES`. Reason `retention_size_exceeded`, `400`.
- the owner has a count quota and its count of permanent artifacts would exceed it.
  Reason `retention_quota_exceeded`, `409`.
- the owner has a byte quota and its total permanent bytes would exceed it. Reason
  `retention_quota_exceeded`, `409`.

Quotas are optional. An owner's quota for a dimension is its per-owner override if one is
set, otherwise the instance quota for its kind (`CAIRN_PERMANENT_USER_MAX_COUNT`,
`CAIRN_PERMANENT_USER_MAX_BYTES`, `CAIRN_PERMANENT_TEAM_MAX_COUNT`,
`CAIRN_PERMANENT_TEAM_MAX_BYTES`). When neither is set, that dimension MUST NOT limit
retains. No quota MAY be applied by default.

The owner is the artifact's owner: a user or, under ADR-0029, a team (`owner_user_id` XOR
`owner_team_id`). A team-owned artifact MUST count against the team's quota and never
against the member who retained it. Moving a permanent artifact from one owner to another
(for example, into a team under ADR-0029) MUST re-charge it to the new owner, in the same
transaction as the move. The move MUST fail with `retention_quota_exceeded` if the new
owner has a quota and the move would exceed it.

The quota check and the mode change MUST be atomic per owner: two concurrent retains MUST
NOT both succeed when only one fits. Lowering a quota below current usage MUST NOT release
anything; it MUST only refuse further retains.

#### Scenario: Over the count quota

- **GIVEN** the operator set `CAIRN_PERMANENT_USER_MAX_COUNT=100`
- **WHEN** a user at 100 of 100 permanent artifacts retains another
- **THEN** the server MUST refuse with `409` and reason `retention_quota_exceeded`, with details naming `count`, `used` and `limit`

#### Scenario: No quota configured

- **GIVEN** retention is enabled and no per-user quota or override is set
- **WHEN** a user with 5,000 permanent artifacts totalling 20 GiB retains another eligible artifact
- **THEN** the retain MUST succeed, and `GET /v1/retention/usage` MUST report `limit: null` for both dimensions

#### Scenario: Concurrent retains at the boundary

- **WHEN** a user whose configured count quota has one slot left retains two artifacts concurrently
- **THEN** exactly one MUST succeed and the other MUST be refused with `retention_quota_exceeded`

#### Scenario: Team quota, not member quota

- **WHEN** a team member retains a team-owned artifact
- **THEN** the team's usage MUST increase and the member's personal usage MUST NOT

#### Scenario: Moving into a full team

- **WHEN** a user moves their permanent artifact into a team that is at its configured count quota
- **THEN** the move MUST fail with `retention_quota_exceeded`, and the artifact MUST stay user-owned and permanent

#### Scenario: Oversize artifact

- **WHEN** the owner retains an artifact larger than the per-artifact maximum
- **THEN** the server MUST refuse with reason `retention_size_exceeded`

### Requirement: REQ-6 Who May Retain and Release

Retain and release MUST be owner policy actions. For a user-owned artifact that means the
owner. For a team-owned artifact it means a team admin, one of ADR-0029's `owner` or
`admin` roles. A plain team `member` MUST NOT retain or release team artifacts, even ones
they created. An authenticated non-owner MUST receive the distinct `403` of SPEC-0009, not
a `404`.

**Release MUST be human-only.** It requires `sharing:manage`, which no agent grant carries.

**Retain by an agent** MUST require both of these:

1. the operator has enabled agent retention (`CAIRN_PERMANENT_AGENT_RETAIN=true`, default
   false);
2. the token carries the opt-in `retention:write` scope, which a human MUST have granted
   explicitly, on the OAuth consent screen or when minting the PAT.

`retention:write` MUST NOT be part of the default agent grant. It MUST NOT confer release,
delete, TTL, visibility or rotation. This amends SPEC-0007 REQ "Exactly Three Consent
Scopes": the three default scopes are unchanged, and one opt-in scope is added.

#### Scenario: Agent without the scope

- **WHEN** an agent token with the default grant calls `artifact_retain`
- **THEN** the call MUST fail with a scope error naming `retention:write`, and the artifact MUST stay ephemeral

#### Scenario: Agent with the scope, gate off

- **WHEN** an agent token carrying `retention:write` retains an artifact while `CAIRN_PERMANENT_AGENT_RETAIN` is false
- **THEN** the server MUST refuse with `403` and reason `agent_retention_disabled`

#### Scenario: Agent cannot release

- **WHEN** any agent token, including one with `retention:write`, calls release
- **THEN** the server MUST refuse with `403` and the artifact MUST stay permanent

#### Scenario: Team member without admin role

- **WHEN** a team `member` retains a team-owned artifact they created
- **THEN** the server MUST return `403` and the artifact MUST stay ephemeral

#### Scenario: Non-owner human

- **WHEN** an authenticated human who does not own the artifact retains it
- **THEN** the server MUST return the distinct not-owner `403` and change nothing

### Requirement: REQ-7 Retain and Release Semantics

Retain MUST be idempotent. Retaining an artifact that is already permanent MUST return
`200` with the unchanged artifact, and MUST NOT emit an event, write an audit row or
consume quota.

Retaining MUST re-run the ingest scanner of ADR-0023 over the artifact's stored bodies. On
a finding, it MUST refuse with the scanner's rejection reason. This keeps an artifact
created before redaction shipped from being made permanent with a secret inside it.

Release MUST return a permanent artifact to `ephemeral` with
`expires_at = now + ttl_seconds`, bounded like the TTL endpoint and defaulting to the
instance default TTL. Release MUST set `ever_retained = true`. Releasing an artifact that
is not permanent MUST return `409 conflict` with reason `not_permanent`.

#### Scenario: Retain twice

- **WHEN** the owner retains the same artifact twice
- **THEN** the second call MUST return `200`, and usage, events and the audit log MUST reflect one retain

#### Scenario: Scanner finding blocks retain

- **WHEN** the owner retains a pre-redaction artifact whose body contains a credential the ADR-0023 scanner detects
- **THEN** the retain MUST be refused and the artifact MUST stay ephemeral

#### Scenario: Release with a TTL

- **WHEN** the owner releases a permanent artifact with `ttl_seconds = 86400`
- **THEN** its mode MUST be `ephemeral`, `expires_at` MUST be about 24 hours from now, and `ever_retained` MUST be true

### Requirement: REQ-8 Id Rotation Is Refused While Permanent

`POST /v1/artifacts/{id}/rotate` on a permanent artifact MUST return `409 conflict` with
reason `artifact_is_permanent`, and the id MUST NOT change.

Rotating an artifact that is ephemeral but was ever retained MUST succeed. It MUST write a
tombstone of kind `rotated` at the old id (REQ-9). That tombstone MUST NOT reference the new
id.

#### Scenario: Rotate a permanent artifact

- **WHEN** the owner rotates a permanent artifact
- **THEN** the server MUST return `409` with reason `artifact_is_permanent`, and the old link MUST still resolve

#### Scenario: Release then rotate

- **WHEN** the owner releases a permanent artifact and then rotates it
- **THEN** the new id MUST resolve to the artifact, and the old id MUST return `410` with a `rotated` tombstone carrying the retained checksum and no reference to the new id

### Requirement: REQ-9 Tombstones

When an artifact whose `ever_retained` is true, or which is currently permanent, is
removed, Cairn MUST write a tombstone in the same transaction as the removal. Removal
means owner delete, rotation, or expiry after release. The tombstone MUST record:

- the id and the share type;
- the retained checksum;
- the creation, last-retain and removal times;
- the removing actor and channel (`system` for expiry);
- the kind: `deleted`, `rotated` or `expired`;
- an optional owner-supplied reason of at most 280 characters.

It MUST NOT record the title, tags or body.

A request for a tombstoned id MUST return `410 Gone` with the tombstone, on REST, on MCP,
and on the web shell. This is the only exception to the uniform 404. An id that was never
retained MUST keep returning the uniform 404 after it expires or is deleted.

A tombstoned id MUST never be minted again. Tombstones MUST NOT expire. Only the operator
command in REQ-12 MAY remove one.

Deleting an ever-retained artifact MUST remove its body references, its metadata and its
annotations exactly as a delete of any other artifact does.

#### Scenario: Delete a permanent artifact

- **WHEN** the owner deletes a permanent artifact with reason "superseded by v2"
- **THEN** `GET /v1/artifacts/{id}` MUST return `410` with a tombstone of kind `deleted` carrying the retained checksum and the reason, and the body and annotations MUST be gone

#### Scenario: Never-retained stays uniform

- **WHEN** an artifact that was never permanent is deleted, and another expires
- **THEN** both ids MUST return the uniform `404`, byte-identical to an id that never existed

#### Scenario: Expiry after release

- **WHEN** a released artifact reaches its `expires_at` and the reaper removes it
- **THEN** its id MUST return `410` with a tombstone of kind `expired` and removing actor `system`

#### Scenario: Tombstoned id never reused

- **WHEN** the id generator mints an id that collides with a tombstone
- **THEN** it MUST discard the id and mint another

### Requirement: REQ-10 Annotations Are Retained

Comments and reactions on a permanent artifact MUST persist for as long as the artifact
does. SPEC-0006's edit and soft-delete rules are unchanged. Annotation writes on a
permanent artifact MUST follow the same authentication rules as on any other artifact.

#### Scenario: Annotations outlive the original TTL

- **WHEN** an artifact with 2 comments and 3 reactions is retained and a year passes
- **THEN** all 2 comments and 3 reactions MUST still be readable on it

### Requirement: REQ-11 Surfaces: REST, MCP, CLI and Web

The capability MUST be exposed on every surface (ADR-0003 parity), in these shapes:

- **REST.** `POST /v1/artifacts/{id}/retain`, `POST /v1/artifacts/{id}/release` (optional
  `ttl_seconds`), `DELETE /v1/artifacts/{id}` (accepting an optional JSON body with
  `reason`), and `GET /v1/retention/usage` (optional `team`).
- **MCP.** An `artifact_retain` tool taking an id or `mcp://cairn/` handle, gated by
  REQ-6. There MUST NOT be an MCP release or delete tool.
- **CLI.** `cairn retain <id>`, `cairn release <id> [--ttl]` and `cairn retention [--team]`,
  each honoring `--json`. This amends SPEC-0008's closed command list.
- **Web.** The owner panel MUST offer Retain and Release controls, show quota usage, and
  show the `∞ permanent` badge with the checksum and a copy control. The tombstone MUST
  render as a page with status `410`.

#### Scenario: CLI retain prints the checksum

- **WHEN** an owner runs `cairn retain abc123`
- **THEN** the CLI MUST print the id, `permanent`, and the retained checksum, and with `--json` MUST print the server's artifact object

#### Scenario: Usage endpoint

- **WHEN** an owner calls `GET /v1/retention/usage`
- **THEN** the response MUST give `enabled`, `agent_retain`, `max_artifact_bytes`, and the owner's `count` and `bytes` as `used` and `limit`, where `limit` is `null` when no quota applies

#### Scenario: Usage for a team the caller is not in

- **WHEN** a user requests usage for a team they are not a member of
- **THEN** the server MUST return the uniform `404`

### Requirement: REQ-12 Operator Commands and Audit

`cairnd` MUST provide operator subcommands. Running `cairnd` with no subcommand MUST still
start the server.

- `cairnd retention quota get|set|unset --user <id> | --team <id> [--count N] [--bytes SIZE]`
  manages per-owner overrides of the instance quotas. An override MAY set a limit where no
  instance quota exists, or `unlimited` where one does; `unset` returns the owner to the
  instance quota, or to no quota if none is set.
- `cairnd retention release (--owner <id> | --all) --ttl <dur> --reason <text>` bulk-releases
  permanent artifacts. It MUST emit one `artifact.released` event and one audit row per
  artifact.
- `cairnd retention purge-tombstone <id> --reason <text>` removes a tombstone for a legal
  takedown. After the purge the id MUST return the uniform 404 and MUST still never be
  reused.

Every retain, release, delete, rotation, expiry-with-tombstone, operator release and purge
MUST append a row to the retention audit log. Each row MUST record the actor, the
on-behalf-of harness, the channel, the checksum and the reason. The audit log MUST survive
the artifact's removal.

#### Scenario: Bulk release is audited

- **WHEN** the operator bulk-releases one user's 4 permanent artifacts
- **THEN** 4 audit rows with action `operator_release` and 4 `artifact.released` events MUST exist, and all 4 artifacts MUST be ephemeral with the given TTL

#### Scenario: Purge keeps the id retired

- **WHEN** the operator purges a tombstone
- **THEN** the id MUST return the uniform 404, an `operator_purge` audit row MUST exist, and the generator MUST never mint that id

### Requirement: REQ-13 Storage Lifecycle and the Reaper

The reaper MUST NOT delete a permanent artifact. The object-storage backstop MUST NOT
apply any age-based rule to committed blobs; only the transient `staging/` prefix MAY carry
an age rule. A blob referenced by a permanent artifact MUST survive every reaper phase.

#### Scenario: Reaper skips permanent

- **WHEN** the reaper runs after a permanent artifact's original `expires_at` has passed
- **THEN** the artifact, its blob and its annotations MUST be intact

#### Scenario: Shared blob survives

- **WHEN** a permanent artifact and an ephemeral artifact share a body blob and the ephemeral one expires
- **THEN** the blob MUST survive because the permanent artifact still references it

### Requirement: REQ-14 Retention Events

Cairn MUST emit these event kinds on the ADR-0017 envelope, as extended by ADR-0022. They
are entries in SPEC-0016 EV-1's closed kind registry:

- `artifact.retained`, on a successful non-idempotent retain;
- `artifact.released`, on release and on each operator release;
- `artifact.deleted`, on removal of an ever-retained artifact, whether by delete or
  rotation.

Each MUST be routed as ADR-0022 routes every kind: by the artifact's owning workspace, to
that workspace's subscriptions under ADR-0029 whose event-type filter admits the kind. There
is no instance-wide target (ADR-0029 removes `CAIRN_OUTBOUND_WEBHOOK_URLS`), and this
capability MUST NOT add a fan-out path of its own. Each MUST carry `data.checksum` and
`data.retention`, plus ADR-0022's common actor fields. `artifact.deleted` MUST carry `data.tombstone` with its
kind and removal time. It MUST NOT carry `title` or `tags`.

This capability MUST add nothing to the `artifact.created` payload: every new artifact is
ephemeral, so there is nothing retention-specific to carry at creation.

#### Scenario: Retained event carries the checksum

- **WHEN** the owner has a subscription whose filter admits `artifact.retained`, and retains an artifact
- **THEN** one delivery to that subscription MUST carry `kind: "artifact.retained"`, `data.retention: "permanent"` and `data.checksum` equal to the retained checksum, signed with that subscription's secret

#### Scenario: Filtered out by the subscription

- **WHEN** the owner's only subscription filters to `artifact.created`
- **THEN** retain, release and delete MUST send nothing to it

#### Scenario: Created payload unchanged

- **WHEN** an artifact is created after this capability ships
- **THEN** its `artifact.created` body MUST be byte-identical to what the same build produced before this capability, with no `checksum` or `retention` key

### Requirement: REQ-15 Metrics

Cairn MUST add these metrics to SPEC-0014's endpoint:

- `cairn_permanent_artifacts{share_type}` (gauge);
- `cairn_permanent_bytes{share_type}` (gauge);
- `cairn_retention_actions_total{action}` (counter);
- `cairn_tombstones` (gauge).

Per SPEC-0014 REQ-5, no metric MAY carry an owner, team, actor or artifact label.

#### Scenario: Gauges track retains

- **WHEN** an owner retains a 2 MiB markdown artifact
- **THEN** `cairn_permanent_artifacts{share_type="markdown"}` MUST increase by 1 and `cairn_permanent_bytes{share_type="markdown"}` by 2097152 at the next collection

### Requirement: Error Handling Standards

Retention failures that a caller must distinguish MUST be typed domain errors, rendered in
the ADR-0012 error envelope with a machine-readable `details.reason` (following
ADR-0025/SPEC-0019): `retention_disabled`, `agent_retention_disabled`,
`share_type_not_retainable`, `retention_size_exceeded`, `retention_quota_exceeded`,
`artifact_is_permanent` and `not_permanent`. A new `gone` code MUST map to `410`. Errors
MUST be wrapped with context at each layer boundary, logged with structured key-value
fields, and never swallowed.

#### Scenario: Quota refusal names the dimension

- **WHEN** a retain fails on the byte quota
- **THEN** the envelope MUST carry code `conflict`, reason `retention_quota_exceeded`, and details naming `bytes`, `used` and `limit`

### Requirement: Concurrency Safety

The quota check MUST serialize per owner, with a transaction-scoped advisory lock on the
owner key, so concurrent retains cannot oversubscribe a quota. The reaper's tombstone
writes MUST happen in the same transaction as its deletes. Every path MUST propagate
context cancellation. Tests covering these paths MUST run with race detection in CI.

#### Scenario: Reaper crash mid-batch

- **WHEN** the reaper is cancelled after selecting a batch that contains an ever-retained artifact
- **THEN** either both the delete and its tombstone commit, or neither does

### Requirement: Database Operation Standards

Retain, release, delete-with-tombstone, rotate-with-tombstone and operator release MUST
each execute in a single transaction covering the mode change, the checksum, the audit row
and the tombstone. All queries MUST be parameterized. Connections MUST be returned to the
pool with bounded timeouts.

#### Scenario: Atomic retain

- **WHEN** a retain fails after its quota check but before commit
- **THEN** the artifact MUST remain ephemeral, and no audit row or event MUST exist

## Endpoint Table

| Endpoint | Method | Purpose | Auth |
|---|---|---|---|
| `/v1/artifacts/{id}/retain` | POST | Make permanent | Required: owner (or team admin); agents need `retention:write` plus the operator gate |
| `/v1/artifacts/{id}/release` | POST | Return to ephemeral with a bounded TTL | Required: owner (or team admin); `sharing:manage`, human-only |
| `/v1/artifacts/{id}` | DELETE | Delete; tombstones when ever-retained | Required: owner, human-only (unchanged) |
| `/v1/retention/usage` | GET | Caller's (or a team's) usage and limits | Required |
| `/v1/artifacts/{id}` | GET | Read; `410` + tombstone for a tombstoned id | **Public**: the capability URL is the read token (ADR-0007); a tombstone discloses only to a link holder |
| `/{id}` | GET | Web shell; `410` tombstone page | **Public**: same capability-URL rationale |

## Security Requirements

### Requirement: Authentication & Authorization

Retain, release, delete and usage MUST require authentication. Retain and release MUST
require ownership (or the team admin role). Release and delete MUST refuse every agent
token. Agent retain MUST require `retention:write` and the operator gate (REQ-6). Reads of
tombstones follow the capability-URL rule.

#### Scenario: Unauthenticated retain

- **WHEN** an unauthenticated client posts to `/v1/artifacts/{id}/retain`
- **THEN** the server MUST return `401` and change nothing

### Requirement: Rate Limiting

The retention endpoints MUST sit behind the existing per-IP limiter. Exceeding it MUST
return `429` with `Retry-After`. Tombstone reads MUST share the resolution rate limit that
defends against id scanning (SPEC-0009).

#### Scenario: Burst of retains

- **WHEN** a client exceeds the configured rate on `/retain`
- **THEN** the server MUST return `429` with `Retry-After` and process nothing

### Requirement: Security Headers

Retention responses and the tombstone page MUST carry the existing strict CSP,
`X-Content-Type-Options: nosniff`, `Referrer-Policy`, `X-Frame-Options: DENY` and (over
HTTPS) HSTS. None of them may weaken the viewer isolation rules (SPEC-0003).

#### Scenario: Tombstone page headers

- **WHEN** the web shell renders a `410` tombstone page
- **THEN** the response MUST carry the same security headers as a live artifact page

### Requirement: Request Body Size Limits

Retain, release and delete bodies MUST be capped at 4 KiB before buffering. Oversize
bodies MUST be rejected with `413`. A reason longer than 280 characters MUST be rejected
with `400`.

#### Scenario: Oversize reason

- **WHEN** a delete carries a 1,000-character reason
- **THEN** the server MUST reject it with `400` and delete nothing

### Requirement: CSRF Protection

Retain, release and delete from a browser session MUST pass the existing CSRF seam.
Token-authenticated calls are exempt.

#### Scenario: Cross-site retain

- **WHEN** a session-authenticated retain arrives without a valid CSRF token
- **THEN** the server MUST reject it and change nothing

### Requirement: Redirect & SSRF Validation

After a web retain, release or delete, the web shell MUST redirect only to an internal
path: the artifact, the tombstone or the Bin. It MUST NOT honor a user-supplied absolute
URL. This capability performs no server-side fetch of user URLs.

#### Scenario: External redirect target

- **WHEN** a web delete supplies an external redirect target
- **THEN** the server MUST ignore it and redirect to the tombstone page

## Accessibility Requirements

### Requirement: WCAG 2.1 AA and Semantics

The Retain and Release controls, the quota readout, the permanent badge and the tombstone
page MUST meet WCAG 2.1 AA. The tombstone page MUST keep the shell's landmarks (`banner`,
`main`, `contentinfo`).

#### Scenario: Tombstone landmarks

- **WHEN** an assistive-technology user opens a tombstoned link
- **THEN** the page MUST expose `main` containing the tombstone's kind, date and checksum as text

### Requirement: Icon-Only Controls

The checksum copy control and the `∞` badge MUST carry an `aria-label`: "Copy checksum" and
"Permanent: never expires".

#### Scenario: Screen reader reads the badge

- **WHEN** a screen reader reaches the permanent badge
- **THEN** it MUST announce "Permanent: never expires" rather than the glyph

### Requirement: Dynamic Content Regions

Retain and release results, including quota and scanner refusals, MUST be announced
through an `aria-live="polite"` region. A refusal that leaves the artifact unchanged MUST
be announced assertively.

#### Scenario: Quota refusal announced

- **WHEN** a web retain is refused for quota
- **THEN** the refusal text MUST be announced by an `aria-live="assertive"` region

### Requirement: Keyboard Navigation & Focus Management

Every retention control MUST be reachable and operable by keyboard. The delete dialog,
with its reason field, MUST trap focus while open, focus its first field on open, close on
Escape, and return focus to the control that opened it.

#### Scenario: Delete dialog focus

- **WHEN** a keyboard user opens the delete dialog on a permanent artifact and presses Escape
- **THEN** the dialog MUST close and focus MUST return to the Delete control
