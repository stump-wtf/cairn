---
status: accepted
date: 2026-07-08
decision-makers: joestump
# optional forward-only graph edges (extends / enables / related: lists of ADR IDs).
# Do NOT author inverse edges. Do NOT add `governs`.
extends: [ADR-0003]
---

# ADR-0011: Frontend Architecture and the Unified App Shell

## Context and Problem Statement

Every share type in Cairn — markdown, code, image, generic file, bundle, live
webhook, trajectory — is presented in the *same* web app shell: a header (logo · type
badge · title · one URL control with copy + `◆ mcp` · Share) wrapping a collapsible
right-hand metadata + comments panel and a type-specific body, at a consistent size
and chrome. Some of those bodies are static (rendered markdown), some are richly
interactive (image pins, span expand/collapse, reaction pickers), and two are *live*
(a webhook inspector filling in real time per ADR-0010, a trajectory waterfall filling
as spans append per ADR-0009). What web architecture renders one shell for all seven
types, handles live regions without a heavy client, and meets an accessible,
keyboard-navigable bar — while staying true to the terminal-minimal aesthetic and the
single-static-binary, self-hostable ethos of ADR-0003 and ADR-0012?

This ADR extends the triple-surface-parity decision of ADR-0003 by fixing the web
surface's internals. It consumes the SSE transport of ADR-0012 for live regions and
renders the annotation affordances of ADR-0006; it does not re-decide the backend
(ADR-0012), the trajectory model (ADR-0009), or the webhook model (ADR-0010).

## Decision Drivers

* **One shell, seven bodies** — the header, the URL control, and the collapsible
  metadata + comments panel are identical for every type; only the body swaps. The
  architecture must make the shared chrome a single implementation reused everywhere,
  not re-templated per type.
* **Live regions with a light client** — webhook requests and trajectory spans arrive
  over SSE and must update the page in place, without adopting a full SPA runtime or a
  client-side data-fetching/routing stack just to append a row.
* **Terminal-minimal, fast, self-hosted** — dark aesthetic (`#0A0B0D`), IBM Plex Sans +
  JetBrains Mono, compact chrome; the whole thing ships in the ADR-0012 Go binary. A
  megabyte of JS framework fights all three of those.
* **Progressive interactivity, not app-in-a-page** — most interactions are local
  (toggle the panel, open a reaction picker, expand a span, drop an image pin) or a
  small server round-trip (post a comment, load more). The client should scale *down*
  to the interaction, not impose an application shell.
* **Accessibility is a first-class posture** — WCAG 2.1 AA: keyboard navigation, visible
  focus, correct roles, managed focus for the collapsible panel and Share dialog, and
  live-region semantics so streamed updates are announced.
* **Server owns the truth and the markup** — provenance, annotations, and type-specific
  panels are already computed server-side (ADR-0006/0007/0009/0010); rendering HTML
  there avoids duplicating that logic in a client and re-deriving state over an API.

## Considered Options

* **Option A — Server-rendered Go `html/template` + HTMX + Alpine.js, with vanilla
  JS/SVG/Canvas for the trajectory waterfall.** The server renders the shell and every
  body as HTML. HTMX drives panel toggles, comment posting, "load more," and swapping
  live-stream regions from SSE. Alpine.js handles local interactivity (reaction pickers,
  span expand/collapse, image pins). The waterfall's custom time-axis drawing is a small
  vanilla JS + SVG/Canvas widget.
* **Option B — A single-page app (React/Svelte/Vue)** over the ADR-0012 REST + SSE API:
  a client router, a component per share type, client-side state, hydration.
* **Option C — Pure server-rendered templates, no client framework**: full-page
  navigations, `<form>` posts, and native browser behavior only; live regions via a bare
  `EventSource` script that reloads or naively injects.
* **Option D — HTMX only (no Alpine, no bespoke waterfall JS)**: every interaction,
  including reaction pickers and span expansion, is a server round-trip returning a
  partial.

## Decision Outcome

Chosen option: **"Option A — server-rendered `html/template` + HTMX + Alpine.js, with a
vanilla JS/SVG/Canvas waterfall"**, because it renders one shell for all seven types
from server-owned truth, handles live SSE regions with a swap attribute instead of a
client runtime, keeps local interactions instant without a round-trip, and ships inside
the single Go binary — all while staying light enough to honor the terminal-minimal
aesthetic. Option B is the strongest alternative and the reason to reconsider is real
(the waterfall and image-pin surfaces are genuinely app-like), but a SPA re-derives on
the client the provenance/annotation/panel state the server already computes, doubles
the rendering logic, adds a build/hydration pipeline and a large client bundle that
fights the aesthetic and the self-hosted binary, and buys little for five of the seven
types that are essentially documents. Option C cannot deliver the reaction pickers,
image pins, and span expand/collapse the design shows without punishing round-trips or
full reloads, and its live regions would be crude. Option D keeps things uniform but
makes every reaction-picker open and span toggle a network round-trip — sluggish,
offline-brittle, and needlessly chatty for state that is purely local to the view;
Alpine exists precisely to own that local state in a few kilobytes.

### Why server-rendered + HTMX over a SPA

* **No duplicated state machine.** The server already holds and computes the artifact,
  its provenance (ADR-0007), its annotation stream (ADR-0006), and each type's panel
  data. Rendering HTML there means one source of truth; a SPA would re-fetch and
  re-model all of it client-side.
* **The interactions are mostly local or a small swap.** Toggling the panel, opening a
  picker, expanding a span, and dropping a pin are local (Alpine). Posting a comment,
  loading more of the Bin, and swapping in a new live request are small partial swaps
  (HTMX). Neither needs a router or a client store.
* **Weight and ethos.** HTMX + Alpine are a few tens of kilobytes, no build step,
  embeddable in the ADR-0012 binary — coherent with terminal-minimal and self-hosting.
  A SPA's bundle, build toolchain, and hydration are the opposite.
* **The genuinely app-like bits are scoped.** Only the trajectory waterfall's custom
  time-axis and the image-pin region need bespoke drawing/hit-testing; those are small
  vanilla JS + SVG/Canvas widgets, not a reason to make the entire frontend a SPA.
* **Accepted trade-off.** We give up rich offline behavior and buttery client-side
  transitions; for a share-viewing tool that is a fair trade for simplicity, speed to
  first paint, and one rendering path.

### The shared layout and partial structure

A single base layout owns the shell; bodies and panel are composed partials so the
chrome is defined once:

* **Base layout** — the page frame: `#0A0B0D` canvas, the font stack (IBM Plex Sans UI,
  JetBrains Mono for code/spans/IDs), the SSE/HTMX/Alpine includes, and the two slots
  the shell fills (body, panel).
* **Header partial** — logo/wordmark · **type badge** (`MD`/`PY`/`IMG`/`FILE`/`HK`/`TRJ`)
  · title · the **one URL control** (short link + `copy` + `◆ mcp`) · **Share** button.
  Identical for every type; the badge and title are the only data that vary.
* **Metadata + comments panel partial** — the collapsible right-hand panel: type-specific
  metadata (a file's checksum, a run's stats, a webhook's status mix) above the comments
  thread. Collapse state is an Alpine concern; posting a comment is an HTMX swap.
* **Body partials, one per share type** — `markdown`, `code`, `image`, `file`, `bundle`
  (tabbed), `webhook` (live inspector), `trajectory` (waterfall + stream). Each is
  selected by the artifact's share type (the ADR-0002 registry maps type → body partial
  and → panel fields), so adding a type is adding a partial, not touching the shell.
* **Annotation partials** — reaction clusters and comment items are shared partials used
  across bodies and the panel, so ADR-0006's affordances render consistently wherever an
  anchor lives (a markdown block, a code line, an image pin, a span, a webhook request).

The Bin (the listing) reuses the same base layout with a listing body: rows with type
badge, title, provenance, and reaction/comment counts — provably the "same listing" as
the CLI TUI because both project the same server-side query (ADR-0003).

### How live regions update

Live bodies (webhook inspector, trajectory waterfall/stream) subscribe to the ADR-0012
**SSE** endpoint for their artifact and update in place:

* A live region is an HTMX element bound to the SSE stream; each server event carries a
  ready-to-insert HTML fragment (a new request row, a new span) that HTMX swaps into the
  region — no client-side templating of stream data.
* New content lands in an **`aria-live`** region (`polite`) so assistive tech announces
  arrivals without stealing focus; the status mix / span stats update as sibling swaps.
* The waterfall redraws its time axis in the vanilla JS/SVG/Canvas widget as spans append
  (rescaling the `0s … Ns` ruler), while the textual activity stream updates via the same
  HTMX/SSE swaps — the picture and the readable stream stay in lockstep.
* On disconnect, `EventSource` reconnection plus a "load recent, then tail" fetch (the
  ADR-0010 late-joiner behavior) reconciles missed events, so a reopened tab is correct.

Static bodies (markdown, code, image, file, bundle) open no stream; the same partials
render once and are interactive only through Alpine (pins, pickers) and HTMX (comments).

### Local interactivity (Alpine) and the bespoke widgets

* **Alpine.js** owns view-local state with no server round-trip: the panel collapse
  toggle, the reaction `＋` picker, code/markdown selection-to-comment affordances, span
  expand/collapse in the trajectory stream, and image-pin placement/hover. These are
  ephemeral UI states, not domain data, so they belong on the client.
* **Vanilla JS + SVG/Canvas** draws the two things HTML cannot: the trajectory
  **waterfall** (nested span bars on a shared time ruler, click-to-jump into the stream)
  and the **image-pin overlay** (place a pin at a coordinate, anchor a comment to a
  region). Committing a pin or a span-anchored comment is then an HTMX post; the drawing
  is local, the persistence is a server swap.

### Accessibility posture (WCAG 2.1 AA)

* **Keyboard navigation** — every control (URL copy, `◆ mcp`, Share, panel toggle,
  reaction picker, comment form, span rows, bundle tabs, image pins) is reachable and
  operable by keyboard with a logical tab order and visible focus rings that read on the
  `#0A0B0D` canvas. (Fully keyboard-driving the *entire* web shell like the TUI is an
  explicit "try next," not an excuse to skip AA basics.)
* **Focus management** — opening the Share dialog traps focus and restores it on close;
  toggling the metadata panel moves focus predictably and exposes state via
  `aria-expanded`; the dialog uses proper `role="dialog"` / labelling.
* **Live-region semantics** — streamed webhook requests and trajectory spans arrive in a
  `polite` `aria-live` region so they are announced without hijacking focus, and the
  status-mix/stat updates are associated with accessible names.
* **Roles, contrast, and non-color cues** — badges, span categories, and status use
  correct semantics and are not distinguished by color alone (the `reason/exec/read/net/
  write` legend pairs color with a label); text and controls meet AA contrast against the
  dark canvas; images/pins carry accessible descriptions.
* **Progressive enhancement** — core reading (open a share, read its body, read comments)
  works with HTML and degrades gracefully if Alpine/HTMX fail to load; the JS-only bits
  (waterfall, pins) fail to an accessible fallback (the textual activity stream, a
  static image) rather than a blank region.

### Consequences

* Good, because one server-rendered shell with per-type body partials makes the header,
  URL control, and metadata/comments panel a single implementation reused across all
  seven types, so adding a share type (ADR-0002) is adding a partial, not reshaping the
  chrome.
* Good, because live regions update via SSE-driven HTMX swaps of server-rendered
  fragments — no client data layer — keeping the webhook inspector and trajectory
  waterfall live while the client stays small and the aesthetic stays terminal-minimal.
* Good, because the whole frontend embeds in the ADR-0012 Go binary with no build
  pipeline, and the server-owned rendering means provenance/annotation/panel logic lives
  in exactly one place.
* Bad, because interactivity is split across three mechanisms (HTMX swaps, Alpine local
  state, bespoke waterfall/pin JS); contributors must know which tool owns which
  behavior, and the seams (an Alpine picker that then HTMX-posts) need discipline to stay
  clean.
* Bad, because we forgo SPA niceties — client-side routing, optimistic updates, rich
  offline/transition polish; heavily interactive future surfaces could strain the
  HTMX/Alpine approach and tempt a partial rewrite.
* Neutral, because full keyboard-driving of the web shell to TUI parity is deferred
  ("try next"); v1 commits to WCAG 2.1 AA (operable, focus-managed, announced) but not to
  a complete web-as-TUI keymap.

### Confirmation

* Confirmed by the frontend being Go `html/template` partials rendered by the ADR-0012
  binary with HTMX + Alpine and a small vanilla JS/SVG/Canvas waterfall — no SPA
  framework, no client build step — and by a single base layout whose header, URL
  control, and metadata/comments panel are reused by every share-type body partial.
* A shell-consistency test renders all seven share types and asserts an identical header
  (logo · badge · title · one URL control with copy + `◆ mcp` · Share) and the same
  collapsible panel structure, with only the body and panel fields varying by type.
* A live-region test drives a webhook and a trajectory over SSE and asserts new
  requests/spans are swapped into an `aria-live` region in order, the waterfall rescales,
  and a reconnecting client reconciles via the load-recent-then-tail path (ADR-0010).
* An accessibility audit (automated AA checks plus manual keyboard/screen-reader passes)
  confirms keyboard operability, visible focus on `#0A0B0D`, focus trap/restore for the
  Share dialog, `aria-expanded` on the panel toggle, and non-color-only category cues.
* The specs derived from this ADR (in `docs/openspec/specs/`) assert the shared-shell
  invariant (one header/panel across types), the SSE-driven live-region behavior, and
  the WCAG 2.1 AA posture as acceptance criteria.
