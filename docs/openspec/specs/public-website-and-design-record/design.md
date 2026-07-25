# Design: The Public Website and the Published Design Record

Companion to `spec.md` (SPEC-0010). This document carries the rationale and the shape
of the implementation; the normative requirements live in the spec.

## The central constraint

ADR-0014 reduces to one sentence: **a fact about the design record lives in exactly one
file.** Everything in this design follows from deciding where each fact lives and how
it reaches the page.

There are three kinds of content on this site and it is worth being precise about which
is which, because the failure mode is treating one as another:

| Kind | Source of truth | Written by | Example |
| --- | --- | --- | --- |
| **Record** | `docs/adrs/`, `docs/openspec/specs/` | ADR/spec authors | ADR-0009's Decision section |
| **Derived** | computed from the record at build time | nobody | "13 ADRs", `ACCEPTED` badge, "referenced by SPEC-0004" |
| **Narrative** | `website/docs/*.md`, homepage source | site authors | "Cairn is a pastebin reimagined for the agent era" |

The bug class this design is built to eliminate is *derived content that got written by
a human* — the hardcoded requirement counts in today's `specifications.md`, the ADR
table in `architecture.md`. Those are deleted, not updated.

## Content pipeline

Docusaurus wants its content inside the site directory. The record lives two levels up
and must stay there. Three ways to bridge that; we take the third.

1. **Symlink `website/docs/decisions` → `../../docs/adrs`.** Simplest, and it works
   locally. It breaks on Windows checkouts without developer mode, it breaks in some CI
   checkout configurations, and it gives no place to intervene for MDX escaping or
   front-matter transformation. Rejected.
2. **Point a second `plugin-content-docs` instance directly at `../docs/adrs`.**
   Docusaurus accepts a path outside the site root. This works and avoids a copy — but
   the ADR front-matter is not Docusaurus front-matter (`status`, `extends`, `enables`
   are not theme fields), the filenames are not the slugs we want, and there is nowhere
   to escape MDX-hostile prose. It would force the record's authoring conventions to
   bend toward Docusaurus. Rejected on those grounds: the record's format is the thing
   we are protecting.
3. **A prebuild transform into a generated, git-ignored staging tree.** Chosen.

The prebuild step reads the record, transforms it, and writes
`website/.generated/{decisions,specs}/`. Two `plugin-content-docs` instances mount that
tree. The staging directory is git-ignored, so there is no copy under review and no
opportunity for the copy to be edited by hand — which is what keeps this from being
Option C of ADR-0014 wearing a disguise.

```
docs/adrs/ADR-*.md ─┐
                    ├─▸ scripts/generate-record.ts ─▸ website/.generated/ ─▸ docusaurus build
docs/openspec/…   ─┘         │
                             ├─ parse front-matter + headings
                             ├─ build the reference graph (both directions)
                             ├─ escape MDX-hostile constructs
                             ├─ emit per-page MDX + a record.json data module
                             └─ validate → non-zero exit on any integrity failure
```

The step emits one extra artifact: **`record.json`**, a single data module carrying the
ADR list, the spec list, counts, and the resolved graph. The homepage imports it for its
counts, the generated indexes render from it, and the sidebar is built from it. That is
how "13 ADRs" reaches the marketing page without anyone typing `13`.

### Why validation lives in the generator

The integrity checks (dangling `implements`, malformed front-matter, private-forge
links) could live in a separate lint script. Putting them in the generator means they
run on every `npm run build` and every `npm start`, so an author who breaks a
cross-reference finds out in the dev server rather than in CI. The generator exits
non-zero; Docusaurus never starts. Failing loudly here is the whole point of ADR-0014's
Confirmation section.

### Inverse edges

The record's authoring convention is forward-only — `ADR-0009` declares `extends:
[ADR-0002]` and nothing declares "ADR-0002 is extended by ADR-0009". Readers want both
directions. The generator walks every `extends` / `enables` / `related` / `implements` /
`requires` edge, inverts them into a reverse-adjacency map, and attaches the result to
each node before emitting. Authors keep writing one direction; the page shows two. This
is the single highest-value thing the generator does that a symlink could not.

## Theming

Docusaurus theming is a ladder and we climb only as far as necessary:

1. **CSS custom properties in `src/css/custom.css`** — the whole token set, plus
   overrides mapping `--ifm-*` onto Cairn tokens. This alone gets stock components
   (search modal, admonitions, pagination) onto the palette. Most of the design lands
   here.
2. **Component-scoped CSS modules** — for bespoke pieces that have no theme
   counterpart: the ADR index rows, spec cards, requirement blocks, the waterfall.
3. **Swizzling** — last resort, and each one is a maintenance liability pinned to a
   Docusaurus major.

The swizzle budget is deliberately small. Anticipated: navbar (the wordmark, the
three-bar mark, the search affordance), `DocSidebarItem` (nested ADR/spec lists with
counts and the current-page marker), `TOC` (the "ON THIS PAGE" treatment), and
`DocItem/Footer` (the "view source" link at the public mirror). Anything else should be
reachable with CSS. Every swizzled file carries a comment naming the mockup element it
exists to serve, so a future upgrade can judge whether it is still needed.

## Component inventory

Bespoke components the record pages need, all rendering from `record.json` or from MDX
frontmatter — none of them holding content:

- **`StatusBadge`** — `ACCEPTED` / `PROPOSED` / `DRAFT`. Colour plus text, never colour
  alone.
- **`RecordIndexRow`** — number, title, badge, date. Used by the decisions index.
- **`SpecCard`** — id, derived requirement count, title, summary.
- **`TraceChips`** — the graph edges, forward and inverse, each linking to its target.
- **`RequirementBlock`** — requirement id, normative statement with RFC 2119 keywords
  distinguished, and its scenarios.
- **`TypeBadge`** — `MD` / `PY` / `IMG` / `FILE` / `HK` / `TRJ` / `BUNDLE`, tinted by the
  category the type belongs to. Shared with the homepage.
- **`Callout`** — `NOTE` / `CAUTION` / `RULE OF THUMB`, left-bordered.
- **`Waterfall`** — the span timeline. Homepage only; static data.

## Homepage

The homepage is a plain React route under `src/pages/`. It is narrative, not derived,
with one exception: it imports `record.json` for counts. Its sections map to the
mockup — hero, core loop, share types, trajectory, surface parity, promises, design
record, CTA.

The surface-parity section is a client-side tab set (Web / CLI / MCP). It is the only
stateful thing on the page and it must work without JavaScript as a stacked layout,
because a marketing page that renders blank without JS is a bad advertisement for a
project that server-renders its actual product.

Screenshots ship as static assets under `static/img/shots/`. They are product
screenshots, not decoration, and carry descriptive alt text.

## Accessibility posture

The design is dark, low-contrast-adjacent, and colour-coded by category — three
standing risks. Concretely:

- The tertiary text colour (`#565E6E`) fails AA against the void ground at body size.
  It is therefore restricted to **large or bold monospace labels** where it passes, and
  never used for prose. Where the mockup uses it for running text, the body colour is
  substituted.
- Category and status are always accompanied by their name in text. The five-colour
  palette is chosen to survive the common colour-vision deficiencies, but that is a
  nicety, not the mechanism.
- The sidebar current-page marker is an inset rule plus `aria-current`, not a colour
  change.
- `prefers-reduced-motion` is honoured globally in the token stylesheet.

## What this capability does not cover

- The in-product app shell — that is SPEC-0001 and ADR-0011. Shared design language,
  separate implementation, separate deploy.
- Docs versioning. The record has no versions today; adding them is a later decision.
- Search beyond Docusaurus's local search.
- Any change to how ADRs and specs are authored. This capability reads the record; it
  does not get to reshape it.
