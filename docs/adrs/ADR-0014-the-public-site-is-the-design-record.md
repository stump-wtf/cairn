---
status: accepted
date: 2026-07-25
decision-makers: joestump
# optional forward-only graph edges (extends / enables / related: lists of ADR IDs).
# Do NOT author inverse edges. Do NOT add `governs`.
extends: [ADR-0001]
related: [ADR-0009, ADR-0011]
---

# ADR-0014: The Public Site Is the Design Record

## Context and Problem Statement

Cairn is built spec-first. The reasoning lives in `docs/adrs/` (MADR decision records)
and `docs/openspec/specs/` (paired `spec.md` + `design.md` per capability), and it is
the product's best argument for itself: a project selling *provenance, published* whose
homepage claim is *everything above was decided in public*. That record is also,
today, the largest body of writing the project owns.

The public site does not publish it. `website/` is a Docusaurus site with a bespoke
Cairn homepage and six hand-written narrative doc pages, and two of those pages —
`website/docs/architecture.md` and `website/docs/specifications.md` — are hand-typed
indexes *of* the record. That shape produces three defects, each of which is visible in
the files as they stand.

**The indexes are written by hand and are already wrong.** `architecture.md:27-40` is a
markdown table with one manually authored row per decision. It ends at ADR-0012, while
`docs/adrs/` on the default branch already contains ADR-0013 — a decision that is
accepted, merged, and invisible to a reader of the site. This record will be the second
the moment it merges, by exactly the same mechanism. Nothing detected the first omission,
because nothing checks it. `specifications.md:11-12` goes further and states a requirement total
and a scenario total in prose; recount the specifications it indexes and the sentence is
already false. Its adjacent claim at `:10-11` that "web-facing specs carry a Security section;
UI-facing specs carry an Accessibility section" is contradicted by
`docs/openspec/specs/cli/spec.md`, which has neither. An index that can be wrong is
worse than no index, because readers trust it.

**Every row is an exit.** The links in those tables — and in the navbar, footer, and hero
CTA — resolve to `https://github.com/joestump/cairn/…` (`docusaurus.config.ts:82`, `:112`,
`:113`; `src/pages/index.tsx:47`; `docs/architecture.md:29-40`;
`docs/specifications.md:16-24`). The most pervasive one is not in any of those lists:
`docusaurus.config.ts:47` configures
`editUrl: 'https://github.com/joestump/cairn/tree/main/website/'`, which renders an "Edit
this page" link on *every* docs page the site serves. That repository is not public. A
reader who follows "ADR-0009" leaves the site and lands on an authentication wall. The
published design record is, functionally, a table of contents for a private document.

**The record's text is nowhere on the site.** There is no `/decisions` tree and no
`/specs` tree. `website/sidebars.ts:4-18` is a hand-authored sidebar over six narrative
pages; the docs root is `website/docs/` and contains only those six files. Not one line
of any ADR or any spec renders on the published site.

Note what is *not* wrong. The homepage (`src/pages/index.tsx`,
`src/components/HomepageFeatures/`) is fully bespoke Cairn work, not a scaffold, and
`src/css/custom.css` already carries a real Cairn design language — the ADR-0009 span
categories as accent tokens, IBM Plex Sans and JetBrains Mono, a dark-first surface
ramp. Those are assets. This decision is about what the site *renders*, and about
tightening its chrome — not about starting over.

So: what is the relationship between the published website and the design record in the
repository, and what does the site actually serve?

This ADR extends ADR-0001 (Cairn as an AI-native artifact store — the provenance and
"decided in public" posture). It is *adjacent to but distinct from* ADR-0011, which
fixes the internals of the **application** web surface: the unified app shell that
renders artifacts, server-rendered Go templates with HTMX and Alpine, shipped inside the
single static binary. This ADR covers the **public marketing and documentation site**, a
separate static bundle that is not part of `cairnd`. The two share a design language and
nothing else, and confusing them is the main thing this ADR exists to prevent. It is
`related:` to ADR-0009 because it depends on that ADR's category palette, which it
consumes and does not redefine.

## Decision Drivers

* **Nothing is written twice.** A fact about a decision or a requirement lives in
  exactly one file. Index rows, counts, status badges, dates, and cross-references are
  *derived*.
* **Adding an ADR publishes it.** The marginal cost of a new decision record or spec
  must be zero hand-edited index rows and zero extra build steps.
* **The record must be readable by someone with no forge account.** Full ADR and spec
  text renders on the site itself. There is no external hop to follow.
* **Drift must fail the build, not the reader.** A dangling `implements:`, a missing
  `status:`, or a broken cross-reference should break CI.
* **Keep what already works.** The bespoke homepage, the `custom.css` token set, the
  Docusaurus doc chrome, and the static-bundle deploy stay. This is not an invitation to
  rewrite the toolchain or repaint the palette.
* **The site must demonstrate the design language, not merely describe it.**

## Considered Options

* **Option A — Status quo.** Hand-maintained index pages linking out to the forge.
* **Option B — A build-time content pipeline inside Docusaurus.** Transform `docs/adrs/`
  and `docs/openspec/specs/` into doc routes at build time, render full text on-site,
  derive every index row, badge, count, and cross-reference, and restyle Docusaurus's
  surfaces and chrome to the Cairn design language.
* **Option C — Copy the markdown into `website/docs/` by hand.** Same rendered result as
  B, with a human sync step.
* **Option D — Replace Docusaurus with a bespoke static site generator** built to the
  mockups with no theme to fight.

## Decision Outcome

Chosen option: **"Option B — a build-time content pipeline inside a restyled
Docusaurus"**, because it is the only option that makes the published indexes correct by
construction while keeping the record authored exactly where it is authored today, and
the only one that reaches the mockups without putting a static site generator under
maintenance.

The design record stays exactly where it is. `docs/adrs/` and `docs/openspec/specs/`
remain the single source of truth, authored as plain CommonMark with the front-matter
conventions already in use. A generator owned by the site reads those trees and stages
transformed markdown into **git-ignored subdirectories of the site's existing docs root**,
where the documentation content plugin serves it alongside the hand-written narrative
pages. The generator runs **before Docusaurus initialises its plugins**, so a clean
checkout builds correctly on the first attempt; it re-runs while the development server is
running, so an author editing an ADR sees the page change without a restart. The result is
that **the full text of every ADR and every spec renders on the public site**. The
hand-maintained `architecture.md` and `specifications.md` are deleted, not repaired, and
replaced by generated indexes.

Everything that is prose-in-an-index-page today becomes derived data:

| Rendered element | Derived from |
| --- | --- |
| Decision index rows, ordering, titles | filename + the `# ADR-XXXX:` heading |
| `ACCEPTED` / `PROPOSED` badges | front-matter `status` |
| Dates on index rows and metadata bars | front-matter `date` |
| Traceability chips (*extends*, *enables*, *related*) | front-matter graph edges |
| Spec cards and their requirement counts | count of `### Requirement:` sections in the source |
| "implements ADR-XXXX" on a spec | front-matter `implements` |
| "requires SPEC-XXXX" on a spec | front-matter `requires` |
| The API reference surface | the specs' own endpoint tables |
| Sidebar tree, breadcrumbs, prev/next | the generated route tree |

Because the record's graph edges are forward-only by convention — the newer records state
it in a front-matter comment (*"Do NOT author inverse edges"*, `ADR-0009:5-6`,
`ADR-0010:5-6`, `ADR-0011:5-6`, `ADR-0013:5-6`), and the older ones follow the practice
without stating it — the pipeline computes the inverse edges at build time: "extended by
ADR-0002", "referenced by SPEC-0004". Authors keep writing one direction; readers get
both.

That computation must de-duplicate, because the record already contains the same
relationship authored from both ends. `ADR-0001:6` declares `enables: [ADR-0002,
ADR-0003]`, while `ADR-0002:6` and `ADR-0003:6` each declare `extends: [ADR-0001]`. A
naive inversion makes ADR-0001 list ADR-0002 twice — once as a forward `enables` and
once as an inverted `extends`. Edges are therefore normalised to an unordered pair plus
a relationship kind and de-duplicated before rendering.

### Why not the status quo (A)

A's failure is correctness, not aesthetics. The requirement totals in
`specifications.md` are maintained by nobody and are wrong; the ADR table silently omits
any decision whose author forgets a row, and has. Generation makes that class of error
impossible rather than merely discouraged. A also cannot fix the private-link problem
without duplicating the content, which is C.

### Why not copy the files (C)

C produces the same site as B with a manual sync step. The copies rot between syncs,
reviewers must diff two trees, and every pull request that touches an ADR grows a second
diff that means nothing. It trades a build step for a recurring human obligation. The
whole point of the decision is that the record is written once.

### Why not a bespoke site (D)

D is genuinely tempting: the mockups are specific, and a hand-rolled generator would
match them without fighting a theme. It loses MDX, the sidebar/TOC/breadcrumb/pagination
machinery, the doc-versioning option, and the existing static-bundle deploy — and it
puts a static site generator under maintenance for a project whose actual product is
elsewhere. The docs mockup's information architecture (sticky left rail with a nested
record tree, right-hand "ON THIS PAGE" column, breadcrumb, prev/next footer) is
substantially the Docusaurus doc layout already. Restyling is the cheaper path to the
same picture.

The cost of B is accepted explicitly and it is a **theme-override budget**. Two distinct
hazards are being managed here and they are worth separating, because conflating them is
how a budget gets justified badly.

*Ejecting* is the unsafe action: an eject copies a component's internals into the site,
and those internals may change in a minor release, so a site that ejects signs up for
breakage on a routine upgrade. Ejecting is forbidden outright.

*Wrapping* is not that. Docusaurus explicitly blesses it: of the 55 components
`@docusaurus/theme-classic` configures for swizzling, 41 are marked `wrap: safe`,
including `DocSidebar`, `Footer`, `CodeBlock`, and `SkipToContent`. Capping wraps is
therefore a **policy choice, not a safety constraint**, and the reason is different: every
wrap is a component whose stock behaviour — active-link detection, collapse state,
keyboard semantics, translation strings — must from then on be understood and maintained
locally, and each one shrinks the CSS-first surface that makes upgrades cheap.

The budget is accordingly: as much as possible in CSS custom properties and CSS modules;
**at most one wrap**, spent deliberately and justified in the design document; and **no
ejects at all**. Where the mockup asks for something only an eject could deliver, the
mockup loses.

### Restyling, not repainting

**The category palette is not up for redefinition here.** ADR-0009 fixes a recommended
set of span categories and requires that any other non-empty category render in a
neutral default colour; SPEC-0004 makes that normative. `website/src/css/custom.css:11-25`
already implements exactly that — one `--cat-*` token per recommended category plus
`--cat-other` as the neutral default — and that file is canonical. This ADR restyles
**surfaces, typography, and chrome**. It does not add, remove, recolour, or cap the
category tokens, and any future change to them is an ADR against ADR-0009, not a website
decision.

What this ADR does fix about the token layer is discipline: the site's visual system
must be expressible entirely as custom properties in one file, and component stylesheets
must stop hardcoding colour. Exactly five hex literals live outside the token definition
today, and they fall into three different kinds of drift.
`src/pages/index.module.css:113-115` restates `#f0568f`, `#e6b450`, and `#46c878` —
values `--cat-write`, `--cat-net`, and `--cat-exec` already carry, so they are simple
duplication. `src/pages/index.module.css:53` uses `#b49bff` as a gradient midpoint, and it
corresponds to *no* token, so it cannot merely be swapped for one — it needs a token
minted for it or the gradient rewritten between two existing tokens.
`src/components/HomepageFeatures/styles.module.css:121` is the interesting case: its single
literal is `.cat_mono { --accent: #aab2bd; }`, a class naming a category ADR-0009 does not
define, coloured by hand with the neutral default's value rather than by the neutral
default token. Those three kinds are exactly the drift this decision is about, one layer
down.

Holding the palette fixed has one consequence that has to be named rather than
discovered. The category tokens are declared once, at `:root`, and are deliberately
constant across colour modes — they encode a category, not a surface. Measured as
foreground, all fourteen clear 4.5:1 against the dark ground (`#0a0b0d`; worst is
`--cat-fail` at 5.68:1). Against the light ground (`#ffffff`) not one of them reaches
4.5:1, and only three reach 3:1. The colour-mode switch ships today
(`docusaurus.config.ts:61-65`). Since recolouring the tokens is out of bounds, the
resolution has to come from the other side: a category accent is a **dark-ground text
colour**, and anywhere the site renders one as text it either renders on the dark ramp or
demotes the accent to a non-text indicator with the category name carried by a text token.
Whether the site should offer a light mode at all is left open — see SPEC-0010.

The typographic rule is normative and worth stating because it is the thing most often
got wrong: **if a human wrote it, it is IBM Plex Sans; if a machine emitted it — an id,
a path, a span name, a TTL, a command — it is JetBrains Mono. Never mono for a
sentence.** `custom.css` already declares both families; this makes the split a rule
rather than a habit.

### The homepage is a separate artifact

The homepage is not a doc page and is not generated from the record. It is a bespoke
React route that argues for the product — the pipe-in/link-back hero, the type registry,
the trajectory waterfall, surface parity, the promises — and it keeps its own
hand-written copy. It is *not* allowed to hardcode facts the record owns. The decision
count, the capability-spec count, and requirement totals come from the same derived data
the docs tree uses, so the marketing page cannot go stale in the specific way
`specifications.md` did.

### Deployment must be triggered by the record

`.github/workflows/deploy.yml:5-8` triggers on pushes touching `website/**` and the
workflow file. Under this ADR that filter makes the headline promise false: a commit
that adds `docs/adrs/ADR-0015-….md` changes the published site and matches no path in
the trigger, so the deploy never runs and the "generated straight from the repo" claim
becomes a lie by omission. The deploy trigger must cover the record. Separately, the
site is built today only after merge; a pull request that breaks the site build cannot
fail until it is too late to matter, so the site must also build on pull requests.

### Consequences

* Good, because the design record is genuinely published: full ADR and spec text,
  readable by anyone, with no forge account and no external hop.
* Good, because adding `docs/adrs/ADR-0015-….md` publishes a decision — no index edit, no
  second write, no drift.
* Good, because requirement counts, status badges, dates, and traceability are correct by
  construction, being computed from the files they describe.
* Good, because inverse graph edges appear for readers without authors maintaining them,
  preserving the forward-only authoring rule the record already follows.
* Good, because the site demonstrates the design language it documents, and shares its
  category tokens with the ADR-0011 app shell by construction rather than by copy.
* Bad, because the build now depends on front-matter being well-formed: a missing
  `status:` or a malformed `implements:` breaks the site rather than degrading it. That is
  deliberate — see Confirmation — but it means an ADR author can break a build they never
  think about.
* Bad, because a theme-override budget is a standing liability: even a wrapped component
  must be re-verified on a Docusaurus major upgrade, and the CSS-heavy approach means some
  upgrades will visually regress before anyone notices. Mitigation: keep the wrap budget at
  one, and prefer losing a mockup detail over spending it.
* Bad, because two markdown dialects meet. The record is authored as CommonMark and
  rendered through a pipeline that also injects components; constructs that are legal
  markdown but hostile to MDX — a bare `<`, a bare `{`, an `<id>`-shaped word that
  CommonMark reads as raw HTML — must be handled by the pipeline, not by asking authors to
  write MDX-safe prose. The record's format is the thing being protected; it does not bend
  toward the renderer.
* Bad, because the homepage's argument stays hand-maintained: only the *counts* are
  derived, so the prose can go stale if the product changes shape.
* Bad, because generation must happen before Docusaurus initialises its plugins, which
  puts a step ahead of the standard build entry point and makes "just run `docusaurus
  build`" no longer sufficient on a clean checkout.
* Neutral, because `website/docs/` keeps a small number of genuinely hand-written
  narrative pages (overview, share types, surfaces, annotations). Those are not derived and
  are not meant to be — they are the "explain it to a newcomer" layer above the record.
* Neutral, because the site ships no search today. Publishing the whole record makes its
  absence more noticeable, but adding search is a dependency decision with its own
  trade-offs and is explicitly out of scope here.

### Confirmation

The decision is confirmed by build-time checks, not by review:

* **Coverage** — the generated decision index contains one entry per file matching
  `docs/adrs/ADR-*.md`, and the generated spec index one card per directory under
  `docs/openspec/specs/`. A source file that produces no page fails the build.
* **Referential integrity** — every ADR id in a front-matter graph edge and every id in
  a spec's `implements:` resolves to a real ADR; every `requires:` resolves to a real
  spec. Dangling references fail the build.
* **Front-matter validity** — every ADR has a `status` in the accepted enum and a
  parseable `date`; every spec has `status`, `date`, and `implements`.
* **Clean-checkout build** — a build from a fresh clone, with the staging directories
  absent, produces the full record. This is the check that would catch the generation step
  drifting back behind plugin initialisation.
* **No links into a non-public repository** — rendered output is checked against an
  allowlist of permitted external hosts. Any link to a repository host outside that
  allowlist fails the build, regardless of which forge it is. There is no "view source"
  affordance to exempt: the full text is on the page.
* **No colour literals in site source** — a lint over `website/src/` rejects a hex
  colour written anywhere but the single token definition, so a component cannot
  reintroduce a hardcoded palette.
* **Link and anchor integrity** — the build fails, rather than warns, on a broken
  internal link or a broken heading anchor.
