---
status: accepted
date: 2026-07-08
decision-makers: joestump
# optional forward-only graph edges (include only those that apply):
enables: [ADR-0002, ADR-0003]
---

# ADR-0001: Cairn as an AI-Native Artifact Store

## Context and Problem Statement

Humans and agents increasingly produce small, throwaway outputs worth showing to
someone else — a rendered audit, a code snippet, a screenshot, a captured agent
run — but the tools for sharing them (pastebins, gists, requestbins, file drops)
were built for humans typing at browsers, not for agents acting over an API. An
agent that produces a "receipt" for its human has no first-class place to drop it,
give it a short URL, and let the human read, react, and hand it onward; and the
human has no way to point their agent back at the same artifact to read or annotate
it. What is the bounded context and domain model for a service where **the same
artifact is first-class to both a human and an agent**, and how do we frame the
product so every downstream technical decision serves that goal?

This ADR is foundational. It fixes *what Cairn is* and *what lives in its domain*;
it deliberately defers *how* each piece is built. The share-type mechanism is
decided in ADR-0002, and the surface topology (web, CLI, MCP over one core) in
ADR-0003; storage, identifiers, annotations, provenance/access, trajectories, live
webhooks, and the platform choices follow in ADR-0005 through ADR-0012.

## Decision Drivers

* **Agent-native symmetry** — an agent must be able to do what a human can do with
  an artifact (create, read, comment, react, share), not a reduced subset. This is
  the differentiator versus a pastebin with a bolted-on API.
* **Ephemerality by default** — these are trail markers, not archives. Artifacts
  carry a visible TTL (`⧗ expires 7d`) and are expected to expire. The domain must
  treat expiry as intrinsic, not optional metadata.
* **A single coherent artifact abstraction** — every shared thing (markdown, code,
  image, generic file, multi-file bundle, live webhook, captured trajectory) must
  be the *same kind of object* with the same lifecycle, so that provenance,
  annotations, access, and expiry are written once and reused everywhere.
* **Extensibility of kinds without reshaping the core** — the trajectory type was
  added late in the design (turn 7). The domain must absorb a genuinely new kind of
  artifact without changing what an Artifact fundamentally *is*.
* **Provenance as a first-class fact** — every artifact records who/what made it and
  how (`claude · sonnet-4.6 · via MCP`, `sam@stump.rocks · via CLI`). This is core
  to the "what an agent has dropped for you" framing, not an audit afterthought.
* **Self-hostable, terminal-minimal ethos** — the product is for people who live in
  a shell and run their own infrastructure; the domain should not assume a heavy
  multi-tenant SaaS envelope.

## Considered Options

* **Option A — A single unified Artifact aggregate with a pluggable Share Type.**
  One domain object owns identity, body reference, metadata, provenance, access
  policy, expiry, and an annotation stream; a `Share Type` value drives
  presentation and anchor affordances. Bundles are artifacts that contain files;
  Workspaces scope ownership and access; the Bin is a listing/query over artifacts.
* **Option B — Separate top-level entities per kind** (a `Paste`, an `Image`, a
  `Bundle`, a `Webhook`, a `Trajectory`), each with its own identity, sharing,
  annotation, and expiry logic. Composition by convention rather than a shared
  aggregate.
* **Option C — A generic "object store with metadata" (an S3-with-comments).** No
  domain notion of share type at all; everything is bytes plus a free-form
  key/value bag, and viewers guess how to render from MIME type.
* **Option D — A document/collaboration model** (à la a wiki or docs product):
  long-lived documents with rich permissions and version history as the center of
  gravity, sharing and TTL layered on top.

## Decision Outcome

Chosen option: **"Option A — a single unified Artifact aggregate with a pluggable
Share Type"**, because it is the only option that makes the cross-cutting
concerns — provenance, annotations, access, expiry — *structural properties of one
object* rather than things re-implemented per kind. It gives humans and agents a
single mental model ("an artifact") and gives the codebase a single lifecycle to
enforce, while the Share Type seam (elaborated in ADR-0002) keeps the set of kinds
open for extension. Option B duplicates the hard, easy-to-get-wrong logic (sharing,
expiry, annotation anchoring) N times and guarantees drift between kinds. Option C
throws away exactly the typed-viewer/typed-anchor behavior the design brief is
built around (pins on images, span reactions on trajectories) and pushes that
complexity into every client. Option D optimizes for longevity and rich permissions
that Cairn explicitly does not want — Cairn's artifacts are ephemeral and shared by
link, not durable collaborative documents.

### The domain model (bounded context)

Cairn's bounded context is **"ephemeral, shareable, agent-native artifacts and the
conversation around them."** The core entities:

* **Artifact** — the aggregate root and the only shared unit. It has:
  * **id** — a short, opaque, URL-safe public identifier (scheme and encoding fixed
    in ADR-0005).
  * **share type** — the kind of artifact; drives viewer, metadata panel, and
    annotation anchors (mechanism in ADR-0002).
  * **body** — the content: bytes (text, image, blob) or a structured payload
    (bundle manifest, trajectory spans, webhook request stream). Bodies are stored
    and content-addressed per ADR-0008.
  * **metadata** — title, size, MIME/language hints, and type-specific fields shown
    in the panel (e.g. a file's checksum, a trajectory's run stats).
  * **provenance** — actor (`sam@stump.rocks` or `claude · sonnet-4.6`), channel
    (`via MCP`, `via CLI`, `via web`), and capture time. Detailed in ADR-0007.
  * **access policy** — a link-based sharing policy (`🔒 you + anyone with link`),
    an explicit, deliberate act to change. Detailed in ADR-0007.
  * **expiry** — a TTL after which the artifact is gone (`expires in 5d`).
    Ephemerality is the default; also ADR-0007.
  * **annotation stream** — the ordered set of reactions and comments anchored to
    the artifact or to positions within it (uniform model in ADR-0006).
* **Share Type** — a value that classifies an artifact and selects its presentation
  and anchor affordances. The type *set* is extensible; the registry that realizes
  this is ADR-0002. Known types: markdown, code, image, file (generic), bundle,
  webhook (live), trajectory.
* **Bundle** — an Artifact whose body is a manifest of many files (mixed media),
  browsed as a tabbed viewer and readable file-by-file over MCP. A bundle is *not*
  a second aggregate; it is the artifact abstraction applied to a multi-file body.
* **Workspace** — a person's or team's space of artifacts ("your Cairn workspace"),
  the boundary for ownership, identity, and access. Auth is scoped to a workspace;
  an agent connected over MCP OAuth (ADR-0004) acts within one.
* **The Bin** — the listing/index: a query/projection over a workspace's artifacts,
  rendered identically as a web listing and a CLI TUI (`cairn ls`). It is a *view*
  of artifacts, not an entity of its own.

### Core principles established here (binding on downstream ADRs)

1. **An artifact is first-class to both humans and agents.** Every core operation —
   create, read, list, comment, react, share — is available symmetrically to a
   human (web/CLI) and an agent (MCP). No operation is human-only or agent-only.
   ADR-0003 turns this into a triple-surface-parity constraint.
2. **A run or tool produces artifacts.** Production is an event with provenance: a
   pipe from the CLI, an agent tool call over MCP, or a captured agent run. The
   trajectory type makes production itself shareable — a captured run is an artifact
   that *links to the artifacts it produced* (its `write` spans point at the
   markdown/file shares they created). Provenance is therefore intrinsic, not
   decorative.
3. **Artifacts are ephemeral and shareable by link.** The default posture is a
   short-lived thing you hand onward, not a durable record you curate. TTL and
   link-based access are properties of every artifact.
4. **One artifact, many surfaces.** The artifact is defined independently of how it
   is viewed. Web, CLI, and MCP are clients of the same core; the share type
   governs rendering without forking the artifact model.

### Consequences

* Good, because every cross-cutting concern (provenance, annotations, access,
  expiry) is written once against one aggregate and is automatically uniform across
  all current and future share types.
* Good, because the human/agent symmetry is a property of the domain rather than a
  feature to remember to add to each surface, which is what makes Cairn "AI-native"
  instead of "a pastebin with an API."
* Good, because framing the Bin and Bundle as *views/shapes of the artifact* rather
  than new entities keeps the model small and makes `cairn ls` and the web listing
  provably the same listing.
* Bad, because a single aggregate that must accommodate everything from a 40 MB
  `.sql.gz` blob to a live webhook stream to a structured span tree risks a
  bloated, leaky abstraction if the body/type seams (ADR-0002, ADR-0008) are not
  kept clean; live and structured bodies strain a model conceived around "bytes."
* Bad, because committing to ephemerality-by-default forecloses use cases (durable
  hosting, long-lived galleries) that some users will ask for; we are deliberately
  not that product.
* Neutral, because "workspace" is introduced as the ownership/access boundary but
  its multi-user/team semantics are intentionally thin in v1 — the design centers a
  single human plus their agent, and richer team models are left for later without
  changing the aggregate.

### Confirmation

* This ADR is confirmed by the existence and shape of the domain package (the core
  service of ADR-0003): a single `Artifact` type carrying id, share type, body
  reference, metadata, provenance, access policy, expiry, and an annotation stream,
  with Bundle and the Bin expressed in terms of it rather than as parallel
  aggregates.
* Design review of every downstream ADR checks that it treats the Artifact as the
  unit of sharing and does not introduce a competing top-level shareable entity;
  any proposal to make a kind (image, webhook, trajectory) its own aggregate is a
  regression against this decision and must be justified as superseding it.
* The specs derived from this ADR (in `docs/openspec/specs/`) assert the invariants:
  every artifact has provenance and an access policy at creation, every artifact has
  an expiry, and the create/read/list/comment/react/share operations are defined
  once over the Artifact and reused by all surfaces (ADR-0003).
