---
status: draft
date: 2026-07-25
implements: [ADR-0014]
requires: [SPEC-0004]
---

# SPEC-0010: The Public Website and the Published Design Record

## Overview

Cairn's public website is a Docusaurus 3 application under `website/` that builds to a
static bundle. It is **not** part of the `cairnd` binary and shares nothing with the
in-product app shell of SPEC-0001 except a design language: where SPEC-0001 renders
artifacts for signed-in humans, this capability renders prose and the decision record
for anonymous readers.

This capability realises **ADR-0014**. The design record in `docs/adrs/` and
`docs/openspec/specs/` is the single source of truth; the site renders it in full at
build time; and every index row, badge, date, count, and cross-reference on the site is
**derived** from those files rather than written by hand. The central failure mode this
capability exists to prevent is a fact written twice.

Four surfaces are in scope:

- **The homepage** (`/`) — a bespoke React route arguing for the product. Hand-written
  prose, derived counts.
- **The narrative docs** — the hand-written "explain it to a newcomer" pages that already
  exist under `website/docs/`.
- **The generated record** — the full text of every ADR and every capability
  specification, with generated indexes, metadata bars, and a computed cross-reference
  graph.
- **The derived reference pages** — the token set and type rules, and an API surface
  taken from the specifications' own endpoint tables.

The visual system is the one already shipped in `website/src/css/custom.css`, which is
canonical: the ADR-0009 span-category accent tokens plus a neutral default, IBM Plex
Sans for prose, JetBrains Mono for machine content, and a dark-first surface ramp. This
capability restyles surfaces, typography, and chrome. It does **not** redefine the
category palette, which is owned by ADR-0009 and made normative by SPEC-0004.

## Requirements

### Requirement: Design Token Source of Truth

The site's entire visual system MUST be expressed as CSS custom properties defined in
exactly one file, `website/src/css/custom.css`. No other file under `website/src/` MAY
contain a colour literal; every component stylesheet and inline style MUST reference a
token. Docusaurus's own theme variables MUST be mapped onto these tokens so that theme
surfaces carrying no Cairn override inherit the Cairn palette rather than the stock one.

#### Scenario: Colour literal in a component

- **WHEN** a file under `website/src/` other than the token definition contains a hex
  colour literal
- **THEN** the lint MUST fail the build and MUST name the file, the line, and the literal

#### Scenario: Stock theme surface

- **WHEN** a theme component the site does not style renders
- **THEN** its colours MUST resolve through the token definition rather than through
  Docusaurus's stock defaults

### Requirement: Category Palette Immutability

The token set MUST retain the ADR-0009 span-category accents and the neutral-default
token exactly as shipped, at their shipped values. This capability MUST NOT add, remove,
rename, recolour, or cap the category tokens; changing them is a decision against
ADR-0009, not a website change.

Any category-derived class name emitted by site source MUST resolve to an ADR-0009
category token or to the neutral default. A class naming a category ADR-0009 does not
define MUST fall back to the neutral-default token rather than carry its own colour.

#### Scenario: Unrecognised category class

- **WHEN** site source emits a category class for a string ADR-0009 does not define
- **THEN** the element MUST render in the neutral-default token and MUST NOT introduce a
  colour of its own

### Requirement: Typographic Roles

Prose MUST be set in IBM Plex Sans and machine-emitted content MUST be set in JetBrains
Mono. Machine-emitted content means identifiers, short links, file paths, span names,
categories, TTLs, HTTP methods, commands, and code. Running sentences MUST NOT be set in
the monospace family.

Both families MUST be declared once, in the token definition, with system fallbacks, so
that a page remains legible if a font resource fails to load.

#### Scenario: Identifier inside a sentence

- **WHEN** a paragraph of prose contains an artifact id or a file path
- **THEN** the surrounding sentence MUST render in IBM Plex Sans and the identifier MUST
  render in JetBrains Mono

### Requirement: Site Chrome and Layout

Documentation pages MUST present a sticky top navigation bar, a persistent left
navigation rail carrying the full information architecture, and, on record detail pages,
a right-hand "on this page" table of contents. Detail pages MUST provide a breadcrumb
naming the section and the current document, and prev/next pagination in document order.

The chrome MUST be achieved through CSS custom properties and CSS modules wherever
possible. No theme component MAY be **ejected**. At most **one** theme component MAY be
**wrapped**, and that wrap MUST be justified in the design document. Where a visual detail
can be reached only by ejecting a component, or only by spending a second wrap, that
detail MUST be dropped rather than the budget raised.

#### Scenario: Detail page chrome

- **WHEN** an ADR or specification detail page renders at desktop width
- **THEN** it MUST show a breadcrumb, the left navigation rail, an on-this-page column,
  and prev/next pagination

### Requirement: Record Content Pipeline

The record MUST be transformed and staged **before Docusaurus initialises its content
plugins**, so that a build from a clean checkout produces the full record on its first
attempt and never publishes the previous build's output.

The staged output MUST be **git-ignored** and MUST live inside the documentation content
root, so that one content instance serves both it and the tracked narrative pages. Record
markdown MUST NOT be committed to any tracked path under `website/`.

The pipeline MUST additionally re-run while the development server is running, watching the
record source directories, so that editing an ADR or a specification re-renders the affected
pages without restarting the server.

#### Scenario: Clean-checkout build

- **WHEN** the site is built from a fresh clone in which no staged output exists
- **THEN** the build MUST succeed and MUST publish the full record from that same build

#### Scenario: Record edited during development

- **WHEN** an ADR is edited while the development server is running
- **THEN** the corresponding page MUST re-render without a server restart

#### Scenario: Staged output is not tracked

- **WHEN** the pipeline has run and the working tree is inspected
- **THEN** the staged directories MUST be ignored by git and MUST NOT appear as untracked
  or modified paths

### Requirement: Derived Data Module

The pipeline MUST emit a single derived data module carrying the decision list, the
specification list, the computed counts, and the resolved cross-reference graph. Every
generated index, badge, count, and chip rendered inside a page MUST read from that module.

The sidebar is the one consumer that MUST NOT read it: Docusaurus loads the sidebar
configuration as a value during the documentation plugin's own content loading, before the
module exists. The sidebar MUST instead be generated from the staged tree itself, with any
group labels or counts written into the staged directories by the pipeline.

#### Scenario: A count is needed outside the docs tree

- **WHEN** the homepage renders a decision or specification total
- **THEN** it MUST read that total from the derived data module and MUST NOT restate it

### Requirement: Generated Decisions Tree

The site MUST render the full text of every file matching `docs/adrs/ADR-*.md` as its own
page, generated at build time.

The generated decisions index MUST contain exactly one row per matching source file,
ordered by ADR number, each row carrying the number, the title taken from the
`# ADR-XXXX:` heading, a status badge, and the front-matter date. A source file that
matches the pattern but produces no page MUST fail the build.

#### Scenario: A new decision publishes with no index edit

- **WHEN** a new `docs/adrs/ADR-*.md` with valid front-matter is added and the site is
  rebuilt
- **THEN** it MUST appear in the decisions index in number order and its full text MUST
  render, and no tracked file under `website/` MAY have been edited

#### Scenario: Unrendered decision file

- **WHEN** a file matching `docs/adrs/ADR-*.md` exists and no page is generated for it
- **THEN** the build MUST fail and MUST name the source file

### Requirement: Generated Specs Tree

The site MUST render every capability specification under
`docs/openspec/specs/<capability>/spec.md`, together with its paired `design.md`, as
generated pages at build time. The paired design document MUST be reachable from its
specification.

Specification numbering MUST be read from each specification's own `# SPEC-XXXX:`
heading and MUST NOT be inferred from directory order, because the directory names do
not sort into specification order.

The specs index MUST present one card per capability directory, ordered by specification
number, each carrying the specification number, its title, a one-line summary, and its
derived requirement count. A directory that produces no card MUST fail the build.

#### Scenario: Specification numbering

- **WHEN** the specs index renders
- **THEN** each card's number MUST match the `# SPEC-XXXX:` heading of its source
  specification, and the index MUST be ordered by that number

### Requirement: Derived Status, Dates, and Counts

Status badges MUST be derived from front-matter `status`. Dates shown on index rows and
metadata bars MUST be derived from front-matter `date`. Requirement counts MUST be
derived by counting `### Requirement:` sections in the source, and scenario counts by
counting `#### Scenario:` sections.

No count of decisions, specifications, requirements, or scenarios MAY be written as a
literal anywhere in site source. Every such number MUST be read from the derived data
module. A status badge MUST carry its status as text; the badge's colour MUST NOT be the
only carrier of its meaning.

#### Scenario: Requirement count tracks the source

- **WHEN** a `### Requirement:` section is added to a specification and the site is
  rebuilt
- **THEN** that specification's card count MUST increment and no other file MAY have been
  edited

### Requirement: Derived Cross-Reference Graph

Each record detail page MUST render a metadata bar carrying its status badge, its date,
and traceability chips for its front-matter graph edges (`extends`, `enables`, `related`,
`implements`, `requires`). Each chip MUST link to the record it names.

The site MUST additionally compute and render the **inverse** edges — which decisions and
specifications point *at* this one — without authors writing them, preserving the
record's forward-only authoring convention.

The graph computation MUST de-duplicate. The record already contains relationships
authored from both ends: a decision may declare `enables: [X]` while `X` independently
declares `extends:` back to it. Edges MUST be normalised to an unordered pair plus a
relationship kind before rendering, so that a single relationship appears exactly once.

#### Scenario: Inverse edge is derived

- **WHEN** a specification declares `implements: [ADR-XXXX]` and that ADR declares no
  inverse edge
- **THEN** the ADR's detail page MUST render a chip naming that specification and linking
  to it

#### Scenario: Relationship authored from both ends

- **WHEN** decision A declares `enables: [B]` and decision B declares `extends: [A]`
- **THEN** A's page MUST show that relationship to B exactly once

### Requirement: Record Prose to Structured Components

Requirement blocks, scenarios, and RFC 2119 keywords in the record MUST be rendered as
structured components rather than as plain prose. A requirement MUST render as a discrete
block carrying its name and its normative statement; each `#### Scenario:` MUST render as
its associated WHEN/THEN block; and RFC 2119 keywords MUST be visually distinguished from
surrounding prose.

That structure MUST be recognised and rewritten on the parsed document tree, before the
document is handed to the MDX compiler. Component injection MUST NOT be performed by
writing component markup into the record text, because a document containing literal
component markup can no longer safely contain un-escaped `<` or `{`.

Record source files MUST remain plain CommonMark. Authors MUST NOT be required to write
MDX-safe prose or to annotate their requirements for the renderer's benefit.

#### Scenario: Requirement renders as a block

- **WHEN** a specification page renders a `### Requirement:` section
- **THEN** it MUST render as a discrete block carrying the requirement name and its
  normative statement

#### Scenario: Keyword adjacent to literal markup

- **WHEN** a requirement's normative statement contains both an RFC 2119 keyword and a
  bare `<` in the same paragraph
- **THEN** the keyword MUST render as distinguished, the `<` MUST render as a literal
  character, and the build MUST NOT fail

### Requirement: Record Navigation

The record MUST be served from a **single** documentation content instance so that one
sidebar spans the narrative pages, the decisions tree, and the specs tree. Docusaurus
sidebars are scoped strictly to one content instance, so splitting the record across
additional instances would leave a reader inside one tree unable to see the other.

Record routes MUST therefore live beneath the documentation route base, as
`…/decisions/<id>` and `…/specs/<capability>`, so that the single-instance constraint is
satisfied without a theme override.

The sidebar MUST present the narrative pages, a Decisions group listing every decision, a
Specs group listing every capability, and the reference pages. The nested lists and any
counts shown beside the group labels MUST be generated by the pipeline rather than
hand-authored; no sidebar configuration file MAY enumerate record documents. The current
page MUST carry `aria-current`.

#### Scenario: One sidebar spans the record

- **WHEN** a reader is on a specification detail page
- **THEN** the sidebar MUST still list the decisions tree and the narrative pages

#### Scenario: Sidebar tracks the record

- **WHEN** a new decision is added and the site is rebuilt
- **THEN** the sidebar's Decisions group MUST list it and any count beside that group
  MUST reflect it, with no sidebar file edited

### Requirement: No Repository Links

The site MUST NOT emit a link to a source repository. There MUST be no "view source" or
"edit this page" affordance on any page, and the documentation content instance MUST NOT
configure an edit URL.

Two build-time checks enforce this. The build MUST fail if the documentation content
instance is configured with an edit URL. Separately, **rendered output** MUST be checked
against an allowlist of permitted external hosts, and any link to a repository host outside
that allowlist MUST fail the build, regardless of which forge it names. Because rendered
output exists only after a full build, the second check runs in CI and not in the
development server.

#### Scenario: Link to a non-allowlisted repository host

- **WHEN** rendered output contains a link to a repository host that is not on the
  allowlist
- **THEN** the build MUST fail and MUST name the emitting page and the URL

#### Scenario: Edit affordance configured

- **WHEN** the documentation content instance is configured with an edit URL
- **THEN** the build MUST fail and MUST name the configuration key

### Requirement: Homepage

The site MUST serve a bespoke homepage at `/` that is not a documentation page and is not
generated from the record. It MUST present, in order: a hero carrying the product claim
and a terminal demonstration of the pipe-in/link-back loop; the core loop; the share-type
registry with a card per type; the trajectory section including a span waterfall; surface
parity across web, CLI, and MCP; the product promises; and an entry point into the design
record.

The homepage MAY hardcode its own prose. It MUST NOT hardcode any fact the record owns;
decision, specification, requirement, and scenario counts MUST be read from the derived
data module. Any interactive homepage section MUST render usable content without
JavaScript: a tabbed section MUST fall back to a stacked layout presenting every panel,
rather than rendering an empty or partial region.

#### Scenario: Counts derive from the record

- **WHEN** a decision is added to the record and the site is rebuilt
- **THEN** the homepage's design-record section MUST report the new total and no homepage
  file MAY have been edited

#### Scenario: Homepage without JavaScript

- **WHEN** the homepage is rendered with JavaScript disabled
- **THEN** every section MUST present its content, and any tabbed section MUST fall back
  to a stacked layout

### Requirement: Derived Design-Language Page

The site MUST publish a design-language page rendering the surface ramp, the span
category accents with their category names, the type-badge set, and the two type
families with their usage rule. Each swatch MUST be labelled with its token name and MUST
take its value from the token definition rather than restating it. The page MUST render
every category token ADR-0009 defines plus the neutral default, and MUST state that a
category outside the recommended set renders in the neutral default.

#### Scenario: Swatches read from tokens

- **WHEN** a token's value changes and the site is rebuilt
- **THEN** the design-language page MUST render the new value and no page file MAY have
  been edited

#### Scenario: Neutral default documented

- **WHEN** the design-language page renders the category section
- **THEN** it MUST show the neutral-default token and MUST state that unrecognised
  categories render with it

### Requirement: Derived HTTP Reference Page

The site MUST publish a reference page summarising the service's HTTP surface and its
streaming endpoints, **derived from the endpoint tables the specifications already carry**
and never restated by hand. The pipeline MUST scan an explicit, enumerated list of section
names — today `## HTTP Endpoints`, `## REST Endpoints`, `## Endpoint Table`, and
`## Web Routes` — excluding this specification's own `## Web Routes` table, which describes
the static website rather than the service. A specification carrying none of those sections
contributes no rows, and the page MUST NOT claim to be a complete API surface.

Each row MUST link to the specification that owns it and MUST carry the method and the
auth posture where its source states them. If the derivation cannot be implemented, the
page MUST be dropped rather than hand-maintained.

#### Scenario: Endpoint added to a specification

- **WHEN** a row is added to a specification's endpoint table and the site is rebuilt
- **THEN** the reference page MUST show that endpoint, MUST link the row to that
  specification, and no reference page file MAY have been edited

#### Scenario: Specification with no endpoint section

- **WHEN** a specification carries none of the enumerated section names
- **THEN** the build MUST NOT fail and that specification MUST contribute no rows

### Requirement: Referential Integrity and Front-Matter Validation

The build MUST fail, rather than degrade, on a broken design record. The build MUST verify
that every ADR has a `status` within the accepted enum and a parseable `date`; that every
specification has `status`, `date`, and `implements`; that every ADR id in a front-matter
graph edge resolves to a real ADR; that every `implements` entry resolves to a real ADR;
and that every `requires` entry resolves to a real specification.

Validation MUST run inside the content pipeline rather than as a separate optional step,
so that a broken record fails the development server as well as the build.

#### Scenario: Missing front-matter field

- **WHEN** a record file is added with no `status`
- **THEN** the build MUST fail and MUST name the file and the missing field

#### Scenario: Dangling cross-reference

- **WHEN** a front-matter graph edge, `implements`, or `requires` names an identifier
  with no corresponding source file
- **THEN** the build MUST fail and MUST name the referring file and the unresolved
  identifier

### Requirement: CommonMark Fidelity

The pipeline MUST render the record's CommonMark faithfully. Constructs that are legal
markdown but hostile to MDX — a bare `<`, a bare `{` — MUST be handled by the pipeline and
MUST NOT require authors to change how they write.

A word shaped like an HTML tag, such as `<id>`, is not merely hostile to MDX: CommonMark
reads it as raw HTML, so it reaches the page as a real element rather than as text and is
swallowed by any excerpt derived from the source. The pipeline MUST render such constructs
as literal text, and MUST derive each page's meta description from the transformed
document rather than from the raw source, so that a description is never silently truncated
at a tag-shaped word.

Tables, nested lists, and front-matter comments in the record MUST render or be suppressed
cleanly rather than producing a parse error.

#### Scenario: MDX-hostile prose

- **WHEN** a record file contains a bare `<` or `{` in prose
- **THEN** the page MUST render the character literally and the build MUST NOT fail

#### Scenario: Tag-shaped word in prose

- **WHEN** a record file's prose contains a tag-shaped token such as `<id>`
- **THEN** the page MUST render it as literal text, MUST NOT emit it as an element, and
  the page's meta description MUST NOT be truncated at it

### Requirement: Code Block Fidelity

Fenced code blocks MUST retain their declared language, and the highlighter MUST cover at
minimum `go`, `bash`, `json`, `sql`, and `yaml`. A block declaring a language the
highlighter does not cover MUST render as plain preformatted text rather than failing the
build.

#### Scenario: Unsupported code fence language

- **WHEN** a record file declares a fence language the highlighter does not cover
- **THEN** the block MUST render as plain preformatted text and the build MUST NOT fail

### Requirement: Link and Anchor Integrity

The site MUST be configured to fail on broken internal links and on broken heading
anchors, rather than to warn. Both checks execute during a full build and not during the
development server, so CI MUST run a full build for them to have any effect.

Cross-references between record documents MUST resolve to on-site routes.

#### Scenario: Broken internal link

- **WHEN** a page links to a route that does not exist
- **THEN** the build MUST fail and MUST name the source page and the target

#### Scenario: Broken heading anchor

- **WHEN** a page links to a heading anchor that no target page defines
- **THEN** the build MUST fail and MUST name the source page and the anchor

### Requirement: Build and Deployment

The site MUST build to a static bundle, and the build MUST NOT require network access
beyond dependency installation.

CI MUST build the site on every pull request and on every push that touches `website/`,
`docs/adrs/`, or `docs/openspec/specs/`. The deployment trigger MUST include the record
directories: a change that adds or edits a decision or a specification changes the
published site and MUST cause a deploy. The public base URL MUST be configured in exactly
one place.

#### Scenario: Record-only change deploys

- **WHEN** a commit touching only `docs/adrs/` lands on the default branch
- **THEN** the site MUST rebuild and redeploy

#### Scenario: Record change breaks the site

- **WHEN** a pull request edits a record file in a way that breaks the site build
- **THEN** CI MUST fail on that pull request rather than after merge

## Security Requirements

This capability publishes a static, anonymously readable bundle with no server of its own,
so several of the requirements every other Cairn specification carries have nothing to
attach to. They are named here rather than silently omitted:

- **Authentication & Authorization** — not applicable. The site has no authenticated
  surface and no per-reader state; every route is public by construction (see Web Routes).
- **Rate Limiting** — not applicable at the application layer. The site executes no
  request handling; whatever the static host applies is a hosting concern.
- **Request Body Size Limits** — not applicable. The site accepts no request body.
- **CSRF Protection** — not applicable. The site has no state-changing request, no form
  post, and no ambient credential.
- **Redirect & SSRF Validation** — not applicable. The site performs no server-side fetch
  and honours no user-supplied redirect target.

The remaining requirements are MANDATORY.

### Requirement: First-Party Asset Loading

Every asset the site loads at runtime — fonts, stylesheets, scripts, and images — MUST be
served from the site's own origin. The site MUST NOT load a font, stylesheet, or script
from a third-party host at page load: doing so discloses every reader's IP address and
user agent to that host and makes the site's availability depend on it.

Web fonts MUST be self-hosted, subset, and served with the fallback stack declared in the
token definition.

#### Scenario: Third-party asset request

- **WHEN** a page is loaded and its network requests are inspected
- **THEN** every request MUST target the site's own origin

#### Scenario: Remote font import in source

- **WHEN** site source contains a stylesheet import or link naming a third-party font
  host
- **THEN** the build MUST fail and MUST name the file and the host

### Requirement: Security Headers

Every response MUST carry a strict Content-Security-Policy restricting `default-src`,
`script-src`, `style-src`, `font-src`, and `img-src` to the site's own origin, and setting
`frame-ancestors` to deny framing. Responses MUST also carry `X-Content-Type-Options:
nosniff`, a `Referrer-Policy`, and (over HTTPS) HSTS.

These MUST be delivered as **response headers**. A `<meta http-equiv>` policy is not an
equivalent: `frame-ancestors` is specified to be ignored when delivered that way, and
`nosniff`, `Referrer-Policy`, and HSTS have no honoured `<meta>` form at all. The site
therefore MUST be published on a host, or behind a proxy, that can set response headers.

The policy MUST additionally be verified against the built bundle, so that a dependency
introducing a remote asset is caught at build time rather than by a reader's browser. That
verification is host-independent and MUST run regardless of how the headers are delivered.

#### Scenario: Headers absent from a response

- **WHEN** a published page is requested and its response headers are inspected
- **THEN** the Content-Security-Policy, `X-Content-Type-Options`, `Referrer-Policy`, and
  HSTS headers MUST be present

#### Scenario: Policy violated by the bundle

- **WHEN** the built bundle contains an asset reference the policy would block
- **THEN** the build MUST fail and MUST name the asset

### Requirement: Deployment Least Privilege

The deployment workflow MUST run with the minimum permissions required to publish a
static bundle and MUST NOT be granted write access to repository contents.

No credential, token, or private host name MAY appear in the built bundle or in the
derived data module. Front-matter fields that are not needed for rendering MUST NOT be
emitted into the published output. The bundle MUST be scanned for these before the deploy
step runs, and a match MUST fail the deployment.

#### Scenario: Secret in the bundle

- **WHEN** the built bundle is scanned and contains a credential, token, or private host
  name
- **THEN** the deployment MUST fail and MUST name the file

## Accessibility Requirements

This capability renders user-facing UI. WCAG 2.1 AA is the minimum target.

### Requirement: WCAG 2.1 AA & Semantics

All site UI MUST meet WCAG 2.1 AA. Every page MUST use a single `h1` and a heading order
with no skipped levels, and MUST expose landmark regions for the navigation rail, the main
content, and the table of contents.

Animation and transition MUST be suppressed when `prefers-reduced-motion: reduce` is set,
including hover transforms on cards and tiles. Informative images MUST carry descriptive
alternative text; decorative imagery and decorative glyphs MUST be hidden from assistive
technology.

#### Scenario: Generated page heading order

- **WHEN** a generated record page renders
- **THEN** it MUST contain exactly one `h1` and MUST NOT skip a heading level, whatever
  heading levels the source file used

#### Scenario: Reduced motion honoured

- **WHEN** a visitor has `prefers-reduced-motion: reduce` set
- **THEN** transitions and transforms MUST be suppressed, including hover elevation on
  cards and tiles

### Requirement: Icon-Only Controls

Every icon-only control MUST expose an `aria-label` describing its action. This covers the
controls the theme ships as well as any the site adds: the colour-mode toggle, the sidebar
collapse/expand control, the mobile menu button, and the code-block copy button.

#### Scenario: Icon button labelling

- **WHEN** a control renders with no visible text label
- **THEN** it MUST expose an `aria-label` describing what it does

### Requirement: Dynamic Content Regions

Controls that show or hide content — the tabbed surface-parity section, collapsible
sidebar groups, the mobile menu — MUST expose their state to assistive technology via
`aria-expanded` or `aria-selected`, and MUST move focus predictably rather than leaving it
on a removed node. Any region whose content is replaced client-side MUST announce the
change without stealing focus.

#### Scenario: Disclosure state exposed

- **WHEN** a collapsible sidebar group or a tab is toggled
- **THEN** its control MUST reflect the new state in `aria-expanded` or `aria-selected`,
  and focus MUST remain on a node that still exists

### Requirement: Keyboard Navigation & Focus Management

Every interactive control MUST be reachable and operable by keyboard in a logical order,
and MUST show a visible focus indicator meeting the 3:1 non-text contrast threshold. No
control MAY be operable by pointer only.

The site MUST provide a skip link to the main content, because the left navigation rail
lists every record document and would otherwise sit ahead of the content on every page.

#### Scenario: Focus indicator visible

- **WHEN** a control receives keyboard focus
- **THEN** a focus indicator MUST be visible and MUST meet at least 3:1 against adjacent
  colours

#### Scenario: Skipping the navigation rail

- **WHEN** a keyboard user tabs into a documentation page
- **THEN** a skip link MUST be reachable before the navigation rail and MUST move focus
  to the main content

### Requirement: Contrast

Text MUST meet a contrast ratio of at least **4.5:1** against its background. Large text —
18pt (24px) or larger at regular weight, or 14pt (18.66px) or larger at bold weight — MUST
meet at least **3:1**. Non-text elements that convey information or identify an active
control — badge borders, focus indicators, sidebar markers, waterfall bars — MUST meet at
least **3:1** against adjacent colours per WCAG 1.4.11.

Ratios MUST be measured against the surface the element actually renders on rather than
against the page ground, because tinted badge and chip backgrounds change the effective
background.

The docs mockup's tertiary label colour `#565E6E` MUST NOT be used for text at any size;
every element the mockup sets in it MUST use the shipped muted token instead. It MAY be
retained only for a rule or divider that conveys no information. The measurements behind
this are in the design document.

#### Scenario: Muted label on an elevated surface

- **WHEN** a monospace label renders on the elevated surface
- **THEN** it MUST meet at least 4.5:1 against that surface, or at least 3:1 if it meets
  the large-text size and weight thresholds

### Requirement: Category Accents Are Dark-Ground Text Colours

The ADR-0009 category accent tokens are constant across colour modes and MUST NOT be
recoloured (see Category Palette Immutability). Measured as foreground they clear 4.5:1 on
the dark surface ramp and clear neither 4.5:1 nor, for most of the set, 3:1 on a white
ground.

A category accent MUST therefore be used as a text colour only against the dark surface
ramp. Any chip, badge, or bar that renders an accent as text MUST retain that dark ramp in
every colour mode; where it cannot, the accent MUST be demoted to a non-text indicator and
the category name MUST carry the meaning in a text token.

#### Scenario: Category accent as foreground

- **WHEN** a category accent token is used as a text colour on a chip
- **THEN** that chip MUST render on the dark surface ramp and the accent MUST meet at
  least 4.5:1 against the chip's composited background

#### Scenario: Accent on a light surface

- **WHEN** a surface renders in light colour mode and would otherwise use a category
  accent as its text colour
- **THEN** the text colour MUST come from a text token and the accent MUST appear only as
  a non-text indicator meeting at least 3:1

### Requirement: Non-Colour Encoding

Status, category, share type, and HTTP method MUST NOT be conveyed by colour alone; each
MUST carry its name as text or an equivalent non-colour indicator. RFC 2119 keyword
emphasis MUST NOT rely on colour alone — the keyword MUST also be distinguished by weight
or by an accessible name. The sidebar's current-page indication MUST NOT rely on colour
alone.

#### Scenario: Status without colour

- **WHEN** the page is viewed with colour removed
- **THEN** every status badge, category label, HTTP method, and current-page marker MUST
  remain identifiable

## Web Routes

The published route map of the **website**. Every route is anonymously readable; the site
has no authenticated surface. This table describes a static bundle rather than the service
API, and is excluded from the Derived HTTP Reference Page derivation.

| Route | Content | Source |
|-------|---------|--------|
| `/` | Homepage | `website/src/pages/` — hand-written prose, derived counts |
| `/docs/<page>` | Narrative documentation | `website/docs/*.md` — hand-written, tracked |
| `/docs/decisions` | Decisions index | derived from `docs/adrs/ADR-*.md` |
| `/docs/decisions/<adr-id>` | Full decision record | `docs/adrs/ADR-*.md` |
| `/docs/specs` | Specifications index | derived from `docs/openspec/specs/*/` |
| `/docs/specs/<capability>` | Full specification | `docs/openspec/specs/<capability>/spec.md` |
| `/docs/specs/<capability>/design` | Paired design document | `docs/openspec/specs/<capability>/design.md` |
| `/docs/design-language` | Token set, categories, type rules | derived from the token definition |
| `/docs/reference` | HTTP and streaming surface | derived from the specifications' endpoint tables |

The generated routes carry no hand-written content, and their staged sources live in
git-ignored subdirectories of `website/docs/` rather than in tracked files.
`website/docs/architecture.md` and `website/docs/specifications.md` — the hand-maintained
indexes this capability replaces — are deleted. The five links that point at them today
(`docusaurus.config.ts:79`, `:80`, `:105`, `:106` and `src/pages/index.tsx:42`) are
repointed at the generated indexes, and the hand-authored sidebar entries for them are
removed.
