# Changelog

All notable changes to Cairn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches 1.0.

## [Unreleased]

## [0.0.4] - 2026-07-15

### Fixed

- **MCP clients using a personal access token got `401` at `initialize`** — the
  MCP bearer verifier only accepted OAuth access tokens (`cairn_at_`), so a
  `cairn_pat_` token (the credential Settings creates *for agents*, which
  connect over MCP) was rejected. `/mcp` now accepts personal access tokens with
  the same scope enforcement as OAuth; set a client's `CAIRN_API_TOKEN` to a
  `cairn_pat_…` value and connect, no OAuth browser flow needed. (#99, closes
  upstream #51)

## [0.0.3] - 2026-07-14

The complete build: every share type, the `cairn` CLI, a Settings surface with
agent tokens and MCP session tracking, the webhook inspector, owner policy, and
retention. Every SPEC (0001–0009) is now implemented.

### Added

- **Code viewer** — server-side syntax highlighting (chroma, embedded/CSP-safe),
  line numbers, a jump-to-symbol outline, and `code_line`/`code_range`/
  `text_selection` line-level comments and reactions. (#68)
- **Image viewer** — display with a bespoke SVG **pin overlay**: drop a pin to
  anchor a region comment (normalized `0..1` coords that survive scaling) plus
  a react-below affordance; degrades gracefully if the pin script fails. (#71)
- **Webhook inspector** (`HK`) — the requestbin share type: an endpoint model
  with ring-buffer retention (#83), a hardened open ingress at `/h/{id}` (inert
  capture, header redaction, body caps, per-endpoint rate limiting) (#84), live
  SSE + read-only MCP fan-out (#85), and a live inspector viewer with
  reactions-only annotation (#86).
- **Settings page** (`/settings`, absorbs `/connect`) — create/list/revoke
  **personal access tokens** for agents (secret shown once), the MCP connection
  info restyled, a CLI section, and account/sign-out. (#74, #75)
- **MCP agent sessions** — each MCP connection is recorded (client name/version,
  activity counters); a Settings "Agent sessions" panel shows which agent is
  doing what, with revoke-to-disconnect. (#76)
- **MCP create tools** — `run_create`, `run_append_spans`, and `bundle_create`,
  so agents can create trajectory runs and bundles over MCP (not just read
  them), through the same core services as `/v1`. (#65)
- **Owner policy** — TTL countdown, extend/shorten expiry, and **id rotation**
  (old link 404s, annotations follow the new id); owner + `sharing:manage`
  only. (#94)
- **Retention reaper** — a background worker that deletes past-TTL artifacts and
  reference-counts + garbage-collects orphaned content-addressed blobs (a
  dedup-shared blob survives until its last referrer expires), with a
  staging-prefix object-storage backstop. (#93)
- **The `cairn` CLI** (v0.0.3, issues #20–#22): a single static Go binary and
  pure `/v1` REST client (ADR-0003).
  - `cat file | cairn` / `cairn file` — pipe or path ingest, with a spinner,
    extension/content media-type detection, best-effort clipboard copy
    (OSC52/pbcopy/xclip/xsel/wl-copy fallback chain), and a decorated
    `✓ pushed` / link / expiry+access summary on an interactive terminal
    (bare link only when piped). (#22)
  - `cairn add f1 f2 …` — one atomic bundle via multipart `POST
    /v1/artifacts`, with a bounded concurrent worker pool preparing files and
    a Bubble Tea + Lip Gloss per-file progress display (done/in-flight/
    queued rows) on an interactive terminal. (#22)
  - `--ttl` / `--title` flags; the server now accepts an optional, capped
    `X-Cairn-Ttl-Seconds` request on create (SPEC-0008 `--ttl`) alongside the
    existing `--title`. (#22)
  - `cairn login` / `logout` / `whoami` against the ADR-0004 bearer-token
    seam, with secure OS-keyring-or-`0600`-file credential storage. (#20, #21)

### Fixed

- **MCP clients could not connect** — the OAuth server now accepts the MCP
  server's canonical `/mcp` URI as the RFC 8707 resource indicator (and serves
  the RFC 9728 path-specific metadata), so clients like Crush no longer get
  `invalid_target`. (#62)
- **OAuth consent Approve did nothing in Safari** — the consent page's CSP
  `form-action` now includes the client's validated redirect origin, so the
  approval redirect to a CLI/localhost callback is allowed. (#63, #64)
- **Reactions now render on click** — the reaction pill + count appears
  immediately (a duplicate picker click-handler was removed), across trajectory
  spans, markdown blocks, and bundle members. (#66, #72)

## [0.0.2] - 2026-07-12

The agent-usable milestone: Cairn now speaks MCP, logs humans in passwordlessly
via Pocket ID, and renders Markdown, bundles, and richer trajectory views.

### Added

- **MCP server surface** (`POST/GET /mcp`) — read, create, comment, and react
  tools plus a trajectory-stream resource, in-process over the core services.
  OAuth-only, per-tool scope enforcement, provenance stamped `via MCP`. (#44)
- **OAuth 2.1 authorization server** — authorization-code grant with mandatory
  S256 PKCE, RFC 8414 discovery, RFC 7591 dynamic client registration, RFC 7009
  revocation, exactly three scopes (`artifacts:read`, `artifacts:write`,
  `annotations:write`). (#42, #43)
- **Native OIDC login** — Cairn is a Pocket ID relying party; humans sign in
  passwordlessly (`/auth/login` → code+PKCE → `/auth/callback`). oauth2-proxy is
  no longer required in front of the app. (ADR-0013, #56)
- **OAuth consent UI** — three-scope approval screen with WCAG landmarks. (#45)
- **Markdown viewer** — sanitized render, table of contents, block and
  text-selection annotation anchors. (#18)
- **Bundle viewer** — file rail, per-file Markdown panes, per-file comments. (#19)
- **Share dialog** — header Share button with link/MCP copy and policy summary. (#46)
- **Home is the Bin** — `/` shows the caller's artifact listing when signed in
  and an on-brand landing when logged out; a `/connect` view documents MCP
  setup. (#57)
- **Inline span comments** — the trajectory viewer's per-span comment affordance
  opens a focus-trapped composer at the clicked line. (#58)

### Changed

- Web shell hardened against the Alpine CSP build: all directive expressions use
  bare identifiers (call syntax silently no-ops under the CSP build). A
  regression test guards it. (#41)
- Assorted web polish: htmx indicator CSP noise removed, panel class leak fixed,
  live-view refinements. (#47)

### Security

- Real bearer authentication with audience-bound short access tokens and
  rotating refresh (reuse detection revokes the family); tokens hashed at rest. (#14, #43)
- An OIDC web session grants nothing on `/v1` or `/mcp` — those remain
  OAuth/bearer-only (regression-tested). (#56)

## [0.0.1] - 2026-07-12

The MVP: trajectory shares end to end on a composable share-type SDK.

### Added

- **Composable share-type SDK** — a registry with optional capability
  interfaces (URL prefix, body viewer, metadata panel, locator schema, route
  mounter); behavior is data, not `switch` statements. (#3)
- **Trajectory shares** — an OpenTelemetry-inspired span/run model with
  content-addressed span-output spill, batch and incremental ingestion
  (`/v1/runs*`), and live SSE span streaming with replay-then-tail resume.
  (#7, #8, #9)
- **Trajectory viewer** — bespoke SVG span waterfall, activity stream,
  cross-highlight, RUN panel, and keyboard/focus accessibility. (#15)
- **Annotations** — polymorphic anchors with canonical keys, idempotent
  reactions, threaded comments, and separate count rollups. (#6)
- **Web app shell** — unified header/body/panel chrome, registry-resolved body
  slot, the Bin listing, generic-file viewer, CSRF protection. (#10, #12)
- **Minimal web session auth and login.** (#11)

[Unreleased]: https://github.com/joestump/cairn/compare/v0.0.4...HEAD
[0.0.4]: https://github.com/joestump/cairn/compare/v0.0.3...v0.0.4
[0.0.3]: https://github.com/joestump/cairn/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/joestump/cairn/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/joestump/cairn/releases/tag/v0.0.1
