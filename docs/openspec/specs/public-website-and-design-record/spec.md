---
status: draft
date: 2026-07-25
implements: [ADR-0014]
---

# SPEC-0010: The Public Website and the Published Design Record

## Overview

This capability specifies the **public website** at `website/` — the marketing homepage
and the published documentation site — as distinct from the in-product web app shell of
SPEC-0001. It realizes **ADR-0014**: the design record in `docs/adrs/` and
`docs/openspec/specs/` is the single source of truth, the site renders it in full at
build time, and every index, badge, count, and cross-reference is derived rather than
hand-maintained.

The site is a Docusaurus 3 application that builds to a static bundle. It is **not**
part of the `cairnd` binary and shares nothing with SPEC-0001 except a design language.
Where SPEC-0001 renders artifacts for signed-in humans, this capability renders prose
and the decision record for anonymous readers.

Three surfaces are in scope:

- **The homepage** (`/`) — a bespoke route arguing for the product. Hand-written copy,
  derived counts.
- **The docs site** (`/docs/…`) — hand-written narrative pages plus the generated
  `/decisions` and `/specs` trees carrying the full text of every ADR and spec.
- **The design-language and reference pages** — the palette, the type rules, the badge
  set, and the REST/SSE surface summary.

The visual system is fixed by ADR-0014: five functional colours taken from the ADR-0009
span categories, `net` (`#38BDF8`) doubling as the brand accent, IBM Plex Sans for
prose and JetBrains Mono for machine content, on a `#0A0B0D` ground. There is no sixth
colour.

Requirements below are normative per RFC 2119. Anything the site *derives* is called
out explicitly, because the central failure mode this capability exists to prevent is a
fact written twice.

## Requirements

### Requirement: Design Token Foundation

The site MUST express its entire visual system as CSS custom properties defined in one
place, and MUST NOT hardcode a colour literal in any component stylesheet or inline
style.

The token set MUST include the five functional colours (`reason` `#A78BFA`, `exec`
`#FBBF24`, `read` `#34D399`, `net` `#38BDF8`, `write` `#F472B6`), the surface ramp
(`#0A0B0D` void, `#111317` surface, `#08090B` inset, `#0E1013` raised), the line
colours (`#1C1F26` dim, `#2A2E38`), and the text ramp (`#F2F4F8` heading, `#C4C9D4`
body, `#8A91A0` secondary, `#565E6E` tertiary).

The site MUST load IBM Plex Sans (400/500/600/700) and JetBrains Mono (400/500/600/700)
and MUST map Docusaurus's own theme variables onto these tokens so that unstyled theme
surfaces inherit the design language rather than the stock palette.

`net` MUST be used as the brand accent. A sixth functional colour MUST NOT be
introduced.

#### Scenario: A colour literal in a component

- **WHEN** any file under `website/src/` contains a hex colour literal outside the
  central token definition
- **THEN** the token lint check fails the build and names the file and literal

#### Scenario: Docusaurus surface inherits the palette

- **WHEN** a stock theme component that has not been swizzled renders (for example the
  search modal)
- **THEN** its background, border, and text colours resolve to the Cairn tokens, not
  the Docusaurus defaults

#### Scenario: Fonts applied by role

- **WHEN** prose and machine content render on the same page
- **THEN** prose is set in IBM Plex Sans and identifiers, paths, span names, TTLs, and
  commands are set in JetBrains Mono

---

### Requirement: Generated Decisions Tree

The site MUST render the full text of every architecture decision record from
`docs/adrs/ADR-*.md` under a `/decisions` route, generated at build time. ADR markdown
MUST NOT be copied into `website/`.

The generated index MUST contain exactly one row per matching source file, ordered by
ADR number, each row showing the number, the title taken from the `# ADR-XXXX:`
heading, a status badge derived from front-matter `status`, and the front-matter
`date`.

Each ADR detail page MUST render a metadata bar carrying the status badge, the date,
and traceability chips derived from the forward-only front-matter graph edges
(`extends`, `enables`, `related`). The site MUST additionally compute and render the
**inverse** edges — which ADRs and specs point *at* this one — without requiring
authors to write them.

#### Scenario: A new ADR publishes with no index edit

- **WHEN** `docs/adrs/ADR-0015-example.md` is added with valid front-matter and the site
  is rebuilt
- **THEN** it appears in the `/decisions` index in number order, its detail page
  renders in full, and no file under `website/` was edited

#### Scenario: Status drives the badge

- **WHEN** an ADR's front-matter `status` is `proposed`
- **THEN** its index row and detail page show a `PROPOSED` badge in the `exec` colour,
  and an `accepted` ADR shows `ACCEPTED` in the `read` colour

#### Scenario: Inverse edges are derived

- **WHEN** SPEC-0004 declares `implements: [ADR-0009]` and ADR-0009 declares no inverse
  edge
- **THEN** the ADR-0009 detail page renders a "referenced by SPEC-0004" chip linking to
  that spec

#### Scenario: An unrendered ADR fails the build

- **WHEN** a file matching `docs/adrs/ADR-*.md` exists but produces no page
- **THEN** the build fails naming the missing file

---

### Requirement: Generated Specs Tree

The site MUST render every capability specification from
`docs/openspec/specs/<capability>/spec.md`, together with its paired `design.md`, under
a `/specs` route, generated at build time.

The specs index MUST present one card per capability directory showing the spec number,
the title, a one-line summary, and a **requirement count derived by counting
`### Requirement:` sections** in the source. A requirement count MUST NOT be
hand-written anywhere.

A spec detail page MUST render each requirement as a discrete block carrying its
requirement name and normative statement, with RFC 2119 keywords (`MUST`, `MUST NOT`,
`SHOULD`, `MAY`) visually distinguished, and MUST render each `#### Scenario:` as its
associated WHEN/THEN block. The page MUST render an `implements` chip linking to each
ADR named in front-matter.

#### Scenario: Requirement count tracks the source

- **WHEN** a `### Requirement:` section is added to a spec and the site is rebuilt
- **THEN** the spec's card count increments with no other file edited

#### Scenario: RFC 2119 keywords are distinguished

- **WHEN** a requirement block containing `MUST` and `SHOULD` renders
- **THEN** each keyword is visually distinguished from surrounding prose and the
  distinction is not conveyed by colour alone

#### Scenario: Dangling ADR reference fails the build

- **WHEN** a spec declares `implements: [ADR-9999]` and no such ADR exists
- **THEN** the build fails naming the spec and the unresolved reference

---

### Requirement: Docs Chrome

The docs site MUST present a sticky navbar, a left sidebar carrying the full
information architecture, and a right-hand "on this page" table of contents on detail
pages, styled to the design language.

The sidebar MUST show Overview, Decisions (with a nested list of every ADR and a count),
Specs (with a nested list of every capability and a count), and Reference. The nested
ADR and spec lists MUST be generated, and their counts MUST be derived.

The current page MUST be indicated in the sidebar by a persistent visual marker that
does not rely on colour alone. Detail pages MUST provide a breadcrumb and prev/next
pagination.

Each doc page MUST offer a "view source" link pointing at the **public mirror**.

#### Scenario: Sidebar counts track the record

- **WHEN** a fourteenth ADR is added
- **THEN** the sidebar Decisions group shows a count of 14 and lists the new entry

#### Scenario: Current page marker

- **WHEN** a reader is on an ADR detail page
- **THEN** that ADR's sidebar entry carries a non-colour-only marker distinguishing it
  from its siblings

---

### Requirement: Homepage

The site MUST serve a bespoke homepage at `/` that is not a doc page. It MUST present,
in order: a hero with the product claim and a terminal demonstration of the
pipe-in/link-back loop; the three-step core loop; the share-type registry with a card
per type; the trajectory section including a span waterfall; surface parity across web,
CLI, and MCP; the product promises; and an entry point into the design record.

The homepage MAY hardcode its own prose. It MUST NOT hardcode any fact the design
record owns — specifically the ADR count, the capability-spec count, and the total
requirement count, all of which MUST be imported from the same generated data the docs
tree uses.

Product screenshots referenced by the homepage MUST be served from the site's own
static assets.

#### Scenario: Counts derive from the record

- **WHEN** a fifteenth ADR is added and the site is rebuilt
- **THEN** the homepage's design-record section reports 15 with no homepage edit

#### Scenario: Hardcoded count rejected

- **WHEN** a literal ADR or spec total is written into homepage source rather than
  imported
- **THEN** the build fails naming the file and the literal

---

### Requirement: Design Language and Reference Pages

The site MUST publish a design-language page rendering the surface ramp, the five
functional colours, the type-badge set, and the two type families with their usage
rule, each swatch labelled with its token name and value taken from the token
definition rather than restated.

The site MUST publish a reference page summarising the REST `/v1` surface and the SSE
stream endpoints, with HTTP verbs visually distinguished.

#### Scenario: Swatches read from tokens

- **WHEN** a token's value changes
- **THEN** the design-language page renders the new value with no page edit

---

### Requirement: Build-Time Referential Integrity

The build MUST fail, rather than degrade, on a broken design record.

The build MUST verify that: every ADR id named in any front-matter graph edge resolves
to a real ADR; every `implements` entry resolves to a real ADR; every `requires` entry
resolves to a real spec; every ADR has a `status` within the accepted enum and a
parseable `date`; and every spec has `status`, `date`, and `implements`.

Rendered site output MUST NOT contain any link to the private canonical forge host,
because such links are unreachable for anonymous readers. Forge links MUST target the
public mirror.

#### Scenario: Private forge link rejected

- **WHEN** rendered output contains a `gitea.stump.rocks` URL
- **THEN** the build fails naming the emitting file

#### Scenario: Malformed front-matter

- **WHEN** an ADR is added with no `status`
- **THEN** the build fails naming the file and the missing field

---

### Requirement: Markdown Compatibility

The generation pipeline MUST render the design record's CommonMark faithfully without
requiring authors to write MDX-safe prose. Constructs that are legal markdown but
hostile to MDX — bare `<`, bare `{`, unclosed pseudo-tags — MUST be escaped by the
pipeline.

Fenced code blocks MUST retain their language for syntax highlighting, and the
highlighter MUST cover at minimum `go`, `bash`, `json`, `sql`, and `yaml`.

#### Scenario: MDX-hostile prose renders

- **WHEN** an ADR contains a bare `<` or `{` in prose
- **THEN** the page renders the character literally and the build does not fail

---

### Requirement: Accessibility

The site MUST meet WCAG 2.1 AA. Body text and UI text MUST meet the applicable
contrast ratio against their background. Status, category, and type information MUST
NOT be conveyed by colour alone. All interactive controls MUST be reachable and
operable by keyboard with a visible focus indicator. The site MUST honour
`prefers-reduced-motion`. Images MUST carry descriptive alternative text, and decorative
imagery MUST be hidden from assistive technology.

#### Scenario: Category conveyed without colour

- **WHEN** a span category or ADR status renders
- **THEN** its meaning is available from text or shape, not from hue alone

#### Scenario: Reduced motion honoured

- **WHEN** a visitor has `prefers-reduced-motion: reduce` set
- **THEN** animations and transitions are suppressed

---

### Requirement: Build and Deployment

The site MUST build to a static bundle with no network access at build time beyond
package installation, and the build MUST fail on a broken internal link.

CI MUST build the site on every change touching `website/`, `docs/adrs/`, or
`docs/openspec/specs/`, so that a change to the design record that breaks the site is
caught on the pull request that makes it.

Deployment MUST publish from the canonical forge's CI. The public base URL MUST be
configured in exactly one place.

#### Scenario: Record change breaks the site

- **WHEN** a pull request edits an ADR in a way that breaks the site build
- **THEN** CI fails on that pull request rather than after merge

#### Scenario: Broken internal link

- **WHEN** a doc page links to a route that does not exist
- **THEN** the build fails naming the source page and the target
