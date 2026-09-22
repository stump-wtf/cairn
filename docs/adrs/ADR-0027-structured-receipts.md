---
status: accepted
date: 2026-09-22
decision-makers: [joestump, joestump-agent]
extends: [ADR-0018]
related: [ADR-0002, ADR-0003, ADR-0007, ADR-0009, ADR-0017, ADR-0022, ADR-0023, ADR-0025, ADR-0026, ADR-0028, ADR-0029]
---

# ADR-0027: Structured Receipts — a Typed Metadata Schema, a Card, and a Dedicated Verb

## Context and Problem Statement

A **receipt** is the note an agent leaves when it finishes something: what it did, what
changed, and what a human needs to know. Receipts are already the most common thing agents
put in Cairn. The `cairn` plugin ships a `/cairn:receipt` command, and every scheduled run
in our own fleet ends with one. But a receipt is only markdown. The plugin's command is four
loose prompts ("what was asked, what changed, what was verified, any follow-ups") and a
default 7-day TTL. Nothing about a receipt is structured, so nothing downstream can act on
it:

* The `artifact.created` event carries `title`, `url`, `tags` and provenance, but no content
  (`internal/outboundhook`). Switchboard's routing envelope already reserves
  `.artifact.metadata` for exactly this, and today that field is always `null`.
* A consumer that wants to know "does a human need to do anything?" must fetch and parse
  prose.
* A run and the receipt it produced can be linked (a span's `produced_artifact_id` writes a
  `produced_edges` row), but nothing documents this or renders it, and the receipt itself
  cannot say which run it came from.

A self-hosting customer's operating plan shows what a receipt must contain to be useful. It
requires every meaningful update to answer eight fixed questions:

* What changed?
* Who experiences it?
* Before: what happened previously?
* After: what happens now?
* How do we know it is actually active?
* Did it produce the intended outcome, or when will that be measured?
* What does a human need to do (the default is **nothing**)?
* What remains, and who owns it?

Its closure gate adds a ninth: "what the system now knows that it didn't". The plan also
wants the TL;DR first and the required human action stated plainly, with technical detail
behind links. They built all of this themselves because the tools they used had nowhere to
put it.

How should Cairn make a receipt a first-class, machine-readable record without turning
every artifact into a form?

## Decision Drivers

* **Decidable from the event.** A routing rule must be able to see "a human needs to act"
  or "an outcome is due on this date" without fetching the body. This is ADR-0018's driver,
  now for content instead of routing intent.
* **One source of truth.** The card, the markdown body, the event and the search index must
  never disagree about what a receipt says.
* **Agent-shaped.** A model writing a receipt should meet named, described fields in the
  tool schema, not a free-form object it has to guess at (SPEC-0007 REQ "Agent-Shaped Tool
  Schemas").
* **Parity across surfaces** (ADR-0003): REST, MCP, CLI and web.
* **Honest trust boundary.** Everything in a receipt is claimed by its creator. The card
  must not look like verification, and provenance must stay the only trust signal
  (ADR-0007, ADR-0018).
* **Additive events** (SPEC-0012). The event for an artifact that is not a receipt must stay
  byte-identical.
* **Composable.** Receipts are the natural thing to retain (ADR-0026), to search
  (ADR-0028), and to export as learning records.

## Considered Options

* **A typed metadata object on the artifact, from a closed, versioned schema registry, with
  receipts as the first schema; created through a dedicated `receipt_create` verb.**
* **The same metadata object, created through `artifact_create` with a generic `metadata`
  argument.**
* **A `receipt` share type.**
* **Receipt fields as markdown front-matter in the body, parsed at ingest.**
* **An unvalidated key/value metadata map on every artifact.**

## Decision Outcome

Chosen option: **"a typed metadata object from a closed schema registry, created through a
dedicated `receipt_create` verb"**. It is the only option that keeps a receipt decidable
from the event, gives agents a self-describing schema, and leaves every other artifact
untouched.

### The shape

An artifact gains an optional **`metadata`** object, stored as JSONB. When present, it
carries a `schema` discriminator that names a schema registered in Cairn's code, the same
way share types are registered (ADR-0002). The registry is **closed**: an unknown `schema`
is rejected. It starts with exactly one entry, **`cairn.receipt/v1`**:

| Field | Question it answers | Type | Required |
|---|---|---|---|
| `changed` | What changed? The TL;DR. | text | yes |
| `affected` | Who experiences it? | text | yes |
| `before` | What happened previously? | text | yes |
| `after` | What happens now? | text | yes |
| `evidence` | How do we know it is actually active? | list of `{label, url}`, may be empty | yes |
| `evidence_note` | Anything the links do not show | text | no |
| `outcome` | Did it work, or when will that be measured? | `{status: measured, pending or not_applicable; summary; measure_at}` | yes |
| `human_action` | What must a human do? | text; the server stores `Nothing` when omitted | defaulted |
| `remaining` | What remains, and who owns it? | list of `{item, owner}`, may be empty | yes |
| `learned` | What does the system now know that it didn't? | text; "nothing new" is a valid answer and must be said | yes |
| `follows` | An earlier receipt for the same work (for example, when the outcome is measured later) | artifact id | no |

Metadata, like tags and bodies, is **immutable** once created. When an outcome is measured
later, that is a new receipt that `follows` the first. Nobody edits the first one.

### The body is rendered from the metadata

`receipt_create` creates a **markdown** artifact, and the server writes its body: a
deterministic rendering of the fields using a template versioned with the schema. The
caller's optional `details` (free markdown) is appended under a "Details" heading. Because
the body comes from the fields, the markdown viewer, `artifact_read`, the A2UI resource, the
CLI and the event cannot disagree. The body's checksum (ADR-0008) also covers the fields,
so a receipt retained under ADR-0026 has a checksum that attests to the structured answers,
not just the prose.

The server also adds the tag `receipt` if the caller did not. The Bin filter `?tag=receipt`
then finds every structured receipt. A `receipt` tag **without** metadata is still allowed:
tags are client-asserted routing hints (ADR-0018), and the plugin's existing loose receipts
carry it. Rendering and routing on structure key off `metadata.schema`, never off the tag.

### A dedicated verb, not a generic argument

| Surface | Verb |
|---|---|
| MCP | `receipt_create`: every receipt field is a named, described property, plus `title`, `details`, `tags` and `model` |
| REST | `POST /v1/receipts` (JSON), returning the artifact object |
| CLI | `cairn receipt --from receipt.json` (or `-` for stdin; YAML accepted), plus `--details notes.md` |
| Web | read-only: the receipt card; receipts are written by agents and scripts |

`artifact_create` gains **no** `metadata` argument in this decision. The column is generic,
so a second schema later is a registry entry plus a verb, not a migration. The only way to
write metadata today, though, is a verb whose schema says exactly what it takes.

### The card

When an artifact carries `cairn.receipt/v1` metadata, the web shell renders a **receipt
card** above the markdown body, laid out in the order the customer's format asks for:

1. the TL;DR (`changed`);
2. the **human action**, as a prominent chip ("You need to do: Nothing");
3. before and after, side by side;
4. the evidence links, or a visible "No evidence linked" warning when the list is empty;
5. the outcome status with its measurement date;
6. the remaining items with their owners;
7. "What the system now knows".

The card's header says whose claim this is, using server-derived provenance: "Receipt
asserted by claude · opus-5 for sam@…, via MCP". Nothing on the card says "verified". A
human-only acknowledgement reaction is ADR-0022's job, not the card's.

### Linking a receipt to its trace

`produced_artifact_id` on a trace span is the link. It is already implemented, and this
decision documents it and renders it. A span that produced a receipt records a
`produced_edges` row, and the receipt card shows "Produced by run …" through the reverse
index (`produced_edges_artifact_idx`). The card lists a producing run **only when the run
and the receipt have the same owner** (a user, or a team under ADR-0029). Without that
rule, anyone who can create a run could attach it to a stranger's receipt, because the edge
checks only that the artifact exists. A link is also shown only when the viewer can read the
run under ADR-0029's `authorizeRead`, so a team-visible run's id never appears to an outside
reader who holds only the receipt's link. Since the edge requires the receipt to exist first,
the order is: create the receipt, then append or close the span that names it.

### Events

`artifact.created` for a receipt carries `data.metadata`, the full `cairn.receipt/v1`
object. The field is appended and omitted for every non-receipt artifact, so those events
stay byte-identical (SPEC-0012). Switchboard already projects `data.metadata` onto
`.artifact.metadata`. A rule can therefore route on
`.artifact.metadata.human_action != "Nothing"` (for example, to a notification sink) or
schedule on `.artifact.metadata.outcome.measure_at`, with no Switchboard change. The event
is routed like every other kind, by the artifact's owning workspace (ADR-0022, ADR-0029).
This ADR adds no fan-out path.

### Consequences

* Good, because routing on "a human must act" becomes a one-line rule over data that is
  already in the event.
* Good, because a model writing a receipt gets a described schema. The plugin's template
  becomes the tool's contract rather than a prompt someone has to keep in sync.
* Good, because the body, the card, the event and (under ADR-0028) the search index all
  derive from one object.
* Good, because the generic `metadata` column plus a closed registry gives the next schema
  a place to land without another migration or another debate.
* Bad, because this is the first server-rendered artifact body. A template change is a
  schema version bump (`cairn.receipt/v2`), never an in-place edit. Otherwise two receipts
  with identical fields would render, and checksum, differently.
* Bad, because receipt fields are client-asserted. A receipt can claim evidence it does
  not have. The card says whose claim it is, and it cannot say more than that.
* Bad, because events now carry content, not just a pointer. A receipt's text goes to
  every subscription its owner's workspace has (ADR-0029), so a subscriber sees the fields
  without opening the link. That subscriber could already open the link, so this adds
  convenience, not access. No receipt reaches an operator-chosen target: ADR-0029 removes
  the instance-wide `CAIRN_OUTBOUND_WEBHOOK_URLS` outright, in the change that ships owned
  subscriptions (design review, Joe, 2026-09-22).
* Neutral, because `artifact_create` does not take metadata. A caller that wants a receipt
  uses the receipt verb. That is one more tool in the list, and it is the tool the task
  names.

### Confirmation

SPEC-0021 carries the scenarios. The load-bearing ones:

* a `receipt_create` with every field round-trips through read, the event and the card;
* an unknown schema is rejected, and so is a missing required field (with the field named);
* omitting `human_action` stores `Nothing`;
* the rendered body is byte-identical for identical fields;
* a non-receipt `artifact.created` payload is unchanged by this capability;
* the card lists a producing run of the same owner and hides one from a different owner.

## Pros and Cons of the Options

### Typed metadata, closed registry, dedicated verb (chosen)

* Good, because the tool schema documents every field, and the server validates it.
* Good, because the registry is extensible without a migration, and closed against junk.
* Bad, because it adds a verb on every surface.
* Bad, because server-rendered bodies make the template part of the schema's compatibility
  promise.

### Same metadata through `artifact_create`

* Good, because there are no new verbs, and any artifact type could carry metadata.
* Bad, because a generic `metadata` argument is a union the model must fill in by
  convention. The schema-per-tool clarity that makes agents reliable is lost.
* Bad, because every share type's create path must validate every schema it might receive.

### A `receipt` share type

* Good, because the viewer registry would select the card with no special case.
* Bad, because share types classify content, not intent (ADR-0002, ADR-0018). A receipt is
  markdown and needs the markdown viewer, anchors and A2UI rendering.
* Bad, because every consumer that switches on `share_type` would need to learn a new value
  for what is really a markdown artifact.

### Front-matter in the markdown body

* Good, because it needs no schema change.
* Bad, because it is ADR-0018's rejected "routing intent in the body" option over again.
  The event cannot carry the fields without an ingest-time parser, and a markdown heading
  is not a contract.
* Bad, because body and metadata could disagree the moment someone edits the prose half.

### An unvalidated key/value map

* Good, because it is maximally flexible.
* Bad, because ADR-0018 already rejected a label map for routing. For content it is worse:
  every consumer invents its own keys, and nothing guarantees the nine answers are present.

## Architecture Diagram

```mermaid
sequenceDiagram
    autonumber
    participant Ag as Agent (Harness run)
    participant C as Cairn
    participant SB as Switchboard
    participant H as Human
    Ag->>C: receipt_create {changed, affected, before, after, evidence, outcome, human_action, remaining, learned}
    C->>C: validate cairn.receipt/v1, render body, tag "receipt"
    C-->>Ag: {id, url, checksum}
    Ag->>C: run_append_spans [write span, produced_artifact_id = receipt id]
    C-)SB: artifact.created {data.tags ∋ receipt, data.metadata}
    SB->>SB: rule: .artifact.metadata.human_action != "Nothing"
    SB-)H: notification (Gotify / Apprise sink)
    H->>C: opens card: TL;DR, action chip, before/after, evidence, outcome
    H->>C: retain (ADR-0026) when it is a record to keep
```

## Security and Tenancy

* **Client-asserted content.** Receipt fields carry the same trust as tags: a consumer may
  route on them and must never authorize on them. `actor_id` stays the only identity.
* **Redaction.** Metadata strings are persisted text, so the ingest scanner (ADR-0023)
  MUST scan them as it scans bodies. That covers the rendered
  body and the raw fields.
* **Link safety.** Evidence URLs are rendered as plain links (`rel="noopener noreferrer"`,
  no preview fetch). Only `https`, `http` and `mcp://cairn/` schemes are accepted. The
  server fetches nothing from them.
* **Tenancy.** A receipt is an artifact, owned by a user or a team (ADR-0029). The "Produced
  by run" and "Follows" links are shown only across artifacts with the same owner, so a
  stranger's run or receipt cannot attach itself to yours. They are shown only to a viewer
  who passes `authorizeRead` on the linked artifact, so a team-visible run or receipt never
  appears to an outside link holder.
* **Events.** Metadata travels to the same targets as the rest of the event. The disclosure
  model is therefore exactly as good as cairn#185 and ADR-0029 make it, and no worse than
  the capability URL the event already carries.

## Composition with Switchboard and Harness

* **Switchboard** reads `.artifact.metadata` with no code change: its envelope already
  passes the field through. Its notification sinks (Switchboard ADR-0034) are the natural
  consumer for "a human must act". A receipt's handle is also the right thing to carry as a
  relay attempt's note (Switchboard ADR-0039) and as a reply-to-source link (Switchboard
  ADR-0033).
* **Harness** trace export (F-H7) should set `produced_artifact_id` on the span that wrote
  the receipt, and Harness run history (Harness ADR-0028) can record the receipt id per run.
  Both are Harness-side work. This ADR only guarantees that the link exists and renders.
* **The plugin.** `/cairn:receipt` in the Claude Code plugin becomes a thin wrapper that
  asks for the nine answers and calls `receipt_create`. It also warns that receipts expire
  on the default TTL unless retained (ADR-0026).

## More Information

* Extends ADR-0018 (tags stay routing hints; metadata is the structured sibling ADR-0018
  deliberately did not build) and ADR-0017's event payload (via SPEC-0012 REQ "Event
  Payload").
* Related records (front-matter edges): ADR-0022 (events, human-only
  reactions), ADR-0023 (redaction), ADR-0025 (validation error details), ADR-0026
  (permanent retention), ADR-0028 (search and export), ADR-0029 (teams).
* `produced_artifact_id` is specified in SPEC-0004 and implemented in
  `internal/trajectory`. This record makes it the documented receipt link.
