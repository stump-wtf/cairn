# Changelog

All notable changes to Cairn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches 1.0.

## [Unreleased]

### Changed

- **Outbound webhooks**: the `artifact.created` body appends two keys to `data`:
  `actor_kind` (`human` or `agent`) and `auth` (`session`, `oauth`, `pat` or
  `api_token`), both derived from the creator's credential. No other byte
  changes. The encoder now handles every SPEC-0016 event kind; kinds other than
  `artifact.created` are counted and never sent to `CAIRN_OUTBOUND_WEBHOOK_URLS`.
  (ADR-0022, SPEC-0016, #305)
- **Traces announce themselves.** Opening a run, or uploading one whole, now
  emits `artifact.created` with `share_type: "trajectory"`, like every other
  artifact, so `CAIRN_OUTBOUND_WEBHOOK_URLS` targets start receiving trace
  creations. A consumer that routes on `share_type` or tags needs no change;
  one that assumed every event was a single-body artifact or a bundle should
  ignore `trajectory`. Closing a run (`POST /v1/runs/{id}/close`), and a batch
  run born closed, also emit `run.closed` with its status, span count, start,
  end and duration; like the other new kinds it is counted and not sent to env
  targets until owned subscriptions land. (ADR-0022, SPEC-0016 EV-2, EV-3,
  SPEC-0023, #313)
- **Annotations announce themselves.** A new comment emits `comment.created`,
  a new reaction `reaction.added` (a duplicate click emits nothing), and each
  removed reaction `reaction.removed`, whichever surface made the change.
  Reaction events carry `approval_class` (the emoji is in the approval class)
  and `approval` (in the class **and** stored by a browser session). An
  agent's 👍 is stored and announced, but never as an approval, and no
  request field can change that. The class is set by the new
  `CAIRN_APPROVAL_REACTIONS` (comma-separated, default 👍 ✅ ✔️; skin tones
  and VS-16 are ignored when matching), and a malformed entry fails startup.
  Like `run.closed`, these kinds are counted and not sent to env targets
  until owned subscriptions land. (ADR-0022, SPEC-0016 EV-2, EV-5, #311)
- **Reactions and comments are owned per actor kind.** Each row stores the
  server-derived `actor_kind` (`human` for a browser session, `agent` for every
  bearer credential), and reaction idempotency is keyed per
  `(actor_id, actor_kind)`. An agent reacting with your credentials no longer
  shares, occupies or can withdraw your own reaction: un-react, delete-by-id
  (403 on the other kind's row) and comment edit/delete all match the kind.
  Reactions gain `on_behalf_of`, set exactly as on comments (the REST body
  field, or the MCP client's name). Reaction and comment responses carry
  `actor_kind`; reaction tallies add `human_count` and `agent_count`, and
  `reacted` now means "a row you, as this kind, can remove". Rows written
  before the upgrade read back `actor_kind: ""`, are never counted as human,
  and only their own actor removes them. Migration 0022 builds the new unique
  index concurrently and runs outside a transaction; it is safe to rerun.
  `GET /v1/artifacts/{id}/reactions?include=reactors` also returns the rows
  behind each tally, each with `actor_id`, `actor_kind` and `on_behalf_of`.
  (ADR-0022, SPEC-0016 EV-6, #159)

  **Upgrade note: a binary rollback breaks reactions.** Migration 0022 drops
  the old five-column reaction key, and a binary from before this change
  upserts on exactly that key. Once 0022 has applied, an older cairnd returns
  500 on every reaction write (Postgres 42P10, no matching unique constraint)
  until you roll forward again; the same holds for an old process still
  serving while the new one migrates. The old key cannot be recreated once a
  human and an agent row share an actor. Roll forward, not back.

## [0.1.1] - Unreleased

Changes since `v0.1.0`, staged for the next patch release.

### Added

- **GitHub OAuth login** — a provider interface for web authentication with a
  GitHub provider as the first implementation, and login-method provenance
  recorded on sessions. (ADR-0017, #259)

### Fixed

- CI checks out the PR head SHA rather than the branch ref, so required checks
  gate the exact commit under review. (#245)
- Self-hosting guide: corrected env secrets and the `CAIRN_API_TOKENS` compose
  passthrough. (#239)
- Docs build: unlinked two repository-host references that broke the build, and
  renumbered the embedded-docs spec off the SPEC-0013 collision. (#258, #257)

### Added (records)

- ADR-0020 + SPEC-0013 — single-binary runtime with embedded docs; ADR-0021 +
  SPEC-0014 — Prometheus metrics led by storage and expiry. (#251, #254, #255)

## [0.1.0] - 2026-09-12

First release after the module paths moved to their public homes, and the
first cut under the MIT license. The headline: Cairn talks to the rest of the
machine — outbound webhooks on artifact creation, client-asserted tags for
handoff routing, and a wave of A2UI work that renders traces, bundles, and
markdown straight into an agent's UI.

### Added

- **Outbound webhooks on artifact creation** (SPEC-0012) — configured
  destinations receive a signed `artifact.created` event when a share lands,
  the seam Switchboard's todo routing is built on. (#175)
- **Client-asserted artifact tags for handoff routing** (ADR-0018) — creators
  can tag artifacts at create time so downstream routers can lane them. (#181)
- **A2UI everywhere over MCP** — `cairn://artifact/{id}/a2ui` for single-body
  artifacts, trace and bundle views as A2UI resources, a widened and colorized
  flame graph, bundle members served as their own resources with `open_member`
  navigation via `a2ui_action` (and `a2ui_error` for render failures), and
  guidance for agents to prefer `/a2ui` surfaces. (#91, #92, #93, #94, #98,
  #102, #107)
- **Click-to-copy** for markdown code blocks and the whole document. (#134)
- `--help` and usage errors styled with fang in the Cairn palette. (#132)
- Cross-platform `cairn` CLI binaries built and attached to releases, with the
  release destinations split: binaries on Gitea, container image on GitHub.
  (#60, and the release-pipeline commits through `v0.1.0`)
- Homebrew tap publishing for the CLI. (release-pipeline commits)

### Changed

- Module paths moved to their public homes (`github.com/stump-wtf/…`), and the
  Settings template updated to match. (81454e2, a5e7eb9)
- Cairn is now MIT-licensed. (#206)
- Traces, not trajectories — naming settled in ADR-0016 Phase 1. (#86)
- Release archives ship only the binary, and the shipped CLI is gated against
  containing any server code. (215e99f, 5c3ffb6)
- The four racing CI workflows retired in favor of one composed pipeline. (#29)

### Fixed

- **Artifact creation 500ed on every call when webhooks were disabled** —
  `cairnd` now degrades gracefully instead. (#203)
- The CLI's default server resolves; dead install links dropped. (#217)
- MCP: boolean JSON schemas no longer void the tool list (#45); span output is
  plain text (#44); string-encoded spans unwrap via middleware with end-to-end
  regression tests (#108); `bundle_create` publishes concrete array types so
  the tool is callable (#116); the `run_capture` prompt no longer names a
  nonexistent task argument (#115); A2UI resources honor the `?w=` width hint
  (#102).
- Trajectory viewer: unbounded zoom and toggling pickers (#55), pan tracks the
  centre row with gaps collapsing for real (#57), the category bar is a
  composition not a wall-clock fraction (#58), pan reaches the run's ends and
  the timeline box scrubs (#59), bullet reactions on their own line with prompt
  spans as markers (#48).
- The bin's type menu stays reachable while filtering, and overlapping tabs
  became visibility + type lenses. (020245c, f383ad1)
- A loading indicator for bundle member navigation. (7eaf852)
- Nil-guarded storeless handlers so port reuse cannot panic `httpapi`. (0c8007f)

### Security

- `golang.org/x/net` bumped for CVE-2026-46600. (#165)

### Dependencies

- Go 1.27, chi v5.3.2, Lip Gloss v2.0.6, MCP Go SDK v1.7.0, and the usual
  Renovate churn. (#124, #126, #135, #166, #167, #168, #169)

### Docs

- Getting-started guides for hosted Cairn users, aligned Switchboard sections,
  and links to Switchboard's new first-webhook and handoff recipe pages.
  (#186, #187, #188)
- `cairn.sh` was never acquired — the record and the site no longer imply it
  might be. (#219)
- Deployment docs corrected to the files a self-hoster actually reads. (#227)
- The mobile drawer behind the glass navbar unbroken. (#225)
- Three ways a shell check reports the wrong answer, recorded. (#233)

## [0.0.4] - 2026-07-15

### Fixed

- **MCP clients using a personal access token got `401` at `initialize`** — the
  MCP bearer verifier only accepted OAuth access tokens (`cairn_at_`), so a
  `cairn_pat_` token (the credential Settings creates *for agents*, which
  connect over MCP) was rejected. `/mcp` now accepts personal access tokens with
  the same scope enforcement as OAuth; set a client's `CAIRN_TOKEN` to a
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

[Unreleased]: https://github.com/stump-wtf/cairn/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/stump-wtf/cairn/compare/v0.0.4...v0.1.0
[0.0.4]: https://github.com/stump-wtf/cairn/compare/v0.0.3...v0.0.4
[0.0.3]: https://github.com/stump-wtf/cairn/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/stump-wtf/cairn/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/stump-wtf/cairn/releases/tag/v0.0.1
