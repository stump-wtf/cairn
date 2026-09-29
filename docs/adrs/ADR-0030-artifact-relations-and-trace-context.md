---
status: proposed
date: 2026-09-27
decision-makers: Joe Stump
extends: [ADR-0007, ADR-0017, ADR-0018]
related: [ADR-0003, ADR-0009, ADR-0015, ADR-0027, ADR-0029]
---

# ADR-0030: Artifact Relations and Trace Context — One Artifact Can Say What It Replies To, and Which Trace Made It

## Context and Problem Statement

A self-hosting user building a provenance view over Cairn artifacts filed stump-wtf/cairn#3:

> An artifact has no way to reference another artifact it was produced from, replied to, or is a follow-up to.

On `main`:

* **No relation columns.** `artifacts` has no parent, reply or thread column. The only link between
  artifacts is `bundle_members`, which groups files *under one* artifact.
* **A reference inside the body cannot travel.** `artifact.created` (ADR-0017) carries `id`,
  `title`, `url`, `tags`, `actor_id`, `on_behalf_of`, `model`, `channel`, `share_type` and
  `expires_at`, but never the body.
* **Tags are not relations.** A `reply:` tag in the handoff convention means *where to report an
  outcome*, not *what this artifact is about*. Tags are client-asserted and uninterpreted (ADR-0018).

The same user filed the matching gap in Switchboard (stump-wtf/switchboard#28). Switchboard answers
it with lineage links (Switchboard ADR-0043), which work best when a Cairn delivery can say two
things:

* which earlier artifact this one follows;
* which agent attempt made it.

Switchboard now mints a W3C `traceparent` for every attempt (Switchboard ADR-0042). If an agent
passes it to Cairn when creating an artifact, and Cairn echoes the trace id on the event,
Switchboard can link the artifact to the attempt as a fact rather than a guess.

**How should one artifact reference another it follows, and how should an artifact carry the trace
that produced it, without changing how tags work and without leaking artifacts across owners?**

## Decision Drivers

* **Structural, not textual.** A relation is a typed field Cairn validates and stores, not a
  convention inside a title or a tag.
* **Tags keep their meaning.** ADR-0018 stays as it is: tags are uninterpreted and client-asserted.
* **No existence oracle.** A relation to an artifact the creator cannot read must fail exactly as a
  relation to an id that does not exist, as `authorizeRead` requires (SPEC-0023). A reader who
  cannot see the target sees neither its id nor its title.
* **Trace context is standard.** Use W3C Trace Context, `traceparent` on the wire, so that an
  OTel-instrumented HTTP client propagates it with no code, and the CLI can read `TRACEPARENT` from
  its environment.
* **Parity across web, CLI and MCP** (ADR-0003).
* **Immutable, like tags.** Relations and trace context are set at creation and never edited.

## Considered Options

* **(A) A closed set of typed relations, `reply_to`, `derived_from` and `follows`, plus an optional
  `traceparent` on create.** *(chosen)*
* **(B) A single `parent_artifact_id` column.**
* **(C) Put the body, or an excerpt, in `artifact.created`** so that consumers extract references
  themselves (the fallback in #3).
* **(D) A relation tag convention**, e.g. `reply-to:<id>`, with no schema change.

## Decision Outcome

Chosen: **(A)**.

### Relations

* **Kinds.** An artifact MAY declare up to 8 relations at creation. Each is `{kind, id}`. `kind`
  is one of:
  * `reply_to`: "this answers or comments on that";
  * `derived_from`: "this was made from that", such as a receipt derived from a trace, or a
    revision of a document;
  * `follows`: "this is a follow-up to that". This generalizes the receipt-only `follows` field
    in SPEC-0021 REQ-6. A receipt's `follows` keeps its stricter rule, that the target is a live,
    same-owner receipt, and is also recorded as a `follows` relation. The card in #299 and every
    other reader then share one model.

  The set is closed. A new kind needs an ADR.
* **Where they are declared.**
  * MCP: `artifact_create` and `bundle_create` gain `relations: [{kind, id}]`.
  * REST: repeatable `X-Cairn-Relation: <kind>=<id>` headers and `relation=<kind>:<id>` query
    parameters.
  * CLI: `--reply-to <id>`, `--derived-from <id>` and `--follows <id>`.
* **The target must be readable by the creator.** This uses the same check as a read:
  `authorizeRead` once SPEC-0023 lands, and `GetByPublicID`'s uniform not-found until then. If any
  target fails, the create is rejected with `validation_failed` and the sentinel
  `relation-target-not-found`. The answer is identical for unknown, expired and unreadable ids.
* **Storage.** Relations are stored in `artifact_relations`.
  * The target is kept by internal id with `ON DELETE SET NULL`, alongside its public id.
  * A relation whose target later expires renders as "no longer available", and never with a
    title.
* **Reads.**
  * An artifact read returns `relations`.
  * Each relation's target id and title appear only when *this reader* can read the target.
    Otherwise the relation shows as "an artifact you can't open", with no id.
  * Reads also return `referenced_by`: artifacts that relate to this one and that the reader can
    read, capped at 50.
* **`artifact.created`** gains `relations: [{kind, id}]`, appended `omitempty` so that today's
  golden payload is unchanged. A relation is included only when its target belongs to the same
  owner workspace as the new artifact. Events go only to the artifact's workspace (SPEC-0023), so
  the payload can never name an artifact in someone else's.
* **Viewer.** The viewer renders "In reply to …", "Derived from …" and "Follows …" above the
  body, and a "Replies and derivatives" list below it.

### Trace context

* **Declared on create.** An optional W3C `traceparent` is accepted:
  * MCP: `artifact_create` and `bundle_create` gain `traceparent`.
  * REST: the standard `traceparent` request header.
  * CLI: `--traceparent`, defaulting to the `TRACEPARENT` environment variable.
* **Stored.** A valid value stores `trace_id` (16 bytes) and `parent_span_id` (8 bytes) on the
  artifact. An invalid value is ignored, as the W3C specification requires. It never fails a
  create.
* **Returned.** `trace_id` is returned on read and appended `omitempty` to `artifact.created` as
  32 lowercase hex characters.
* **Composition with OTLP ingest.** When the receiver in ADR-0015 lands, an artifact whose
  `trace_id` equals a run's trace can be linked to that run as a derived `produced` edge. That is a
  follow-up, noted here so the two records agree on the key.

### What this enables downstream

* **Relations.** Switchboard (ADR-0043) turns any relation on a delivery into a `spawned` link
  between the two artifacts' todos.
* **Trace ids.** It turns `trace_id` into a link to the attempt that minted that trace.
* **Both are facts**, marked with their provenance. Until this ships, Switchboard accepts a
  `todo:<id>` tag, which is honestly labelled as client-asserted (ADR-0018).

### Consequences

* Good, because #3's "which artifact was this a reply to" is a field with an authorization rule,
  rather than a guess from a title.
* Good, because one standard header ties an artifact to the agent run that made it, across
  products.
* Good, because tags are untouched.
* Bad, because `artifact.created` grows two optional fields, and create surfaces gain two
  arguments.
* Bad, because the read path gains a join, and a reverse lookup for `referenced_by`. An index on
  the target bounds it.
* Neutral, because relations are immutable. A wrong relation is fixed by creating a new artifact,
  as a wrong tag is today.

### Confirmation

* Creating B with `relations: [{kind: reply_to, id: A}]` stores the relation. B's read shows "In
  reply to A", A's read lists B under `referenced_by`, and `artifact.created` for B carries the
  relation.
* A relation to an unknown id and a relation to another user's private artifact produce
  byte-identical errors.
* A reader who can open B but not A sees the relation without A's id or title.
* `artifact.created` for a team artifact omits a relation to the creator's personal artifact.
* A create with `traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01` returns
  `trace_id = 4bf92f3577b34da6a3ce929d0e0e4736` on read and on the event. A malformed value is
  ignored, and the create succeeds.
* The untagged, unrelated `artifact.created` golden file is byte-identical.

## Pros and Cons of the Options

### (A) Typed relations and trace context

* Good, because it covers "reply", "follow-up" and "made from" with three kinds and one
  authorization rule. It also folds the receipt-only `follows` into the same model.
* Bad, because a new table and two new arguments are more contract to keep.

### (B) `parent_artifact_id`

* Good, because it is the smallest change.
* Bad, because an artifact can reply to one thing *and* derive from another, and a single untyped
  parent cannot say which relation it is.

### (C) Body in the event

* Bad, because it ships user content, possibly large and possibly secret, to every subscriber, and
  still leaves each consumer to parse references out of prose.

### (D) Relation tags

* Bad, because ADR-0018 says tags are uninterpreted. Making Cairn validate a tag's target would
  break that. Leaving the target unvalidated would make the relation a guess, which is the problem
  #3 describes.

## More Information

* **Extends:**
  * ADR-0007: provenance gains relations and trace context.
  * ADR-0017: `artifact.created` gains two fields.
  * ADR-0018: tags are explicitly unchanged.
* **Related:**
  * ADR-0029 / SPEC-0023: `authorizeRead`, and workspace-scoped events.
  * ADR-0015 / SPEC-0011: OTLP ingest, keyed by trace id.
  * ADR-0009: `produced` edges.
  * ADR-0027 / SPEC-0021: the receipt `follows` field, which becomes a `follows` relation, and
    the receipt card (#299).
* **Cross-product, cited in prose:** Switchboard ADR-0042 (per-attempt `traceparent`) and ADR-0043
  (lineage links with provenance).
* **Reported in** stump-wtf/cairn#3, which is public intake, triaged onto the canonical tracker.
* Implemented by SPEC-0024 (`docs/openspec/specs/artifact-relations/spec.md`).
