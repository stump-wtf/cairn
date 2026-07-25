# Design: The Public Website and the Published Design Record

## Context

ADR-0014 reduces to one sentence: **a fact about the design record lives in exactly one
file.** Everything below follows from deciding where each fact lives and how it reaches
the page.

The site as it stands has three defects and one set of assets, and the design has to
respect both. The defects: `website/docs/architecture.md` is a hand-typed table of
decision rows that stops at ADR-0012 while `docs/adrs/` on the default branch already
holds ADR-0013; `website/docs/specifications.md` states requirement and scenario totals in
prose that no process maintains; and every link out of both pages, plus the navbar, the
footer, the hero CTA, and the `editUrl` at `docusaurus.config.ts:47` that puts an "Edit
this page" link on *every* docs page, resolves to a repository that is not publicly
readable. There is no `/decisions` tree and no `/specs` tree, so not one line of the record
renders on the site.

The assets: the homepage and its feature grid are fully bespoke Cairn work, not a
scaffold, and `website/src/css/custom.css` already carries the ADR-0009 category accents
plus a neutral default, the IBM Plex Sans / JetBrains Mono pairing, and a dark-first
surface ramp. That file is the canonical palette. This design restyles surfaces,
typography, and chrome around it; it does not repaint it.

There are three kinds of content on this site and it is worth being precise about which
is which, because the failure mode is treating one as another:

| Kind | Source of truth | Written by | Example |
| --- | --- | --- | --- |
| **Record** | `docs/adrs/`, `docs/openspec/specs/` | ADR and spec authors | ADR-0009's Decision section |
| **Derived** | computed from the record at build time | nobody | index rows, `ACCEPTED` badges, requirement counts, "referenced by SPEC-0004" |
| **Narrative** | `website/docs/*.md`, homepage source | site authors | "Cairn is a pastebin reimagined for the agent era" |

The bug class this design eliminates is *derived content written by a human*. Those pages
are deleted, not updated.

## Goals / Non-Goals

### Goals

- Render the full text of every ADR and every capability specification on the public
  site, with no external hop and no forge account.
- Compute every index row, badge, date, count, and cross-reference from the record, so
  adding a decision publishes it with zero edits under `website/`.
- Compute the inverse graph edges the record's forward-only authoring convention
  deliberately omits, and de-duplicate the relationships that are already authored from
  both ends.
- Keep record source files plain CommonMark; make the renderer bend, not the authors.
- Fail the build — and the development server — on a broken record rather than shipping a
  quietly wrong page.
- Reach the mockup's chrome through tokens and CSS modules, spending as close to zero
  theme overrides as the layout allows.

### Non-Goals

- Redefining the category palette. ADR-0009 owns it, SPEC-0004 makes the neutral default
  normative, and `custom.css` already implements both. Out of scope here.
- The in-product app shell. That is SPEC-0001 and ADR-0011: shared design language,
  separate implementation, separate deploy.
- Documentation versioning. The record has no versions today.
- Site search. The site ships none, and adding it is a dependency decision of its own —
  see Open Questions.
- Changing how ADRs and specifications are authored. This capability reads the record; it
  does not get to reshape it.

## Decisions

### Generation runs before plugin initialisation, and again on watch

The tempting shape — a Docusaurus plugin whose `loadContent()` stages the record for the
docs content plugin to serve — does not work, and it fails in two different ways that are
worth writing down so nobody rebuilds it.

*It hard-fails on a clean checkout.* `plugin-content-docs` resolves and stat-checks its
content directory in the **plugin factory**
(`plugin-content-docs/lib/index.js:51` → `readVersionsMetadata` → `versions/files.js:135`,
which throws *"The docs folder does not exist for version …"*). Factories are awaited by
`initPlugins`, which completes before any plugin's `loadContent()` runs
(`core/lib/server/plugins/plugins.js`: `initPlugins()` then
`executeAllPluginsContentLoading()`). A staging directory that is git-ignored does not
exist in CI, so every CI build is a cold build and every cold build throws.

*It lags one build behind when it does run.* `executeAllPluginsContentLoading` runs every
plugin's `loadContent()` inside a single `Promise.all`, so the docs plugin globs the
staging directory *concurrently* with the record plugin writing it. Build N publishes what
build N−1 staged — the same derived-content drift this capability exists to abolish, at a
shorter period.

Moving generation into the record plugin's own factory does not fix it either: plugin
factories are themselves initialised with `Promise.all`
(`core/lib/server/plugins/init.js`), so the two factories race.

The first hook that is reliably *before* all plugin initialisation is the site config
itself. `loadSiteConfig` awaits a config module that exports a function
(`core/lib/server/config.js`: `typeof importedConfig === 'function' ? await
importedConfig() : await importedConfig`), and `loadContext` — which loads the config —
completes before `loadPlugins` (`core/lib/server/site.js`). So:

- **`docusaurus.config.ts` exports an async factory** that awaits the record generator
  before returning the config object. This runs on `docusaurus build` and on
  `docusaurus start` alike, so a clean checkout is correct on its first build and there is
  no one-build lag.
- **A thin record plugin handles the development loop.** `getPathsToWatch()` returns
  `docs/adrs/**` and `docs/openspec/specs/**`; `loadContent()` re-runs the same generator
  so an in-flight edit re-stages; `contentLoaded()` publishes the derived data module for
  the components that consume it at render time. On the dev server the generated tree is
  already current from config load, so the re-run is a refresh rather than a first write —
  and the docs plugin's own watcher picks up the rewritten files.

The generator is one module called from two places. It is deliberately *not* an
`npm run generate && docusaurus build` chain, because that chain is the thing that leaves
`docusaurus start` serving a frozen snapshot while an author edits ADRs and concludes the
pipeline is broken.

The staged output is git-ignored. That single fact is what separates this approach from
"copy the files in": there is no tracked copy to review, to diff, or to edit by hand, and
therefore no copy to rot.

### One docs instance, one content root

`plugin-content-docs` takes a single `path` string — `options.js:64` is
`path: Joi.string().default(...)`, with no multi-root form. So "the narrative pages come
from `website/docs/` and the record comes from a separate generated directory, both served
by one instance" is not constructible.

The record is therefore staged **into git-ignored subdirectories of the existing docs
root**: `website/docs/decisions/` and `website/docs/specs/`, with both paths in
`.gitignore`. One content root, one instance, one sidebar. The hand-written narrative
pages stay exactly where they are and stay tracked; the generated trees sit beside them and
are never committed.

This also removes the cold-build failure mode described above at its root, because
`website/docs/` always exists — it holds the tracked narrative pages. The config-time
generation is still required, for the one-build-lag reason.

### The route shape that follows

Docusaurus sidebars are strictly per content instance. A `plugin-content-docs` instance
renders only its own sidebar; a reader inside a second instance sees that instance's tree
and nothing else. Mounting `/decisions` and `/specs` as two extra instances would
therefore give a reader on an ADR page no way to see the specs tree, and a reader on a
spec page no way to see the ADRs — which is the opposite of publishing a connected record.

Two ways out. Wrap `DocSidebar` — `theme-classic`'s swizzle config marks it
`{eject: unsafe, wrap: safe}`, so wrapping it is sanctioned — and render a unified tree
from the derived data module; or use a single instance and let the route shape follow.
The wrap is available and is the documented fallback, but it spends the one-wrap budget on
navigation and means owning active-link detection, collapse state, and the keyboard
semantics the stock component already handles correctly, in exchange for one URL segment.

Chosen: **a single documentation content instance**, with the record beneath the docs
route base as `/docs/decisions/…` and `/docs/specs/…`. The cost is honest and small — the
mockup's breadcrumb already reads `docs / decisions / ADR-0009`. The benefit is one
sidebar, one prev/next sequence, and zero swizzles spent on navigation.

The existing `/docs/…` navbar and footer entries mostly keep working, but not all of them:
`docusaurus.config.ts:79`, `:80`, `:105` and `:106` point at `/docs/architecture` and
`/docs/specifications`, and `src/pages/index.tsx:42` sends the hero's secondary CTA to
`/docs/architecture`. Those five targets are the two hand-maintained index pages this
capability deletes, so all five are repointed at the generated indexes as part of the
change; `onBrokenLinks: 'throw'` is what stops one being missed.

**The sidebar configuration cannot read the derived data module.** `sidebars.ts` is not a
React module — it is loaded Node-side from inside the docs plugin's own content-loading
path (`plugin-content-docs/lib/sidebars/index.js`: `loadSidebarsFileUnsafe` →
`loadFreshModule`), and it is consumed as a *value*, never invoked, so an exported function
is not a way in either. That load happens concurrently with the record plugin's
`loadContent()` and strictly before any `contentLoaded()` has run, so there is no ordering
in which an import of the data module could be current. (This is why the `DocSidebar` wrap
above remains a genuine alternative: a wrapped React component renders long after all
content is loaded and *can* import the module. The Node-side config file cannot.)

The sidebar is therefore **`type: 'autogenerated'`** over `website/docs/`, and the
generator writes a `_category_.json` into each staged directory carrying the group label
(counts included in the label text). Because generation happens at config-load time, those
files are on disk before the docs plugin reads them. The derived data module remains the
source for everything rendered *inside* a page; the sidebar is built from the staged tree
itself.

### Components come from the AST, never from the text

The record pages need real components — a requirement block, a scenario block, distinguished
RFC 2119 keywords, status badges, traceability chips. There is a trap here: you cannot
have both un-escaped `<` / `{` and embedded component markup in the same file. The moment
a document contains literal JSX, every bare `<` in it becomes a parse error, and record
prose is full of them.

So the record text is never touched. Structure recognition happens on the parsed tree: a
remark plugin, registered ahead of the default plugins, walks the mdast, matches the
`### Requirement:` heading shape and the `#### Scenario:` + `- **WHEN**` / `- **THEN**`
list shape, and rewrites those subtrees into JSX flow nodes bound to the site's
components. A second visitor wraps RFC 2119 keywords in text nodes.

Because every rewrite happens after parsing, the surrounding prose is never re-parsed as
MDX. A bare `<` in an ADR paragraph stays a text node and cannot become a tag. The record
stays CommonMark; the page gets components; neither compromises the other.

**Raw-HTML-shaped words are the one case CommonMark does not hand back.** `<` and `{` on
their own are safe under `markdown.format: 'detect'`, but a token like `<id>` is a valid
CommonMark *open tag*, so it parses to an `html` node, survives into the DOM as a real
`<id>` element, and — worse — is swallowed by the auto-derived page excerpt, which then
ships mangled text in `<meta name="description">` and `og:description`. This pattern
occurs in the record. The generator therefore rewrites `html` nodes originating in record
prose into literal text nodes before the tree is handed on, and derives each page's
description from the transformed tree rather than letting Docusaurus excerpt the raw
source.

### No repository links at all

ADR-0014 drops the "view source" affordance entirely, and the pipeline drops `editUrl`
with it. There is nothing to link to that the page does not already contain, so
`DocItem/Footer` — which has no swizzle entry and therefore defaults to unsafe — needs no
override, and the theme-override budget survives intact.

The build-time check bans links to *any* repository host outside an explicit allowlist,
rather than one hardcoded hostname. A hostname blocklist stops being true the first time
the project moves forges; an allowlist stays true.

### Validation runs where it can actually run

Dangling `implements`, malformed front-matter, unresolved graph edges, and colour literals
in site source are all *source-level* checks. They live in the generator, so they run at
config load on every build *and* every `docusaurus start`, and an author who breaks a
cross-reference finds out in the dev server rather than in CI. The generator throws;
Docusaurus never finishes loading the config.

The link-host allowlist is not a source-level check and cannot join them. It is defined
against *rendered output*, and `docusaurus start` never renders HTML to disk — so it runs
as a `postBuild` scan over the built bundle, which means it is a CI check rather than a
dev-server one. That is a real gap in feedback latency and it is accepted: the alternative,
scanning source for link-shaped strings, misses links assembled at render time and is the
weaker check.

## Architecture

```
                    docusaurus.config.ts  (async config factory)
                              │  await generateRecord()      ← before initPlugins()
docs/adrs/ADR-*.md ─┐         ▼
                    ├─▸ website/plugins/record/generate.ts
docs/openspec/…    ─┘         ├─ parse front-matter + headings
                              ├─ build the reference graph (both directions, de-duped)
                              ├─ remark: Requirement / Scenario / RFC-2119 → components
                              ├─ validate → throw on any integrity failure
                              ├─▸ website/docs/decisions/   ┐ git-ignored,
                              ├─▸ website/docs/specs/       ┘ + _category_.json
                              └─▸ record data module  (lists, counts, resolved graph)
                                        │
   website/docs/*.md  (tracked) ─────────┤
                                         ▼
                   one plugin-content-docs instance, path: 'docs'
                                         │
   website/plugins/record/ (thin plugin) ─┤  getPathsToWatch() → record trees
        loadContent() re-runs generate    │  contentLoaded()   → data module
                                          ▼
                            postBuild: link-host scan ─▸ static bundle
```

**The derived data module.** One artifact carries the decision list, the specification
list, the counts, and the resolved graph. The homepage imports it for its counts and the
generated index pages render from it. That is how a decision total reaches the marketing
page without anyone typing a number. The sidebar is the one consumer that cannot import it
— see above — and is autogenerated from the staged tree instead.

**Inverse edges.** The record's convention is forward-only; the newer ADRs state it in a
front-matter comment and the older ones follow it without saying so. The generator walks
every `extends` / `enables` / `related` /
`implements` / `requires` edge and inverts it, so readers get both directions for free.
De-duplication is not optional: `ADR-0001` declares `enables: [ADR-0002, ADR-0003]` while
`ADR-0002` and `ADR-0003` each declare `extends: [ADR-0001]`, so the same relationship is
already authored twice. Edges are normalised to `{a, b, kind}` with a canonical
orientation before rendering.

**Numbering.** Specification numbers are read from each `# SPEC-XXXX:` heading. Directory
names do not sort into specification order — the alphabetically first capability
directory is not SPEC-0001 — so anything that sorts by dirname produces a wrong index.

**Theming ladder**, climbed only as far as necessary:

1. **Custom properties in `src/css/custom.css`** — the token set, plus the `--ifm-*`
   mappings that pull stock components onto the palette. Most of the design lands here.
   The five hex literals living outside that file are cleared as part of this work, and
   they are not all the same job:
   `index.module.css:113-115` restate `--cat-write` / `--cat-net` / `--cat-exec` and are a
   straight substitution; `index.module.css:53` uses `#b49bff` as a gradient midpoint that
   matches no token, so it needs either a named surface token minted for it or the gradient
   rewritten to interpolate between the two tokens it already sits between; and
   `HomepageFeatures/styles.module.css:121` is `.cat_mono { --accent: #aab2bd; }`, a class
   naming a category ADR-0009 does not define, which is remapped to `var(--cat-other)` —
   the same colour, but reached through the neutral-default token instead of copied.
2. **Component-scoped CSS modules** — the bespoke pieces with no theme counterpart:
   decision index rows, spec cards, requirement blocks, metadata bars.
3. **A theme wrap** — at most one, and only if the layout genuinely cannot be reached from
   CSS. Wrapping is sanctioned by Docusaurus (41 of the 55 components `theme-classic`
   configures are `wrap: safe`), so the cap is a maintenance budget, not a safety rule: each
   wrap is a component whose stock behaviour has to be understood and kept working locally.

Nothing is ejected. An eject copies component internals that may change in a minor
release, so a site that ejects takes a routine upgrade as a breakage. That, not wrapping, is
the unsafe action.

**Component inventory** — all render from the data module or from the transformed AST,
none hold content: `StatusBadge` (text plus colour, never colour alone), `RecordIndexRow`,
`SpecCard`, `TraceChips` (forward and inverse edges, each linked), `RequirementBlock`,
`ScenarioBlock`, `Rfc2119` (weight plus accessible name), `TypeBadge` (shared with the
homepage, tinted by category token), `Callout`, and `Waterfall` (homepage only, static
data).

**Homepage.** A plain React route under `src/pages/`, narrative rather than derived, with
one exception: it imports the data module for its counts. The surface-parity section is a
client-side tab set and is the only stateful thing on the page; it degrades to a stacked
layout without JavaScript, because a marketing page that renders blank without JS is a
poor advertisement for a project that server-renders its actual product.

**Contrast, on the dark ramp.** The mockup's tertiary label colour `#565E6E` measures
3.02:1 on the `#0A0B0D` page ground and 2.82:1 on the shipped elevated surface
(`--cairn-elevated: #131418`, `custom.css:55` — the mockup's own `#111317` gives 2.85:1 and
does not ship). It fails AA for normal text on both surfaces, and fails even the 3:1
large-text floor on the elevated surface. On the page ground alone it would technically
clear 3:1 for large text; it is nonetheless barred from text at any size, as a policy
call — the elements the mockup uses it for (breadcrumb, monospace sidebar group labels,
eyebrow kickers, index-row dates) are all small text, and a colour whose legality depends
on which surface it lands on is a trap. The shipped `--cairn-muted` (`#9ba1a8`) replaces it
and measures 7.55:1 and 7.06:1 on those two surfaces.

**Contrast, and the light ramp.** The category accents are declared once at `:root` and are
constant across colour modes. As foreground on the dark ground they run 5.68:1
(`--cat-fail`) to 11.22:1 (`--cat-search`), and 5.09:1 to 9.22:1 on the shipped 12 % chip
tint over that ground — comfortably clear. On the light ground (`--cairn-bg: #ffffff`) not
one of the fourteen reaches 4.5:1; the best is `--cat-fail` at 3.46:1, eleven fail even
3:1, and over the 12 % tint `--cat-search` bottoms out at 1.64:1. The palette is fixed by
ADR-0009 and is not being recoloured, and the colour-mode switch currently ships
(`docusaurus.config.ts:61-65`, `disableSwitch: false`). The resolution is therefore on the
usage side: a category accent is a **dark-ground text colour**. Chips, badges, and
waterfall bars that render an accent as text keep the dark surface ramp in both colour
modes; anywhere that is not acceptable, the accent becomes a non-text indicator and the
category name carries the meaning in a text token. Whether the site should offer a light
mode at all is an open question below.

**Deployment.** The build is a static bundle. `onBrokenLinks` and `onBrokenAnchors` are
both set to `throw` — noting that neither fires during `docusaurus start`, only on a full
build, so CI has to run one for the setting to mean anything. The deploy trigger is
extended to cover `docs/adrs/**` and `docs/openspec/specs/**`; without that, the headline
promise of ADR-0014 is false, because a commit that adds a decision matches no path in
the current filter and never deploys. A pull-request build job is added so a record change
that breaks the site fails before merge rather than after.

**Security headers do not fit the current deploy target.** The workflow publishes via
`actions/upload-pages-artifact@v3` + `actions/deploy-pages@v4` (`deploy.yml:45-60`) — GitHub
Pages, which serves static files and offers no way to set response headers. The only channel
left is `<meta http-equiv="Content-Security-Policy">`, and the CSP specification requires
`frame-ancestors` to be *ignored* when delivered that way; `X-Content-Type-Options`,
`Referrer-Policy`, and HSTS have no `<meta>` form that browsers honour at all. So the header
posture the spec requires is not deliverable on the pinned host. This is recorded as an open
question rather than quietly softened: the site needs either a header-capable host, a proxy
or CDN in front of Pages, or an explicit accepted exception. Meta-CSP is deployed in the
meantime for the directives it does enforce, and the bundle scan — which is a `postBuild`
step and host-independent — carries the weight it can.

## Risks / Trade-offs

- **Front-matter is now load-bearing** → a missing `status:` in an ADR fails the site
  build, and the author of that ADR may have no idea the site exists. Mitigation: the
  error names the file and the field, and the same validation runs in the dev server, so
  anyone previewing the record sees it immediately.
- **Longer record URLs** → the single-instance decision puts the record at
  `/docs/decisions/…` rather than `/decisions/…`, one segment deeper than the mockup's
  navbar implies. Accepted in exchange for a unified sidebar and zero navigation
  swizzles; the `DocSidebar` wrap remains available if top-level routes ever become a
  requirement.
- **The remark transform is coupled to the record's heading conventions** → it matches
  `### Requirement:` and `#### Scenario:` by shape. A spec that renames those headings
  silently loses its component rendering. Mitigation: the transform counts what it
  matched and the count feeds the spec cards, so a spec that suddenly reports zero
  requirements is visible on the index.
- **CSS-heavy theming degrades quietly** → because almost nothing is swizzled, a
  Docusaurus upgrade that changes class names or DOM structure produces a visual
  regression rather than a build error. Mitigation: keep the wrap budget at one so
  the surface area of "things that can silently drift" stays small, and treat a major
  upgrade as requiring a visual pass.
- **Generation sits ahead of the standard build entry point** → because it runs from the
  async config factory, `docusaurus build` and `docusaurus start` both work, but anything
  that loads the site by another path (a tool that imports `docusaurus.config.ts`
  synchronously, a future programmatic build) will not see the staged tree. Mitigation:
  the clean-checkout build in CI is the canary, and the generator is idempotent so calling
  it twice is harmless.
- **Self-hosting fonts adds bundle weight** → `custom.css:8` imports both families from
  `fonts.googleapis.com`, which is smaller to ship and worse for readers' privacy and for
  the site's availability. Accepted: subset the families and serve them first-party.
- **Deriving the reference page from spec tables depends on those tables' shape** → the
  record names those sections five different things (`## HTTP Endpoints` ×2,
  `## REST Endpoints`, `## Endpoint Table` ×2, `## Web Routes`, and this specification's
  own `## Web Routes`), their columns are not identical, and three of the nine product
  specifications — `cli`, `trajectory-share`, `webhook-inspector` — carry no such section
  at all, so the derived page cannot claim to be a complete API surface. If normalising
  proves brittle, the spec requires dropping the page rather than hand-maintaining it.
- **The security-header posture is not deliverable on GitHub Pages** → see Deployment
  above. Until the host question is answered, the site ships meta-CSP plus a bundle scan
  and does not have `frame-ancestors`, `nosniff`, `Referrer-Policy`, or HSTS.
- **No search over a much larger corpus** → publishing the whole record makes the site's
  missing search conspicuous in a way six narrative pages did not.

## Open Questions

- Should the site have search? It has none today — no search dependency, no Algolia
  configuration — and the record is the largest corpus it will ever serve. A local index
  adds a real dependency. Note that the site does *not* currently use the Rspack-based
  faster build: `@docusaurus/faster` is a declared dependency (`package.json:19`) but
  nothing enables it (`docusaurus.config.ts:12-14` sets only `future: {v4: true}`), so the
  bundler-compatibility caveats attached to some search plugins do not apply as things
  stand. Explicitly out of scope for this capability; it wants its own decision.
- Where should the site be hosted? The security requirements ask for real response
  headers and GitHub Pages cannot set any. Options: move to a header-capable static host,
  put a CDN in front of Pages, or accept a documented exception. This blocks the Security
  Headers requirement from being fully satisfiable and is the most urgent question here.
- Should the site offer a light colour mode at all? It ships one today
  (`disableSwitch: false`), the brand is explicitly dark-first, and the ADR-0009 category
  accents cannot meet AA as text on a light ground without being recoloured — which is out
  of bounds. Turning the switch off would make the contrast story trivially true;
  keeping it means every accent-as-text surface has to hold the dark ramp.
- Is the reference page derivable in practice from five differently-named,
  differently-shaped endpoint tables covering only six of nine specifications, or is
  normalising them a bigger job than it looks?
- Should the paired `design.md` render as a sibling page of its specification, or as a
  tab within it? The route table assumes a sibling page.
- Where does the design brief in `docs/DESIGN.md` belong — is it a fourth generated tree,
  a narrative page, or does it stay unpublished?
- Does the homepage's trajectory waterfall use static illustrative data, or should it read
  a real captured trajectory fixture so it cannot misrepresent the product?
- What is the accepted `status` enum for ADRs, precisely? The validator needs a closed
  list, and today every record in the tree is `accepted` or `draft`, so the proposed and
  superseded paths are untested.
