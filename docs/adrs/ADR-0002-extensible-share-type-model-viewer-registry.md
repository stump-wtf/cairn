---
status: accepted
date: 2026-07-08
decision-makers: joestump
# optional forward-only graph edges (include only those that apply):
extends: [ADR-0001]
---

# ADR-0002: Extensible Share-Type Model with a Viewer Registry

## Context and Problem Statement

ADR-0001 makes the Artifact a single aggregate whose **share type** selects its
viewer, its metadata panel, and its annotation affordances. But the set of types is
open — markdown, code, image, generic file, bundle, live webhook, and trajectory
today, with trajectory added late (turn 7) and more expected. Each type contributes
markedly different behavior: markdown renders with a TOC and margin comments, images
carry region **pins**, files are non-previewable blobs with a checksum and download,
webhooks stream live requests that are reactable-but-not-commentable, and
trajectories render an OTel-style span waterfall over a structured span tree. How do
we structure the codebase so a new share type can be added — with its badge, viewer,
metadata shape, and anchor affordances — **without editing the core artifact model
or the app shell**, while unknown or opaque types still degrade gracefully?

## Decision Drivers

* **Open for extension, closed for modification.** Trajectory arrived after the
  model was set; the next type should too. Adding a type must not require touching
  the Artifact aggregate (ADR-0001), the storage layer (ADR-0008), or the shell
  chrome (ADR-0011).
* **Per-type behavior is real and divergent.** Badge code, viewer, metadata-panel
  fields, and which annotation anchors are legal all vary by type — and some rules
  are semantic, not cosmetic (webhook requests are reactable but *not* comment-
  threaded; images support region pins; trajectories anchor to spans). The seam must
  carry behavior, not just a label.
* **Graceful degradation for the unknown.** An artifact of an unrecognized or
  not-yet-registered type must still be viewable, shareable, and downloadable. The
  design already names the fallback: the generic **file** viewer (size, checksum,
  download).
* **A clean core/per-type boundary.** Provenance, access, expiry, identity, and the
  annotation *storage* model are core and uniform (ADR-0001, ADR-0006, ADR-0007);
  only rendering and anchor *affordances* are per-type. The line between them must be
  explicit or every type will reach into the core.
* **One binary, no plugin runtime.** The house stack is a single static Go binary
  for self-hosting. Extensibility should be a compile-time registration pattern, not
  a dynamic plugin loader or out-of-process renderer.
* **Server-rendered, low-JS shell.** Viewers are Go `html/template` fragments
  enhanced with HTMX/Alpine (ADR-0011); the registry must fit that rendering model,
  not assume a client-side component framework.

## Considered Options

* **Option A — A Go `ShareType` interface with a compile-time registry.** Each type
  is a value implementing an interface (`Badge()`, `Render(ctx, artifact) template`,
  `MetadataPanel(artifact)`, `AllowedAnchors()`, `DecodeBody`/`ValidateBody`) and
  registers itself into a central registry at package init. The core looks up the
  registered handler by the artifact's type; an unknown type resolves to the
  built-in generic-file handler.
* **Option B — A `switch`/enum over a fixed type set.** Type is an enum; viewers and
  panels are selected by `switch shareType { … }` at each call site (render, panel,
  anchor validation). Adding a type means finding and editing every switch.
* **Option C — Out-of-process / dynamic plugins** (Go `plugin` package, WASM
  modules, or a subprocess renderer protocol). Types are loaded at runtime from
  external artifacts so third parties can add viewers without recompiling.
* **Option D — Data-driven declarative descriptors.** A type is a config record
  (badge string, template name, list of panel fields, list of allowed anchors) with
  no code; a generic engine interprets the descriptor to render and validate.

## Decision Outcome

Chosen option: **"Option A — a `ShareType` interface with a compile-time registry"**,
because it makes each type a self-contained unit that contributes exactly the four
things the design says a type owns (badge, viewer, metadata panel, anchor
affordances) while the core stays closed: registration is the *only* integration
point, and an unregistered type deterministically falls back to the generic file
viewer. It fits the single-static-binary constraint (registration at `init`, no
runtime loader) and the server-rendered shell (a viewer returns a template
fragment). Option B spreads type knowledge across every call site and turns "add a
type" into a scavenger hunt with guaranteed omissions — precisely the friction that
made trajectory hard to add. Option C buys runtime extensibility Cairn does not need
and pays for it in security surface, operational fragility (Go `plugin` is famously
brittle across builds; WASM/subprocess adds a rendering protocol to maintain), and
complexity that contradicts the self-host-a-single-binary ethos. Option D is elegant
for the cosmetic 80% but cannot express the semantic behavior — webhook's
"reactable, not commentable," image pin geometry, trajectory span anchoring — so it
would need code escape hatches anyway, collapsing back toward Option A with a weaker
type boundary.

### The `ShareType` seam

A share type is a Go value satisfying a `ShareType` interface. The interface is the
contract for the four things a type owns:

* **Badge** — the short code and styling shown in the shell and the Bin
  (`MD`, `PY`/lang, `IMG`, `FILE`/`GZ`, `HK`, `TRJ`; bundle renders its own badge).
* **Viewer** — given the artifact (and a lazily-fetched body reader from the storage
  layer, ADR-0008), return the server-rendered body fragment: rendered markdown with
  TOC, syntax-highlighted code with a symbol outline, an image with a pin overlay, a
  file card, a bundle tab strip, a live webhook inspector, or a trajectory waterfall
  + activity stream.
* **Metadata panel** — the type-specific fields for the right-hand panel: a file's
  size/gzip note/checksum, a trajectory's run stats and time-by-category breakdown,
  an image's pin count. Core panel sections (provenance, access, expiry, comments)
  are supplied by the shell, not the type.
* **Anchor affordances** — which annotation anchors are legal for this type, so the
  uniform annotation layer (ADR-0006) can validate and place them: markdown block/
  bullet + text selection, code line/selection, image region pin, webhook request
  (reaction only), trajectory span/turn/tool-call + text selection, whole-artifact
  (always legal). This is where the "webhook requests are reactable but not comment-
  threaded" rule lives — as a property of the type, enforced centrally.

Types register themselves into a central registry keyed by a stable type identifier.
The core service (ADR-0003) and the app shell (ADR-0011) never `switch` on type;
they resolve the handler through the registry.

### The core/per-type boundary

**Core (uniform, type-agnostic):** identity and URL (ADR-0005); body storage and
content addressing (ADR-0008); provenance, access policy, expiry (ADR-0007);
annotation *storage* and threading (ADR-0006); the app-shell chrome — logo, URL
control, Share button, collapsible panel (ADR-0011). A type may not reimplement any
of these.

**Per-type (the interface above):** badge, body rendering, metadata-panel *fields*,
and the *set* of legal annotation anchors. A type interprets its own body shape
(e.g. trajectory decodes the span tree, bundle reads the manifest) but stores bytes
through the same content-addressed layer as everyone else.

The seam is deliberately narrow: a type sees the artifact and its body; it does not
see other artifacts' storage, the auth layer, or the persistence schema.

### Degradation for unknown or opaque types

Resolution is total: the registry lookup **always** returns a handler. If an
artifact's type is unregistered (an old artifact, a type from a newer build, or a
body we cannot interpret), it resolves to the **generic file** handler — the same
one that backs `FILE`/`GZ` artifacts. The user still gets a badge, size, checksum,
download, and whole-artifact reactions and comments; only the rich viewer is absent.
This is the same non-previewable-blob experience the design specifies for
`staging-db-dump.sql.gz`, reused as the universal floor. No type can render an
artifact unviewable.

### Consequences

* Good, because adding a share type is a localized, additive change: implement the
  interface, register it, ship one template fragment — no edits to the core model,
  storage, or shell. This is exactly the "trajectory added late without reshaping
  the core" property the design demands.
* Good, because semantic anchor rules (reaction-only webhooks, image pins,
  trajectory span anchors) live in one place per type and are enforced centrally by
  the annotation layer, so anchors cannot drift between viewer and validator.
* Good, because total resolution to the generic file handler guarantees forward and
  backward compatibility: an older binary can still show an artifact whose type it
  does not know.
* Bad, because compile-time registration means a genuinely third-party share type
  requires rebuilding and redeploying the binary; there is no runtime plugin path.
  We judge this acceptable given the self-host-a-single-binary ethos, but it is a
  real limit for anyone wanting to extend a hosted instance they do not build.
* Bad, because the interface is a compatibility contract: broadening it later (a new
  method every type must implement) is a breaking change across all registered
  types, so the initial surface must be chosen conservatively and grown via optional
  capability interfaces rather than by widening the base contract.
* Neutral, because bundle sits awkwardly between "a type" and "a container of typed
  files": it is modeled as its own share type whose viewer composes per-file
  rendering by delegating each file back through the registry, which keeps one
  registry but means the bundle viewer is the one type aware of others' viewers.

### Confirmation

* Confirmed by the presence of a single `ShareType` interface and a registry with no
  `switch shareType` statements in the core service, storage layer, or shell chrome;
  a code review or lint check for such switches outside a type's own package flags
  regressions.
* A conformance test iterates every registered type and asserts it returns a
  non-empty badge, a renderable viewer fragment, a metadata-panel shape, and a
  well-formed anchor-affordance set; and asserts that an unregistered/opaque type
  resolves to the generic file handler and remains viewable, downloadable, and
  annotatable at the whole-artifact level.
* Anchor rules are confirmed against ADR-0006: a test asserts the annotation layer
  rejects an illegal anchor for a type (e.g. a comment thread on a webhook request)
  and accepts the legal ones, using the type's declared affordances as the source of
  truth.
* Adding the trajectory type (ADR-0009) serves as the worked proof: it lands as a new
  registered type plus its viewer, with zero diffs to the Artifact aggregate, the
  storage content model, or the shell header.
