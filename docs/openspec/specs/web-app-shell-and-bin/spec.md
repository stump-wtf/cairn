---
status: draft
date: 2026-07-08
implements: [ADR-0011, ADR-0003]
requires: [SPEC-0002]
---

# SPEC-0001: Web App Shell and the Bin

## Overview

Cairn presents every share type — markdown, code, image, generic file, bundle, live
webhook, trajectory — inside a single web app shell: a header (logo · type badge ·
title · one URL control with copy + `◆ mcp` · Share) wrapping a collapsible right-hand
metadata + comments panel and a type-specific body slot, at a consistent size and
chrome. This capability owns that shared **shell frame**, the **URL control and Share
affordance**, the **collapsible panel behavior**, and the **Bin** — the artifact
listing (rows with type badge, title, provenance, and reaction/comment counts, an
empty state, and pagination).

It realizes ADR-0011 (server-rendered `html/template` + HTMX + Alpine.js, one shell for
all seven bodies, live regions over SSE, WCAG 2.1 AA) and ADR-0003 (the web surface is a
thin adapter over the one core; the Bin is the *same listing* the CLI TUI projects). It
depends on SPEC-0002 for the Artifact aggregate, the share-type registry, the public-id
URL scheme, and the Bin query it renders. The **per-type body rendering** (rendered
markdown, image pins, the trajectory waterfall, the webhook inspector, etc.) is
**SPEC-0003** and is out of scope here: this spec owns only the frame the bodies mount
into and the invariant that the frame is identical for every type.

## Requirements

### Requirement: Unified App Shell

The web app MUST render a single shell — header, collapsible metadata + comments panel,
and a type-specific body slot — from one base layout, so the header, URL control, and
panel are a single implementation reused for every share type. The shell MUST NOT
`switch` on share type to build its chrome; only the body slot and the panel's
type-specific metadata fields vary by type, and those are supplied by the registry
(SPEC-0002), not by the shell. Adding a share type MUST require adding a body partial,
not editing the shell.

#### Scenario: Same chrome across types

- **WHEN** any two artifacts of different share types are rendered
- **THEN** both MUST present an identical header structure (logo · type badge · title ·
  one URL control with copy + `◆ mcp` · Share) and the same collapsible panel structure,
  with only the badge, title, panel fields, and body differing

#### Scenario: New type mounts without shell edits

- **WHEN** an artifact whose share type resolves to a newly registered body partial is
  rendered
- **THEN** the shell MUST place that partial in the body slot without any change to the
  header, URL control, or panel implementation

### Requirement: Header Composition

The header MUST contain, in order, the `cairn` logo/wordmark, the share type badge, the
artifact title, exactly one URL control (below), and a Share button. The badge and title
are the only header data that vary by artifact; every other element MUST be identical
across types.

#### Scenario: Header elements present

- **WHEN** an artifact shell is rendered
- **THEN** the header MUST expose the logo, a type badge, the title, one URL control, and
  a Share button, and MUST NOT render a second URL control

### Requirement: One URL Control with Copy and MCP Affordance

The header MUST present exactly one URL control showing the artifact's short public link
(`cairn.sh/<id>`, or `cairn.sh/run/<id>` for a trajectory per SPEC-0002). It MUST provide
a copy action that copies the link, and an `◆ mcp` affordance that yields the
corresponding `mcp://cairn/...` handle for the same id. The copy action SHOULD complete
locally without a server round-trip.

#### Scenario: Copy the link

- **WHEN** the user activates the copy action on the URL control
- **THEN** the artifact's short public link MUST be placed on the clipboard and success
  MUST be indicated

#### Scenario: MCP handle for the same id

- **WHEN** the user activates the `◆ mcp` affordance
- **THEN** the control MUST surface the `mcp://cairn/...` handle carrying the same public
  id as the web link

### Requirement: Share Affordance

The Share button MUST open a dialog through which the authenticated owner adjusts the
artifact's link access policy. The dialog MUST invoke the core sharing operation
(SPEC-0002, `POST /v1/artifacts/{id}/share`); the shell MUST NOT re-implement access
rules locally. A non-owner or unauthenticated viewer MUST NOT be able to change sharing.

#### Scenario: Owner opens Share

- **WHEN** the authenticated owner activates the Share button
- **THEN** a Share dialog MUST open exposing the current link access policy and the
  controls to adjust it

#### Scenario: Non-owner cannot mutate sharing

- **WHEN** a viewer who is not the owner attempts to submit a sharing change
- **THEN** the server MUST reject it and the policy MUST remain unchanged

### Requirement: Collapsible Metadata + Comments Panel

The shell MUST render a right-hand panel holding type-specific metadata above the
comments thread. The panel MUST be collapsible; its collapsed/expanded state is view-local
(Alpine) and MUST NOT require a server round-trip to toggle. The toggle control MUST
expose its state via `aria-expanded`. Posting a comment MUST be a server round-trip
(HTMX swap) that reaches the core, not a client-only update.

#### Scenario: Toggle the panel

- **WHEN** the user activates the panel toggle
- **THEN** the panel MUST collapse or expand without a full-page reload and the toggle's
  `aria-expanded` MUST reflect the new state

#### Scenario: Comment posts through the core

- **WHEN** the user submits a comment from the panel
- **THEN** the comment MUST be persisted via the core operation and the thread MUST be
  updated by swapping in the server-rendered result

### Requirement: Type-Specific Body Slot

The shell MUST resolve the body partial for an artifact through the share-type registry
(SPEC-0002) and mount it in the body slot. Resolution MUST be total: an unrecognized or
unregistered type MUST fall back to the generic file body so the artifact remains
viewable and shareable. The shell MUST NOT contain per-type rendering logic.

#### Scenario: Unknown type falls back

- **WHEN** an artifact of an unregistered share type is rendered
- **THEN** the shell MUST mount the generic file body in the slot rather than fail, and
  the header and panel MUST render normally

### Requirement: Progressive Enhancement

Core reading — opening a share, reading its body, and reading its comments — MUST work
from server-rendered HTML and MUST degrade gracefully if HTMX or Alpine fail to load.
JS-only enhancements (local pickers, the waterfall, image pins in SPEC-0003) MUST fail to
an accessible fallback (the static content) rather than a blank region.

#### Scenario: Scripts fail to load

- **WHEN** HTMX/Alpine assets do not load
- **THEN** the artifact body and its comments MUST still be readable from the
  server-rendered HTML

### Requirement: The Bin Listing

The web app MUST render the Bin as a listing of the workspace's artifacts, reusing the
same base layout with a listing body. Each row MUST show the share type badge, the title,
the provenance (actor · channel · relative age, e.g. `claude · via mcp · 1d`), and the
reaction/comment counts (e.g. `💬 2 · 👀 3`). The Bin MUST project the same server-side
query the CLI TUI projects (ADR-0003), so both are provably the same listing. Rows MUST
be ordered by stored `created_at` (never by id, per SPEC-0002).

#### Scenario: Row content

- **WHEN** the Bin renders a row for an artifact
- **THEN** the row MUST display the type badge, title, provenance, and reaction/comment
  counts for that artifact

#### Scenario: Ordering

- **WHEN** the Bin lists artifacts
- **THEN** rows MUST be ordered by `created_at`, not by public id

### Requirement: Bin Empty State

When the workspace has no listable artifacts, the Bin MUST render an explicit empty state
rather than a blank listing, and MUST NOT surface a spurious error.

#### Scenario: No artifacts

- **WHEN** a workspace with zero listable artifacts opens the Bin
- **THEN** an empty-state message MUST be shown in place of rows

### Requirement: Bin Pagination

The Bin MUST paginate using the keyset (cursor) model of the core Bin query (SPEC-0002),
so the listing neither skips nor duplicates rows while artifacts are inserted and expired
mid-scroll. "Load more" MUST fetch the next page as an HTMX partial swap, not a full-page
reload.

#### Scenario: Load more under churn

- **WHEN** the user loads additional Bin pages while artifacts are being created and
  expired
- **THEN** the keyset cursor MUST advance without skipping or repeating rows already shown

### Requirement: Settings & Connect Instructions

The web app MUST provide a single, sectioned Settings page (`/settings`) covering: API
tokens (create with a name and a scope subset, view the plaintext secret exactly once at
creation, list existing tokens by name/scopes/created/last-used, revoke), MCP connection
(the server URL, the OAuth 2.1 + PKCE note, the three consent scopes, copy-paste connector
steps), CLI (install + login instructions once the `cairn` CLI ships, else an explicit
"not yet released" state that fabricates no working command), and Account (the signed-in
identity and a sign-out control). The shell header/nav MUST carry a persistent entry point
into Settings. Settings MUST require authentication like the Bin (unauthenticated → login
redirect with a validated `?next`); the API tokens section's management actions MUST be
CSRF-guarded. A legacy `/connect` URL MUST redirect (permanently) to `/settings`.

#### Scenario: Settings entry point

- **WHEN** an authenticated caller views the app shell header
- **THEN** a Settings entry point MUST be present and MUST navigate to `/settings`

#### Scenario: One-time token secret

- **WHEN** an authenticated caller creates a new API token
- **THEN** the plaintext secret MUST be shown exactly once in that response and MUST NOT
  be retrievable again from any later request

#### Scenario: Legacy /connect redirects

- **WHEN** any caller requests `/connect`
- **THEN** the server MUST respond with a permanent redirect to `/settings`

#### Scenario: Unauthenticated Settings access

- **WHEN** an unauthenticated client requests `/settings`
- **THEN** the server MUST redirect to login rather than render any Settings content

## Security Requirements

This capability is web-facing. The following are MANDATORY.

### Requirement: Authentication & Authorization

Workspace-scoped and mutating shell routes MUST require authentication (web session per
ADR-0004). Rendering an individual artifact page is a link-capability read: possession of
a valid public id grants read per ADR-0007, so `GET /{id}` and `GET /run/{id}` are the
only Public routes and unknown/unauthorized/expired ids MUST return a uniform 404. The
Bin and the Share dialog MUST require authentication, and no viewer MUST be able to change
sharing without authenticated ownership.

#### Scenario: Unauthenticated Bin access

- **WHEN** an unauthenticated client requests the Bin listing
- **THEN** the server MUST require authentication and MUST NOT list the workspace's
  artifacts

#### Scenario: Uniform 404 for unknown id

- **WHEN** a client requests an artifact page for an unknown, unauthorized, or expired id
- **THEN** the server MUST respond with an identical 404 in every case, leaking no signal

### Requirement: Rate Limiting

Public and ingress routes — chiefly `GET /{id}` / `GET /run/{id}` id resolution — MUST be
rate-limited per-identity/per-IP to make id enumeration infeasible; limits MUST return 429
with `Retry-After`.

#### Scenario: Enumeration burst

- **WHEN** a client exceeds the configured rate resolving artifact ids
- **THEN** the server MUST respond 429 with `Retry-After` without processing further
  lookups

### Requirement: Security Headers

Every shell response MUST set a strict Content-Security-Policy, `X-Content-Type-Options:
nosniff`, a `Referrer-Policy`, and (over HTTPS) HSTS. User-supplied artifact bodies MUST
be served/rendered so they cannot execute in Cairn's app origin.

#### Scenario: Untrusted HTML/markdown body

- **WHEN** an artifact body contains active content (script/HTML)
- **THEN** it MUST be sanitized or isolated so it cannot run in Cairn's origin, and the
  CSP MUST still forbid inline execution in the shell

### Requirement: Request Body Size Limits

Shell endpoints that accept a request body (e.g. comment posts, the Share form) MUST
enforce a maximum request size; oversize requests MUST be rejected with 413 before the
full body is buffered. (Artifact body uploads are governed by SPEC-0002.)

#### Scenario: Oversize comment post

- **WHEN** a comment or form submission exceeds the configured limit
- **THEN** the server MUST reject it with 413 and persist nothing

### Requirement: CSRF Protection

Cookie/session-authenticated state-changing requests from the shell (posting comments,
reacting, changing sharing/TTL) MUST be CSRF-protected via token and/or SameSite strategy.
Token-authenticated API/MCP requests are exempt (no ambient credentials).

#### Scenario: Cross-site form post

- **WHEN** a session-authenticated state-changing request arrives without a valid CSRF
  token
- **THEN** the server MUST reject it and make no change

### Requirement: Redirect & SSRF Validation

Post-login and post-share redirect targets MUST be validated against an allow-list of
internal paths; no user-supplied absolute URL is honored for redirects. Any server-side
fetch of a user-supplied URL MUST be guarded against SSRF.

#### Scenario: Open-redirect attempt

- **WHEN** a request supplies an external redirect target (e.g. `?next=https://evil`)
- **THEN** the server MUST ignore it and redirect only to a safe internal path

## Accessibility Requirements

This capability renders user-facing UI. WCAG 2.1 AA is the minimum target.

### Requirement: WCAG 2.1 AA & Semantics

All shell UI MUST meet WCAG 2.1 AA. The page MUST use ARIA landmarks (banner for the
header, navigation for the Bin, main for the body, contentinfo where applicable). Text and
controls MUST meet AA contrast against the `#0A0B0D` canvas, and information (type badges,
provenance, reaction/comment counts) MUST NOT be conveyed by color alone.

#### Scenario: Color-only state

- **WHEN** a state or category is shown with color (e.g. a type badge)
- **THEN** an equivalent text/shape cue MUST also be present

### Requirement: Icon-Only Controls

Every icon-only control in the shell — the URL copy action, the `◆ mcp` affordance, the
Share button, the panel toggle, and the reaction `＋` picker — MUST expose an `aria-label`
describing its action.

#### Scenario: Icon button labelling

- **WHEN** a shell control has no visible text label
- **THEN** it MUST expose an `aria-label` describing what it does

### Requirement: Dynamic Content Regions

Live/HTMX-swapped regions (posted comments, appended Bin pages, and the live webhook/
trajectory streams that mount in the body slot) MUST update inside an `aria-live` region
(`polite` for normal arrivals, `assertive` for critical), so assistive tech announces
changes without stealing focus.

#### Scenario: New streamed item

- **WHEN** a new comment or a streamed live item lands in the view
- **THEN** it MUST be announced via an `aria-live` region without moving focus

### Requirement: Keyboard Navigation & Focus Management

All interactive shell elements MUST be keyboard-operable with a logical tab order and a
visible focus ring that reads on `#0A0B0D` (Enter/Space activate; Escape dismisses
popovers/dialogs). The Share dialog MUST trap focus while open and restore focus to the
Share button on close; the panel toggle MUST expose `aria-expanded`.

#### Scenario: Share dialog focus trap

- **WHEN** a keyboard user opens the Share dialog
- **THEN** focus MUST move into the dialog, cycle within it, and return to the Share
  button when the dialog closes

## Web Routes

The shell is served by the ADR-0011 web handlers (distinct from the SPEC-0002 `/v1`
REST/JSON API, which the shell and CLI both project). Auth-by-default: every route is
`Auth: Required` unless explicitly marked `Public` with a justification.

| Method | Path | Purpose | Auth |
|--------|------|---------|------|
| GET | `/{id}` | Render the artifact shell (header · panel · registry-selected body) | Public — link-based capability read (ADR-0007); unknown/unauthorized/expired ids return a uniform 404 |
| GET | `/run/{id}` | Render the trajectory shell (`/run/` sub-path per SPEC-0002) | Public — same link-capability justification |
| GET | `/bin` (and `/`) | Render the Bin listing (keyset paginated) | Required — workspace-scoped listing |
| GET | `/{id}/share` | Render the Share dialog partial | Required — only the owner may open sharing controls |
| GET | `/{id}/comments` | Load-more comments partial (HTMX) | Public — annotation read is granted by the same capability link as the artifact |

State-changing actions the shell triggers — posting a comment/reaction, submitting a
sharing/TTL change — are handled by the core operations exposed in SPEC-0002 and SPEC-0006
(e.g. `POST /v1/artifacts/{id}/share`); the shell renders their affordances but does not
re-implement their rules.
