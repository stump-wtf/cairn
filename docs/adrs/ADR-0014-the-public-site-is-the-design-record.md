---
status: accepted
date: 2026-07-25
decision-makers: joestump
# optional forward-only graph edges (extends / enables / related: lists of ADR IDs).
# Do NOT author inverse edges. Do NOT add `governs`.
extends: [ADR-0001]
related: [ADR-0011]
---

# ADR-0014: The Public Site Is the Design Record

## Context and Problem Statement

Cairn's design record is large and it is the product's best argument for itself:
fourteen ADRs, nine paired capability specs (spec.md + design.md, 187 requirements
across 303 scenarios), and a design brief — roughly 8,500 lines of markdown living in
`docs/adrs/` and `docs/openspec/specs/`. A project that sells "provenance, published"
and whose homepage claim is *everything above was decided in public* cannot keep that
record behind a private forge.

Today it effectively is. `website/` is a Docusaurus site whose `architecture.md` and
`specifications.md` are **hand-maintained index pages**: a table of ADR titles, a table
of spec titles, and a link per row pointing at
`github.com/joestump/cairn/blob/main/docs/…`. Three things follow from that shape and
all three are bad.

**The record is written twice.** Every ADR exists as its own file *and* as a row of
summary prose in `architecture.md`. The summary drifts the moment an ADR is added,
superseded, or re-titled — and it drifts silently, because nothing checks it. The
spec index carries hardcoded requirement counts, which are wrong the instant a
requirement lands.

**Every link leaves the site, and most of them 404.** The canonical repo is
`gitea.stump.rocks/stump.wtf/cairn` and it is private. A reader who follows
"ADR-0009" from the published site gets a login wall for a self-hosted forge they have
no account on. The design record is, in practice, unpublished.

**The site does not look like the product.** Cairn has a specific design language —
terminal-minimal, dark, one accent, monospace only where it carries meaning, and a
five-colour functional palette taken from the span categories (ADR-0009). The site is
a lightly-themed stock Docusaurus template. The one place a stranger meets the
project first is the one place that does not demonstrate its taste.

So: what is the relationship between the published website and the design record in
the repo, and what does the site actually render?

This ADR extends ADR-0001 (Cairn as an AI-native artifact store — the provenance and
"decided in public" posture) and is *adjacent to but distinct from* ADR-0011. ADR-0011
fixes the internals of the **application** web surface — the unified app shell that
renders artifacts, server-rendered Go templates with HTMX and Alpine, shipped inside
the single static binary. This ADR covers the **public marketing and documentation
site**, which is a separate static artifact deployed to a CDN and is not part of
`cairnd`. The two share a design language and nothing else. Confusing them is the main
thing this ADR exists to prevent.

## Decision Drivers

- **Nothing is written twice.** A fact about an ADR or a requirement lives in exactly
  one file. Index pages, counts, status badges, and cross-references are *derived*.
- **Adding an ADR publishes it.** The marginal cost of a new decision record or spec
  must be zero build steps and zero hand-edited index rows.
- **The record must be readable by someone with no forge account.** Full ADR and spec
  text renders on the site itself; the forge link is a courtesy, not the payload.
- **The site must demonstrate the design language**, not merely describe it.
- **Keep what already works.** The Docusaurus MDX pipeline, local search, the
  `website/build` → Pages deploy, and dark-mode-first theming are fine. This is not an
  invitation to rewrite the toolchain.
- **Drift must fail the build, not the reader.** A broken cross-reference between a
  spec and an ADR should break CI.

## Considered Options

- **A — Status quo.** Hand-maintained index pages that link out to the forge.
- **B — Build-time content pipeline into Docusaurus.** Mount `docs/adrs/` and
  `docs/openspec/specs/` as generated doc routes, render full text on-site, derive
  every index and badge from front-matter, and restyle Docusaurus to the Cairn design
  language via theme tokens and targeted swizzles.
- **C — Copy the markdown into `website/docs/` manually.** Same rendering result as B,
  but the copy is a human step.
- **D — Replace Docusaurus with a bespoke static site** that matches the mockups
  pixel-for-pixel with no theme to fight.

## Decision Outcome

**Chosen: Option B — a build-time content pipeline into a restyled Docusaurus.**

The design record stays exactly where it is. `docs/adrs/` and
`docs/openspec/specs/` remain the single source of truth, authored as plain markdown
with the front-matter conventions already in use. A prebuild step mounts those trees
into the Docusaurus content model as two additional docs instances — `/decisions` and
`/specs` — so the **full text of every ADR and every spec renders on the public site**.
The hand-maintained `architecture.md` and `specifications.md` index pages are deleted
and replaced by generated indexes.

Everything that is currently prose-in-an-index-page becomes derived data:

| Rendered element | Derived from |
| --- | --- |
| ADR index rows, ordering, titles | filename + `# ADR-XXXX:` heading |
| `ACCEPTED` / `PROPOSED` badges | front-matter `status` |
| Dates on index rows | front-matter `date` |
| Traceability chips (*extends*, *enables*, *related*) | front-matter graph edges |
| Spec cards, "N reqs" counts | count of `### Requirement:` sections |
| "implements ADR-XXXX" on a spec | front-matter `implements` |
| Prev/next pagination, sidebar tree | generated sidebar |

Because the graph edges are forward-only by convention (ADR-0011's front-matter
comment: *do not author inverse edges*), the pipeline computes the inverse edges —
"referenced by SPEC-0004", "superseded by ADR-XXXX" — at build time. Authors keep
writing one direction; readers get both.

### Why generation over the status quo

Option A's failure is not aesthetic, it is correctness. The spec index today asserts
requirement counts that no process maintains, and the ADR index will silently omit any
ADR whose author forgets to add a row. An index that can be wrong is worse than no
index, because it is trusted. Generation makes the class of error impossible.

Option A also cannot fix the 404 problem without duplicating the content, which is
Option C.

### Why not copy the files (Option C)

C produces the same site as B with a manual sync step, which means the copies rot
between syncs and reviewers must diff two trees. It trades a build script for a
recurring human obligation. The whole point of the decision is that the record is
written once.

### Why not a bespoke site (Option D)

D is genuinely tempting: the mockups are specific, and a hand-rolled site would match
them exactly without swizzling anything. It loses local search, MDX, the docs
versioning story, the sidebar/TOC/pagination machinery, and the existing Pages deploy —
and it would put ~2,000 lines of bespoke static-site plumbing under maintenance for a
project whose *actual* product is elsewhere. The mockup's information architecture
(sticky left nav tree, right-hand "on this page", breadcrumb, prev/next) is
substantially the Docusaurus doc layout already. Restyling is the cheaper path to the
same picture.

The cost of B is accepted explicitly: **some swizzling.** The navbar, sidebar, TOC,
and doc-item footer need theme overrides to reach the mockup. Swizzled components are
pinned to a Docusaurus major and must be re-checked on upgrade. That is a real, bounded
maintenance cost and it is smaller than owning a static site generator.

### The design language becomes tokens

The five span categories from ADR-0009 are already the product's functional palette.
They become the site's palette too, as CSS custom properties, so the site and the app
shell of ADR-0011 stay in sync by construction:

| Token | Value | Meaning |
| --- | --- | --- |
| `--cairn-reason` | `#A78BFA` | model turns — planning, ranking, composing |
| `--cairn-exec` | `#FBBF24` | shell and tool execution |
| `--cairn-read` | `#34D399` | file and data reads |
| `--cairn-net` | `#38BDF8` | anything over the wire — **doubles as the brand accent** |
| `--cairn-write` | `#F472B6` | produces an artifact |

Surfaces (`#0A0B0D` void, `#111317` surface, `#08090B` inset, `#1C1F26` / `#2A2E38`
lines) and the two families — IBM Plex Sans for prose, JetBrains Mono for machine
content — complete the set. The typographic rule is normative and worth stating
because it is the thing most often got wrong: **if a human wrote it, it is Plex; if a
machine emitted it — an id, a path, a span name, a TTL, a command — it is Mono. Never
mono for a sentence.**

There is no sixth colour. Adding one is a decision that belongs in a new ADR.

### The homepage is a separate artifact

The homepage is not a doc page and is not generated from the record. It is a bespoke
React route that argues for the product: the pipe-in/link-back hero, the type registry,
the trajectory waterfall, surface parity, and the promises. It is allowed to hardcode
its own copy. It is *not* allowed to hardcode facts that the record owns — the ADR
count, the spec count, and the requirement totals are imported from the same generated
data the docs use, so "13 ADRs, 9 capability specs" cannot go stale on the marketing
page either.

### Deployment

The site continues to build to `website/build` and deploy as a static bundle. The
existing `.github/workflows/deploy.yml` targets GitHub Pages at
`joestump.github.io/cairn/`; since the canonical repo is now
`gitea.stump.rocks/stump.wtf/cairn`, the deploy is re-pointed at a Gitea Actions
workflow. The public URL and the `baseUrl` move together and are set in one place in
`docusaurus.config.ts`. Forge links rendered on the site point at the public GitHub
mirror (`github.com/stump-wtf/cairn`), never at the private Gitea origin — that is the
fix for the 404 problem, and it is a build-time constant, not a per-page decision.

### Consequences

**Good.**

- The design record is genuinely published: full ADR and spec text, readable without a
  forge account, searchable by the site's local search.
- Adding `docs/adrs/ADR-0015-….md` publishes a decision. No index edit, no second
  write, no drift.
- Requirement counts, status badges, dates, and traceability are correct by
  construction because they are derived from the files they describe.
- Inverse graph edges ("referenced by") appear for readers without authors having to
  maintain them, preserving the forward-only authoring rule.
- The site demonstrates the design language it documents, and shares its palette with
  the ADR-0011 app shell by construction.

**Bad, and accepted.**

- **Swizzled components pin us to a Docusaurus major.** Navbar, sidebar, TOC, and
  doc-item footer overrides must be re-verified on every major upgrade. Mitigation:
  swizzle as few components as possible and prefer CSS custom properties over
  ejecting; every swizzle needs a comment naming the mockup element it exists to
  serve.
- **The build now depends on front-matter being well-formed.** A missing `status:` or a
  malformed `implements:` breaks the site rather than degrading it. This is deliberate
  — see Confirmation — but it means an ADR author can break the site build.
- **Two markdown dialects.** The record is authored as CommonMark for the forge and
  rendered through MDX. Constructs that are legal markdown but hostile to MDX (bare
  `<`, `{`) must be escaped by the pipeline rather than by asking authors to write
  MDX-safe prose. The config already sets `markdown.format: detect` for this reason.
- The homepage's bespoke sections are hand-maintained. Only the *counts* are derived;
  the argument is written by a human and can go stale if the product changes shape.

**Neutral.**

- `website/docs/` retains a small number of genuinely hand-written narrative pages
  (overview, share types, surfaces, annotations). These are not derived and are not
  meant to be — they are the "explain it to a newcomer" layer above the record.

### Confirmation

The decision is confirmed by build-time checks, not by review:

1. **Coverage** — the generated ADR index contains one entry per file matching
   `docs/adrs/ADR-*.md`, and the generated spec index one card per directory under
   `docs/openspec/specs/`. A file that is present but unrendered fails the build.
2. **Referential integrity** — every ADR id named in a spec's `implements:` and every
   ADR id in a graph edge resolves to a real ADR. Every `requires:` resolves to a real
   spec. Dangling references fail the build.
3. **No hardcoded counts** — CI greps the homepage and the narrative doc pages for
   literal ADR/spec/requirement totals; a literal count that is not imported from the
   generated data fails the build.
4. **No private-forge links** — CI fails on any occurrence of `gitea.stump.rocks` in
   rendered site output, since those links 404 for the public.
5. **Front-matter validity** — every ADR has a `status` in the accepted enum and a
   parseable `date`; every spec has `status`, `date`, and `implements`.
