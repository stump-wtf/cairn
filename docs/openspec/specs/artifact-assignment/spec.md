---
status: draft
date: 2026-10-02
implements: [ADR-0032]
requires: [SPEC-0002, SPEC-0012, SPEC-0023]
related: [SPEC-0007, SPEC-0016]
---

# SPEC-0025: Artifact Assignment

## Graph Edges

- **Implements:** **ADR-0032**: a native, reassignable assignee on artifacts.
- **Requires:** **SPEC-0002**: the artifact aggregate the assignee attaches to,
  and the tag rules that keep tags routing hints.
- **Requires:** **SPEC-0012**: the outbound envelope, signing and delivery the
  new event rides on.
- **Requires:** **SPEC-0023**: `authorizeRead`, the owner model, teams and
  roles the assignment authorization reuses.
- **Related:** **SPEC-0007** (the MCP surface gaining `artifact_assign`),
  **SPEC-0016** (the lifecycle-event registry the new kind joins).

## Overview

An artifact MAY carry one **assignee**: a nullable reference to a user, set at
creation and changeable or clearable afterwards by the users who may already
change the artifact's metadata. The assignee is announced by two signed events:
`assignee` inside `artifact.created`'s `data`, and a new `artifact.assigned`
kind on every effective change. Tags remain client-asserted routing hints
(ADR-0018); the assignee is the validated, reassignable identity field.

Requirement IDs (`AS-n`) are stable. Plans and code cite them as
`SPEC-0025 AS-n`.

## Requirements

### Requirement: AS-1 Assignee Model and Invariants

An artifact MUST carry at most one assignee, stored as `assignee_user_id` on
the artifact: nullable, a foreign key to `users`, never an owner column. The
assignee MUST NOT change the artifact's owner, visibility, expiry or access
policy; it is a named worker, not a co-owner. An assignee MUST be a user who
passes `authorizeRead` for the artifact at the moment of assignment. A stale
assignment — one whose user can no longer read the artifact after a visibility
or membership change — MUST NOT be retroactively cleared: read authorization
continues to gate reads (SPEC-0023), and the assignment's event remains the
record of the intent at the moment it was made.

#### Scenario: Assignment survives a visibility tightening

- **GIVEN** an artifact assigned to user V while visibility is `link`
- **WHEN** the owner tightens visibility to `private`
- **THEN** `assignee_user_id` is unchanged, and V's reads now fail
  `authorizeRead` exactly as any other non-reader's

#### Scenario: Assignee is not an owner

- **WHEN** V is assigned to an artifact V does not own
- **THEN** V gains no ability to delete, rotate, re-share or expiry-change it
  beyond what `authorizeRead` already grants

#### Scenario: One assignee

- **WHEN** an assignment replaces a previous assignee
- **THEN** the artifact has exactly one `assignee_user_id` afterwards and the
  previous one survives only in the emitted event

### Requirement: AS-2 Who May Assign

A set, change or clear MUST be refused unless the actor may change the
artifact's metadata:

- a personal artifact: the owning user, including an agent acting for them;
- a team artifact: any current, unsuspended member of the owning team.

Assignment is metadata, not sharing policy: an agent MAY assign within its
principal's rights, where `sharing:manage` (visibility, SPEC-0023 REQ
"Visibility Values") stays closed to agents. A refused request MUST answer
`403` and change nothing.

#### Scenario: Member reassigns a team handoff

- **WHEN** member B of team T calls `artifact_assign` on a T artifact
- **THEN** the assignee is replaced and one `artifact.assigned` event is emitted

#### Scenario: Non-member cannot assign

- **WHEN** a signed-in user who is not the owner calls `artifact_assign` on a
  `private` artifact
- **THEN** the response is `403` and the assignee is unchanged

#### Scenario: Agent assigns for its principal

- **WHEN** an agent authorized by the owner calls `artifact_assign` on the
  owner's `link` artifact
- **THEN** the assignment succeeds; the event's `actor_id` and `actor_kind`
  record the principal and `agent`

### Requirement: AS-3 Assignee Must Be a Reader

The assignee named in a create or assignment request MUST resolve to an
existing user who passes `authorizeRead` for the artifact. An unknown username
MUST answer `400 assignee_unknown`. A user who exists but cannot read the
artifact MUST answer `400 assignee_unreadable` for a `private` artifact, and
for a `team` artifact a non-member MUST answer the same. For `link` visibility
any signed-in user qualifies. The answers MUST be identical in shape and timing
to the validation-error standards (SPEC-0002 REQ "Error Handling Standards").

#### Scenario: Unknown username refused

- **WHEN** a create carries `assignee=nosuchuser`
- **THEN** the request fails with `400 assignee_unknown` and nothing is persisted

#### Scenario: Private artifact, other user refused

- **WHEN** the owner assigns user V to the owner's `private` artifact
- **THEN** the request fails with `400 assignee_unreadable`

#### Scenario: Team artifact, non-member refused

- **WHEN** the owner of a `team` artifact assigns a user outside the team
- **THEN** the request fails with `400 assignee_unreadable`

#### Scenario: Link artifact, any signed-in user accepted

- **WHEN** the owner assigns any existing user to a `link` artifact
- **THEN** the assignment succeeds and the user can read the artifact

### Requirement: AS-4 Setting at Create

Every create surface MAY accept an assignee at creation: MCP `artifact_create`
and `bundle_create` gain optional `assignee` (a username); REST create accepts
`assignee=<username>` as a query parameter, `X-Cairn-Assignee` header, or
multipart field. Validation (AS-3) runs before persistence, inside the create
transaction, so a rejected create persists nothing and emits no
`artifact.created`. The create response MUST return the assignee's username
when set, and nothing when not.

#### Scenario: Create with assignee

- **WHEN** an agent calls `artifact_create` with `assignee=joestump`
- **THEN** the artifact is created with that assignee and the returned artifact
  names them

#### Scenario: Rejected assignee persists nothing

- **WHEN** a create carries `assignee_unreadable`-quality input
- **THEN** no artifact row, blob or event exists afterwards

#### Scenario: Omitted assignee changes nothing

- **WHEN** a create carries no assignee
- **THEN** the artifact is created exactly as today, with no assignee key in
  the response or the event

### Requirement: AS-5 Changing and Clearing

`artifact_assign` (MCP) and `PUT /v1/artifacts/{id}/assignee` (REST, body
`{"assignee": "<username>"}` or `{"assignee": null}`) MUST set or clear the
assignee in one transaction. An assignment to the user already assigned MUST
succeed idempotently with `200` and emit nothing. Every effective change MUST
emit exactly one `artifact.assigned` event after the transaction commits, from
the service layer that owns the change, through the same choke point as every
lifecycle event (SPEC-0016 REQ "EV-2 Emission Points"). Emission MUST NOT
block, fail or alter the response.

#### Scenario: Reassign emits once

- **WHEN** the assignee moves from U to V
- **THEN** exactly one `artifact.assigned` event is emitted, carrying the new
  and previous values

#### Scenario: Clear emits with a null new value

- **WHEN** the assignee is cleared
- **THEN** one `artifact.assigned` event is emitted whose new assignee is
  absent (null), not an empty string

#### Scenario: Same-value write is silent

- **WHEN** `artifact_assign` names the current assignee
- **THEN** the response is `200` with the artifact unchanged and no event is
  emitted

#### Scenario: Rolled-back change emits nothing

- **WHEN** the assignment transaction fails after validation
- **THEN** no event is emitted and the previous assignee is intact

### Requirement: AS-6 Event Payloads

`artifact.created`'s `data` MUST gain an optional `assignee` — the assigned
user's display handle — subject to the additive rules of SPEC-0012 REQ "Event
Payload": omitted when unset, so an artifact without an assignee produces a
byte-identical event to today's. A new kind `artifact.assigned` MUST be added
to the SPEC-0016 EV-1 registry before it is emitted, and its `data` MUST carry:

1. the subject-artifact and principal fields every event carries (EV-3);
2. `assignee`: the assignee after the change (omitted when the change cleared
   it), the same field the router projects;
3. `assignment`: an object carrying `previous` — the assignee immediately
   before the change, omitted when there was none.

Both kinds MUST be delivered only to the owning workspace's subscriptions
(SPEC-0023 REQ "Events Go Only to the Artifact's Workspace"), signed per
SPEC-0012 REQ "Signed Delivery", and honoured by the subscription kind filter.

#### Scenario: Created event carries the assignee

- **WHEN** an artifact is created with `assignee=joestump`
- **THEN** `artifact.created`'s `data.assignee` is `"joestump"` and the
  signature covers it

#### Scenario: Unassigned create is byte-identical

- **WHEN** an artifact is created without an assignee
- **THEN** its `artifact.created` body contains no `assignee` key and is
  byte-identical to the pre-feature payload for the same field values

#### Scenario: Assigned event shape

- **WHEN** the assignee moves from U to V
- **THEN** the `artifact.assigned` body carries `data.assignee = "V"`,
  `data.assignment.previous = "U"`, and the standard envelope fields

#### Scenario: Kind filter applies

- **WHEN** a subscription filters kinds to `artifact.created` only
- **THEN** an `artifact.assigned` change is not delivered to it

### Requirement: AS-7 API and MCP Surface

REST: `PUT /v1/artifacts/{id}/assignee` requires authentication, CSRF, and the
AS-2 authorization check; it MUST answer with the artifact in the same shape as
a read, including the assignee's username when set. MCP: a new `artifact_assign`
tool takes the artifact id and an optional assignee (omitted or null clears),
and follows the MCP tool-surface and error standards (SPEC-0007, SPEC-0019).
Both surfaces MUST answer `404` — not `403` — for an artifact the caller cannot
read, per the uniform not-found rule.

#### Scenario: MCP round trip

- **WHEN** an agent calls `artifact_assign` with `assignee=joestump`, then
  `artifact_read`
- **THEN** the read shows the assignee, and a second `artifact_assign` with the
  same value is a silent idempotent success

#### Scenario: Unreadable artifact answers 404

- **WHEN** a user who cannot read an artifact calls `artifact_assign` on it
- **THEN** the response is `404`, identical to an unknown id

### Requirement: AS-8 UI

The artifact page MUST show the assignee when set — display handle, not email —
with who assigned and when from the event history where available. The Bin
MUST accept an `assignee=<username>` filter and an "assigned to me" view backed
by the AS-9 index, and MUST show assigned artifacts where they are shown today
plus the filter. The UI MUST NOT offer assignment to a user who would fail
AS-3, and MUST NOT offer it to actors who would fail AS-2.

#### Scenario: Assignee shown on the artifact

- **WHEN** a reader opens an assigned artifact
- **THEN** the assignee's display handle is visible on the page

#### Scenario: Bin filtered by assignee

- **WHEN** the owner lists the Bin with `assignee=joestump`
- **THEN** only artifacts currently assigned to that user are returned, and
  artifacts whose assignment was later cleared are not

#### Scenario: Assigned to me

- **WHEN** a user opens "assigned to me"
- **THEN** the Bin is filtered to artifacts assigned to them that they can
  still read

### Requirement: AS-9 Database Shape

One migration SHALL add `assignee_user_id BIGINT NULL REFERENCES users(id)` to
`artifacts` and a partial index `ON artifacts (assignee_user_id) WHERE
assignee_user_id IS NOT NULL` for the assigned-to-me path. All access MUST use
bound parameters (SPEC-0002 REQ "Database Operation Standards"), and the
assignee write MUST join the create or assignment transaction it belongs to.
The migration MUST NOT backfill or rewrite any existing row.

#### Scenario: Migration is additive

- **WHEN** the migration runs on a database with existing artifacts
- **THEN** every artifact has a null assignee and nothing else changes

#### Scenario: Index serves assigned-to-me

- **WHEN** the Bin is listed with an assignee filter
- **THEN** the query is index-backed on `assignee_user_id`

### Requirement: AS-10 Golden Tests

The outbound golden tests MUST pin the new shapes:

- an `artifact_created` golden with an assignee, whose `data.assignee` is
  present and signed;
- an `artifact_assigned` golden, carrying `assignee` and `assignment.previous`;
- the existing unassigned `artifact_created` goldens MUST remain byte-identical.

A payload change that breaks either invariant MUST fail the suite.

#### Scenario: Goldens guard additivity

- **WHEN** the emitter changes and an unassigned artifact's payload differs
  from its golden
- **THEN** the test suite fails

#### Scenario: Goldens pin the assigned event

- **WHEN** the emitter encodes `artifact.assigned` with previous `U` and new `V`
- **THEN** the output matches `artifact_assigned.golden.json` field for field
