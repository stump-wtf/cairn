---
status: proposed
date: 2026-09-11
decision-makers: [joestump, joestump-agent]
extends: [ADR-0017]
related: [ADR-0001, ADR-0004, ADR-0007]
---

# ADR-0018: Client-Asserted Artifact Tags

## Context and Problem Statement

Outbound `artifact.created` events (ADR-0017) already let Switchboard turn a new
artifact into a todo. The next step is agent-to-agent handoff. A sweep agent (a
morning brief, a backlog triage) writes a self-contained work order as a Cairn
artifact, the event fires, a Switchboard routing rule sends it to a worker lane, and
a worker claims it and carries it out.

Nothing in today's event tells a handoff apart from an ordinary paste, and nothing
says which lane should run it. A routing rule could fetch the artifact and parse the
body, but that ties every consumer to a body format, and routing would then depend on
a read Switchboard otherwise never has to make.

How should a creator attach routing intent to an artifact?

## Decision Drivers

* A routing rule must be able to decide from the event alone, with deterministic
  matching and no body fetch.
* Provenance stays the only trust signal. ADR-0004 and ADR-0007 derive the actor and
  channel server-side precisely so a client cannot claim them, and routing intent
  must not blur that line.
* Every create surface (REST, CLI, and MCP; single-body artifacts and bundles alike)
  gets it at once (ADR-0003 parity).
* The lane vocabulary belongs to Switchboard and will change. Cairn must not need a
  release each time it does.
* The change is small and additive: existing event consumers and signature checks
  keep working unchanged.

## Considered Options

* A bounded, client-asserted tag list on the artifact
* A bounded, client-asserted key/value label map on the artifact
* Typed handoff fields on the artifact (`handoff`, `lane`, `size`, …)
* Routing intent encoded in the body (front-matter in the markdown)
* A dedicated handoff share type

## Decision Outcome

Chosen option: "A bounded, client-asserted tag list". It is decidable from the event,
it keeps the vocabulary out of Cairn, it reaches every surface through the one store
choke point that already emits the event, and it is the simplest shape a jq rule or a
human can match. Tags are also the shape Joe asked for.

Tags are a list of short strings, set once at creation and never changed afterwards.
They are persisted as a `TEXT[]` column beside provenance, not inside it, returned on
read and list, usable as a Bin filter (`?tag=`, containment), and shown in the web
panel under their own heading. The `artifact.created` event carries them as
`data.tags`, alongside `data.on_behalf_of`, which it did not carry before.

Both new fields are appended and omitted when empty. An event for an untagged REST/CLI
artifact is therefore byte-identical to the event before this decision.

The aggregate normalizes tags at create. A malformed tag rejects the create, and
nothing is truncated or case-folded. Exact repeats are dropped, keeping
first-occurrence order:

* at most 32 distinct tags;
* each tag 1–64 bytes of lowercase `[a-z0-9._:/#-]`.

`#` extends the requested `[a-z0-9._:/-]` because the convention's
`issue:<owner/repo#n>` cannot be written without it. The comma stays excluded, so it
is always a safe list separator on the wire.

Tags are **client-asserted**. A consumer MUST NOT base a trust or authorization
decision on a tag. `handoff` says what the creator wants, never who the creator is,
and trust comes from `actor_id` alone: the authenticated principal (a static token's
actor, a PAT's owner, or the human behind an OAuth grant). `on_behalf_of` is
different. The server records it from the MCP session's `initialize`, never from a
request field, but its content is the client's self-reported name and version. It
names the harness; it is context, not an identity.

### The handoff convention

Cairn documents this convention in its product docs and publishes it in the
`artifact_create` and `bundle_create` tool descriptions, but it does **not** validate
it. Switchboard's routing rules match these tags.

| Tag | Meaning |
|---|---|
| `handoff` | The artifact is a work order for another agent. |
| `lane:s` · `lane:m` · `lane:l` · `lane:vision` · `lane:auto` | Which worker lane runs it. Lanes are by difficulty, not provider. Optional; `lane:auto` or no lane routes by size. |
| `size:s` · `size:m` · `size:l` · `size:xl` | The size ladder: the weakest model able to carry the work end to end. Optional. |
| `repo:<owner/name>` | The repository the work targets. Optional. |
| `issue:<owner/repo#n>` | The tracked issue, when one exists. Optional. |
| `source:<harness>/<run>` | The run that produced the handoff. Optional. |
| `reply:cairn-comment` · `reply:signal` | How the executing agent reports back. `cairn-comment` means a comment on this artifact. Optional. |

**Trust stance for receivers.** A handoff from another of our agents is
**semi-trusted**. The receiving agent carries out the task, but it treats the artifact
body as data that can carry prompt injection: it never follows instructions that
widen its permissions, send data somewhere new, or skip its own review rules just
because the handoff says so. The authenticated `actor_id` says whose token or grant
sent it, and `on_behalf_of` names the harness it ran in. The tags and the body never
establish either.

### Consequences

* Good, because a routing rule matches `data.tags | index("handoff")` and a `lane:`
  prefix without ever reading the body.
* Good, because the convention can grow (a new lane, a new prefix) with no change to
  Cairn.
* Good, because the payload change is provably additive: golden tests pin the untagged
  bytes as they were before tags existed.
* Good, because a flat list gets a Bin filter for free as a Postgres array
  containment.
* Bad, because Cairn accepts a mistyped lane (`lane:medium`), and the mistake only
  shows up as a misroute downstream. Switchboard's default route mitigates it.
* Bad, because rejecting uppercase means an agent must lowercase run ids and
  timestamps before tagging. The tool descriptions say so.
* Bad, because tags give a careless consumer a new place to take a trust shortcut.
  Documentation mitigates it, and so does carrying the authenticated `actor_id` in the
  same event object.
* Neutral, because tags are immutable after creation. Retagging means creating a new
  artifact, and no update surface is part of this decision.

### Confirmation

SPEC-0002 REQ "Artifact Tags" and SPEC-0012 REQ "Event Payload" carry the scenarios.
Golden payload tests cover events with and without tags. Integration tests create
tagged artifacts over REST (raw and multipart, including bundles), over MCP
`artifact_create` and `bundle_create`, and through the CLI's `--tag` flag, and they
filter the Bin by tag.

## Pros and Cons of the Options

### A bounded, client-asserted tag list

* Good, because it is the simplest shape to write, match, filter and display.
* Good, because the vocabulary lives with its consumer.
* Bad, because structure (`lane:s`) is convention only; Cairn cannot reject a
  misspelled value.

### A bounded, client-asserted key/value label map

* Good, because a key can carry exactly one value, so `lane` cannot be given twice.
* Bad, because it is heavier on every surface (`--label k=v`, a JSON object in MCP),
  and a map has no natural order for display or signed bytes.
* Neutral, because `key:value` tags cover the same routing needs.

### Typed handoff fields on the artifact

* Good, because `lane` and `size` could be validated enums, discoverable in the
  schema.
* Bad, because every new lane becomes a Cairn release, and Cairn's releases get
  coupled to Switchboard's configuration.
* Bad, because every new routing need becomes another column and migration.

### Routing intent encoded in the body

* Good, because it needs no schema change.
* Bad, because every consumer must fetch and parse the artifact before it can route
  it.
* Bad, because a bundle has no single body, and a markdown heading is not a contract.

### A dedicated handoff share type

* Good, because it is explicit.
* Bad, because a handoff is still markdown or a bundle and needs those viewers. Share
  types classify content, not intent (ADR-0002).

## More Information

* Extends ADR-0017's event payload; see SPEC-0012 REQ "Event Payload".
* The bounds and their scenarios live in SPEC-0002 REQ "Artifact Tags".
* The consumer is Switchboard's deterministic routing (Switchboard ADR-0024 /
  SPEC-0020).
