---
status: proposed
date: 2026-10-02
decision-makers: [joestump]
extends: [ADR-0018, ADR-0029]
related: [ADR-0022, ADR-0024, ADR-0027, ADR-0030]
---

# ADR-0032: Native Artifact Assignee — One Named User Per Artifact, Reassignable, Announced

## Context and Problem Statement

A Cairn handoff cannot name who should do it. The only identity signal a handoff
carries today is a tag convention, `agent:<login>`, which ADR-0018 makes
deliberately uninterpreted: tags are client-asserted metadata set once at
creation, validated only against a character grammar, and nothing checks that
the login named exists. Three costs follow (issue #441):

* there is no reassignment — a handoff sent to the wrong identity must be
  re-shared as a new artifact, and the new id strands the comment thread;
* no surface shows who owns a piece of work, so nobody can see who has a
  handoff without reading its tags;
* a router cannot trust the tag as identity, because nothing validated it.

The question: where does "who should do this" live, such that it is
reassignable, validated against real users, and visible to the Switchboard
router that turns handoffs into todos?

## Decision Drivers

* Reassignment must not strand the artifact's annotation stream — the comment
  thread is the review history of a handoff (ADR-0006).
* The value must be a *validated* reference to a real user, not another
  client-asserted string; routing decisions (Switchboard) depend on it.
* Outbound events are additive by contract (SPEC-0012 REQ "Event Payload");
  artifacts without the new field must produce byte-identical events.
* Tenancy (ADR-0029) already fixes who may touch an artifact: the owner for a
  personal artifact, the team for a team artifact. The assign rule must reuse
  that, not invent a parallel permission.
* Agents act through their principal's account (SPEC-0023 REQ "Static API
  Tokens Act as an Operator's User"); whatever we add must work for the
  agent-driven handoff flow, which is the point of the feature.

## Considered Options

* **(A) A native assignee on the artifact** — one nullable user reference,
  settable at create and changeable or clearable later, announced by events.
* **(B) An `assignee:` tag convention** — keep tags as the carrier and
  standardize `assignee:<login>`.
* **(C) An artifact-relation kind** — add `assigned_to` to ADR-0024's relation
  kinds and let the target be a user.

## Decision Outcome

Chosen: **(A)**. A native assignee is the only option that is reassignable in
place, validated against the `users` table, and visible to the router as a
first-class field rather than a convention. Tags stay what ADR-0018 made them —
routing *hints*, never identity — and SPEC-0025 defines the model, API, events
and UI. Switchboard projects `.artifact.assignee` in a follow-up
(stump.wtf/switchboard#558).

### Assignee

* **One, not many.** An artifact has at most one assignee. Handoffs are
  one-shot work orders; co-assignment is a queue concern for the receiver
  (Switchboard lanes), not for the artifact. If a future need is real, it
  arrives as a new ADR, not as a silently widened column.
* **A user, not an "agent identity".** Cairn records the account behind the
  token (`actor_id`), never which harness used it (ADR-0022's `actor_kind` is
  derived, not registered). There is no agent object to assign to. An agent is
  assigned to exactly as far as it is a user. When agents eventually own
  accounts of their own, the column already points at users and needs nothing.
* **Must be a reader.** An assignee MUST pass `authorizeRead` (SPEC-0023) for
  the artifact at assignment time: any signed-in user for `link` visibility, a
  current team member for `team`, the owner for `private`. Assigning work to
  someone who cannot open it is the error the UI would otherwise surface as a
  mystery.
* **Stale assignments stay.** Visibility tightening or a membership change can
  strand an assignment. We keep the row and say so: assignment records intent
  at the moment it was made, the signed event is the receipt (ADR-0027), and
  retroactive clearing would rewrite history the receiver already routed on.
* **Who may assign.** Exactly the users who may change the artifact's
  *metadata*: the owner of a personal artifact (including agents acting for
  them), and any current member of an owning team. Assignment is metadata, not
  sharing policy — an agent MAY assign, where ADR-0007 forbids agents only
  `sharing:manage` (visibility). This keeps the primary flow, an agent
  re-routing a handoff to another pool, one call.
* **Where it is declared.**
  * MCP: `artifact_create` and `bundle_create` gain an optional `assignee`;
    a new `artifact_assign` sets or clears it later.
  * REST: `assignee=<username>` on create; `PUT /v1/artifacts/{id}/assignee`
    with `{"assignee": "<username>"}` or `{"assignee": null}`.
  * Storage: `artifacts.assignee_user_id`, nullable, no owner-change semantics.
* **Announced.** `artifact.created` carries `assignee` in the signed `data`
  (omitted when unset); a new `artifact.assigned` kind fires on every effective
  change with the previous and new values, so a receiver can re-route. Both go
  through owned subscriptions and the kind filter (SPEC-0012, SPEC-0016).

### Consequences

* Good, because reassignment stops destroying review history: the artifact id,
  comments and reactions survive the handoff moving to someone else.
* Good, because the router gains a validated, signed identity field —
  `.artifact.assignee` — replacing tag matching as trust-bearing input, while
  tags stay uninterpreted hints (the `agent:` convention remains useful and
  remains advice).
* Good, because "assigned to me" becomes an index-backed view rather than a
  tag grep (SPEC-0025 REQ on the database shape).
* Bad, because a second mutable field on an immutable-feeling object adds a
  state transition to every consumer's mental model; mitigated by the
  `artifact.assigned` event being the only mutation signal and the no-change
  write being silent.
* Bad, because a stale assignment can point at a user who can no longer read
  the artifact; accepted above, and documented in SPEC-0025.

### Confirmation

SPEC-0025 carries the requirements and the golden-test expectations; the
implementation plan in its design.md lands the model, API, events, and UI as
separately reviewable PRs. Compliance review reads the golden files first:
an artifact created without an assignee must produce byte-identical events to
today's.

## Pros and Cons of the Options

### (A) Native assignee

* Good, because it is reassignable in place — the only option that keeps the
  annotation stream across a re-route.
* Good, because the value is a foreign key to `users`, so the router's trust
  question reduces to Cairn's own signature.
* Bad, because it is a new column, two event shapes, an API route and an MCP
  tool — the largest contract of the three.

### (B) `assignee:` tag convention

* Good, because it costs no schema: the Switchboard router matches tags today.
* Bad, because tags are set once (SPEC-0002 REQ "Artifact Tags"), so the
  option cannot survive contact with the core requirement — reassignment.
* Bad, because nothing validates the login, which is the defect #441 starts
  from; a validated tag would break ADR-0018's "the system MUST NOT interpret
  tags", and an unvalidated one ships the bug we are fixing.

### (C) Relation kind `assigned_to`

* Good, because ADR-0024 already has typed, authorized relations with a
  readability check on the target — the assignee-must-be-a-reader rule falls
  out for free.
* Bad, because relations are artifact-to-artifact: a user is not an artifact,
  so the model would need either a user-artifact shim or a second target type
  on every relation surface.
* Bad, because relations are immutable at create (ADR-0024's create-time
  declaration), which re-inherits the reassignment problem option (B) dies of.

## More Information

* Issue: `stump.wtf/cairn#441` — not linked, per ADR-0014 (the private forge
  has no public page to point at; ADR-0020 carries the same note).
* Spec: SPEC-0025, `docs/openspec/specs/artifact-assignment/`
* Switchboard follow-up: `stump.wtf/switchboard#558` (not linked, same rule).
* Incident context (why handoff routing matters): the 2026-10-01 OMG — a
  dropped `CAIRN_ENCRYPTION_KEY` silenced every outbound handoff for ~30h
  (Outline: 2026-10-01-cairn-handoffs-went-dark).
