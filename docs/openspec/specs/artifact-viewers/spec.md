---
status: draft
date: 2026-07-08
implements: [ADR-0002, ADR-0011]
requires: [SPEC-0002]
---

# SPEC-0003: Artifact Viewers

## Overview

Cairn renders every artifact inside one app shell (SPEC-0001) but gives each
**share type** a type-specific **body viewer**. This capability formalizes the
per-share-type viewers that render *inside* that shell: **markdown**, **code**,
**image**, **file/generic**, and **bundle**. Each viewer is a server-rendered
`html/template` fragment selected through the ADR-0002 viewer registry, enhanced
with HTMX (server swaps) and Alpine (local interactivity) per ADR-0011.

This spec realizes **ADR-0002** (the extensible share-type model and its viewer
registry, including graceful degradation to the generic-file viewer) and
**ADR-0011** (the server-rendered shell, its body partials, and its accessibility
posture). It depends on **SPEC-0002** for the artifact core, the share-type
registry, content-addressed bodies, and body-download semantics.

Two live viewers — the **webhook inspector** and the **trajectory** waterfall +
activity stream — are their own capabilities (SPEC-0005 and SPEC-0004) and are out
of scope here. Annotation *mechanics* (how a reaction or comment is stored, tallied,
validated, and posted) live in **SPEC-0006**; this spec specifies only each viewer's
annotation **anchor affordances** — what a reader can react to or comment on in that
viewer, and how the render locates that anchor so SPEC-0006 can persist it.

## Requirements

### Requirement: Registry-Driven Viewer Resolution

The shell MUST resolve an artifact's body viewer through the ADR-0002 registry keyed
by the artifact's share type; it MUST NOT `switch` on type in the shell chrome.
Resolution MUST be total: an artifact whose type is unregistered, unknown to this
build, or whose body cannot be interpreted MUST resolve to the **generic-file
viewer** and remain viewable, downloadable, and annotatable at the whole-artifact
level. No share type SHALL be able to render an artifact unviewable.

#### Scenario: Unknown share type

- **WHEN** an artifact carries a share type this build has no registered viewer for
- **THEN** the shell MUST render the generic-file viewer (size, checksum, download)
  and MUST NOT return an error page

#### Scenario: Registered type

- **WHEN** an artifact's share type is registered
- **THEN** the shell MUST render that type's registered body partial and MUST expose
  its declared anchor affordances

### Requirement: Markdown Viewer

The markdown viewer MUST render the artifact body as sanitized HTML with a
**table of contents** derived from its headings. Each rendered block MUST carry the
deterministic `block_id` assigned at ingest (ADR-0006) so anchors resolve to the
same block on every render. The TOC entries MUST be in-page links to their heading
blocks. Rendered markdown MUST NOT execute active content (see Security
Requirements).

#### Scenario: Rendered document with TOC

- **WHEN** a markdown artifact with headings is opened
- **THEN** the viewer MUST show the rendered body and a TOC whose entries link to the
  corresponding heading blocks

#### Scenario: Stable block ids

- **WHEN** the same markdown body is rendered on two requests or two surfaces
- **THEN** each block MUST receive the same `block_id`

### Requirement: Markdown Annotation Anchors

The markdown viewer MUST expose an anchor affordance to **react under a block** and
**react to the left of a bullet**, and MUST expose a **select-text-to-comment**
affordance whose comment thread lands in the **right margin**. Reaction anchors MUST
serialize as `md_block` (`{"block_id":…}`) and `md_bullet`
(`{"block_id":…,"path":[…]}`); comment anchors MUST serialize as `text_selection`
(`{"start","end","quote"}`) over the canonical body text. The viewer MUST NOT offer
anchor affordances outside markdown's declared capability set (ADR-0006); posting is
governed by SPEC-0006.

#### Scenario: React under a block

- **WHEN** a reader activates the react `＋` affordance on a rendered block or bullet
- **THEN** the viewer MUST produce an `md_block`/`md_bullet` anchor carrying that
  block's `block_id` (and bullet `path`) for SPEC-0006 to persist

#### Scenario: Select to comment in the margin

- **WHEN** a reader selects text and starts a comment
- **THEN** the viewer MUST capture a `text_selection` anchor (offsets + quoted
  substring) and render the resulting thread in the right margin

### Requirement: Code Viewer

The code viewer MUST render the body as **syntax-highlighted** source with **line
numbers** and a **symbol outline** (a navigable list of top-level symbols) beside or
above the source. Highlighting MUST be performed server-side and MUST NOT execute the
source. Line numbers MUST correspond one-to-one to the immutable body's lines so they
are permanent coordinates.

#### Scenario: Highlighted source with outline

- **WHEN** a code artifact is opened
- **THEN** the viewer MUST show syntax-highlighted source with line numbers and a
  symbol outline whose entries jump to the corresponding lines

#### Scenario: Language fallback

- **WHEN** the source language is unrecognized
- **THEN** the viewer MUST still render line-numbered, selectable plain source rather
  than failing

### Requirement: Code Annotation Anchors

The code viewer MUST let a reader **react to a single line or a line range** and
**comment on a single line or a text selection**. Reaction anchors MUST serialize as
`code_line` (`{"line":N}`) or `code_range` (`{"start":A,"end":B}`); comment anchors
MUST serialize as `code_line` or `text_selection`. A `code_line` anchor SHOULD carry
a short hash of the line's text so a client can detect it is annotating against a
stale render (ADR-0006). These anchors MUST match code's declared capability set.

#### Scenario: Comment on a line

- **WHEN** a reader activates the comment affordance on line 42
- **THEN** the viewer MUST produce a `code_line` anchor `{"line":42}` for SPEC-0006

#### Scenario: React on a range

- **WHEN** a reader selects lines 40–47 and reacts
- **THEN** the viewer MUST produce a `code_range` anchor `{"start":40,"end":47}`

### Requirement: Image Viewer

The image viewer MUST display the image and support dropping a **pin** at a point on
the image to anchor a region comment, plus a **react-below** affordance for the whole
image. Pin placement MUST use **normalized fractional coordinates** (`0..1`) so a pin
survives display scaling and thumbnailing (ADR-0006). The pin overlay is a bespoke
vanilla-JS/SVG widget (ADR-0011); committing a pinned comment is a server post
handled by SPEC-0006. If the pin script fails to load, the viewer MUST still show the
static image and expose whole-artifact reactions/comments.

#### Scenario: Drop a pin to comment

- **WHEN** a reader drops a pin on the image and writes a comment
- **THEN** the viewer MUST produce an `image_region` anchor with normalized `{"x","y"}`
  (optionally `w`,`h`) for SPEC-0006 to persist

#### Scenario: Pin overlay unavailable

- **WHEN** the pin overlay script fails to load
- **THEN** the viewer MUST still render the image and offer whole-artifact
  reactions/comments

### Requirement: File / Generic Viewer

For non-previewable blobs the generic-file viewer MUST show the file **size**, a
**gzip note** when the body is gzip-compressed, the content **checksum**
(SHA-256/base62 per ADR-0005), and a **download** control that fetches the raw bytes.
It MUST NOT attempt to render the body inline. This viewer MUST also serve as the
universal degradation floor for unknown types. Its only annotation anchor is the
whole `artifact`.

#### Scenario: Non-previewable blob

- **WHEN** a `FILE`/`GZ` artifact (e.g. a `.sql.gz` dump) is opened
- **THEN** the viewer MUST show size, gzip note, checksum, and a download control,
  and MUST NOT inline the body

#### Scenario: Checksum-verifiable download

- **WHEN** a reader downloads the body
- **THEN** the bytes served MUST match the displayed checksum

### Requirement: Bundle Viewer

The bundle viewer MUST present a bundle's member files in a **tabbed** layout, one tab
per file, and MUST render each selected file by **delegating back through the
registry** to that file's own viewer (markdown, code, image, or generic file). Each
member file's viewer MUST expose that file's own anchor affordances; annotations MUST
be anchored so they resolve to the correct member file. The tab strip MUST indicate
each file's type.

#### Scenario: Browse a mixed-media bundle

- **WHEN** a bundle containing a markdown file and an image is opened
- **THEN** the viewer MUST show a tab per file and render the selected file with its
  registered viewer and anchor affordances

#### Scenario: Annotate within a member file

- **WHEN** a reader reacts or comments inside a bundle member file
- **THEN** the anchor MUST resolve to that specific member file, not the bundle as a
  whole

### Requirement: Viewer Renders Inside the Shared Shell

Every viewer MUST render only the **body slot** of the app shell; it MUST NOT
re-implement the header (logo · type badge · title · one URL control with copy +
`◆ mcp` · Share) or the collapsible metadata + comments panel, which are owned by the
shell (SPEC-0001, ADR-0011). A viewer MUST supply only its type-specific
metadata-panel *fields* (e.g. a file's checksum, an image's pin count) for the shell
to place in the panel.

#### Scenario: Consistent chrome across types

- **WHEN** any of the five viewers renders
- **THEN** the surrounding header and panel structure MUST be identical, with only the
  body and the type-specific panel fields varying

### Requirement: Progressive Enhancement

Core reading — open a share, read its body, read its comments — MUST work with server-
rendered HTML alone and MUST degrade gracefully if Alpine/HTMX or a bespoke widget
fails to load. JS-only affordances (image pins, selection-to-comment) MUST fail to an
accessible fallback (a static image, the readable body, whole-artifact annotation)
rather than a blank or broken region.

#### Scenario: Scripts fail to load

- **WHEN** Alpine/HTMX or the pin widget does not load
- **THEN** the body MUST still render and remain readable, and whole-artifact
  reactions/comments MUST remain available

## Security Requirements

This capability is web-facing. The following are MANDATORY.

### Requirement: Authentication & Authorization

Read access to a rendered viewer MUST enforce the ADR-0007 link-capability access
policy: only a client presenting the artifact's link capability (or an authenticated
member with access) MAY view it. The body-download route MUST enforce the same
policy. Any annotation affordance a viewer exposes MUST defer authorization to
SPEC-0006 (posting requires authentication); agents MUST NOT exceed the human's
permissions.

#### Scenario: Read without capability

- **WHEN** a client requests a viewer or a body download without the artifact's link
  capability or member access
- **THEN** the server MUST deny the read (404/403 per the access policy) and render no
  body

### Requirement: Rate Limiting

All viewer-render and body-download endpoints MUST be rate-limited per-identity/per-IP;
exceeding the limit MUST return 429 with `Retry-After`.

#### Scenario: Burst on body download

- **WHEN** a client exceeds the configured rate downloading bodies
- **THEN** the server MUST respond 429 with `Retry-After` and not serve the body

### Requirement: Security Headers

Viewer responses MUST set a strict `Content-Security-Policy`,
`X-Content-Type-Options: nosniff`, `Referrer-Policy`, and (over HTTPS) HSTS.
User-supplied artifact bodies (markdown HTML, code, images, bundle members) MUST be
served/rendered so they cannot execute in Cairn's app origin: markdown MUST be
sanitized, code MUST be escaped/highlighted as inert text, and raw bodies SHOULD be
downloaded from an isolated origin or with `Content-Disposition: attachment` and a
non-executable content type.

#### Scenario: Untrusted markdown body

- **WHEN** a markdown body contains `<script>` or an event-handler attribute
- **THEN** the rendered output MUST be sanitized so it cannot execute in Cairn's origin

#### Scenario: Sniff-proof raw body

- **WHEN** a raw body is downloaded
- **THEN** it MUST be served with `nosniff` and disposition/typing that prevents the
  browser from executing it as active content in the app origin

### Requirement: Request Body Size Limits

The body-download and any viewer-side upload/post endpoints MUST enforce a maximum
response/request size and MUST stream rather than fully buffer large bodies; oversize
requests MUST be rejected with 413 before buffering the full body.

#### Scenario: Oversize request

- **WHEN** a viewer-side request exceeds the configured limit
- **THEN** the server MUST reject it with 413 without buffering the full body

### Requirement: CSRF Protection

Any session-authenticated state-changing action initiated from a viewer (e.g. posting
a pinned comment through the shell) MUST be CSRF-protected via token or SameSite
strategy. Token-authenticated API/MCP requests are exempt (no ambient credentials).

#### Scenario: Cross-site post from a viewer

- **WHEN** a state-changing request from a viewer arrives on a session-auth route
  without a valid CSRF token
- **THEN** the server MUST reject it

### Requirement: Redirect & SSRF Validation

Any viewer-driven redirect (e.g. after a download or share action) MUST target only an
allow-listed internal path; no user-supplied absolute URL SHALL be honored as a
redirect target. If a viewer ever fetches a user-supplied URL server-side, it MUST be
guarded against SSRF.

#### Scenario: Open-redirect attempt

- **WHEN** a viewer request supplies an external redirect target
- **THEN** the server MUST ignore it and redirect only to a safe internal path

## Accessibility Requirements

This capability renders user-facing UI. WCAG 2.1 AA is the minimum target.

### Requirement: WCAG 2.1 AA & Semantics

All viewers MUST meet WCAG 2.1 AA. The body MUST sit within the shell's `main`
landmark; headings/TOC MUST use correct heading semantics and a navigable structure.
Information MUST NOT be conveyed by color alone — code token colors, type badges, and
reaction clusters MUST carry text/shape cues too. Images MUST carry accessible
descriptions.

#### Scenario: Color-only cue

- **WHEN** a viewer distinguishes something by color (e.g. a syntax token or type
  badge)
- **THEN** an equivalent text or shape cue MUST also be present

### Requirement: Icon-Only Controls

Every icon-only control a viewer exposes (copy, download, react `＋`, image pin, bundle
tab close, TOC toggle) MUST have an `aria-label` describing its action.

#### Scenario: Icon button

- **WHEN** a viewer control has no visible text label
- **THEN** it MUST expose an `aria-label` describing its action

### Requirement: Dynamic Content Regions

Viewer regions updated via HTMX swaps (a newly posted comment thread, a loaded bundle
tab) MUST use `aria-live` (`polite` for normal updates) so assistive technology
announces the change without stealing focus.

#### Scenario: Comment posted in a viewer

- **WHEN** a comment thread is swapped into the margin or panel
- **THEN** the update MUST be announced via an `aria-live` region

### Requirement: Keyboard Navigation & Focus Management

All viewer interactive elements MUST be keyboard-operable with a logical tab order:
Enter/Space activate, Escape dismisses popovers, and arrow keys move within composite
widgets (bundle tab list, symbol outline). The reaction picker and any pin/comment
popover MUST trap focus while open and restore focus to the trigger on close. Image
pins MUST be reachable and placeable/openable by keyboard (or provide an equivalent
keyboard path to region annotation).

#### Scenario: Keyboard-only tabbing through a bundle

- **WHEN** a keyboard user moves through bundle tabs
- **THEN** arrow keys MUST move between tabs, Enter/Space MUST activate one, and focus
  MUST be visible on the dark canvas

#### Scenario: Keyboard-only reaction

- **WHEN** a keyboard user opens a viewer's reaction picker
- **THEN** focus MUST move into it, cycle within it, and return to the trigger on close

## HTTP Endpoints

Viewers are rendered by, and read bodies from, the following core routes (ADR-0012).
Auth-by-default: reads are `Auth: Public` only because they are gated by the ADR-0007
**link capability**, which is itself the access credential; all other access is
`Auth: Required`.

| Method & Path | Purpose | Auth |
|---------------|---------|------|
| `GET /{id}` (web) | Render the app shell + resolved body viewer | Public — gated by link capability (ADR-0007); returns 404/403 without it |
| `GET /v1/artifacts/{id}` | Fetch metadata + rendered/preview payload for a viewer | Public — gated by link capability (ADR-0007) |
| `GET /v1/artifacts/{id}/body` | Download raw, checksum-verifiable bytes (file/bundle download) | Public — gated by link capability (ADR-0007) |

No new mutating endpoints are introduced by this spec; annotation posts a viewer
initiates are the SPEC-0006 endpoints and are `Auth: Required`.
