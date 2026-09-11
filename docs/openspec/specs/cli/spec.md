---
status: draft
date: 2026-07-08
implements: [ADR-0003]
requires: [SPEC-0007]
---

# SPEC-0008: The `cairn` Command-Line Interface

## Overview

The `cairn` CLI is the human's terminal surface for Cairn. It is *pbcopy for
cairn*: pipe or pass content and files in, get a shareable, agent-native link back —
printed to stdout and copied to the clipboard. It also pushes many files at once as a
**bundle** (`cairn add`) and browses the Bin as a keyboard-driven TUI (`cairn ls`).

This capability realizes **ADR-0003** (triple-surface parity): the CLI is a
first-class **REST client** of the core `/v1` API (ADR-0012). It carries **no domain
logic** — expiry, access policy, provenance capture, and anchor legality live only in
the core service — and it authenticates with the **same OAuth 2.1 authorization-code +
PKCE** flow the agent uses (ADR-0004), formalized by **SPEC-0007**. The only surface
difference the CLI is permitted is *ergonomics* (a keymap, a progress bar, clipboard
integration); its *semantics* are the core's.

The CLI is a single static Go binary distinct from the server binary; its TUI is built
on Bubble Tea. It reads the API base URL and credentials from configuration and the
environment, and it consumes the API's structured error envelope (ADR-0012) so that
both humans and scripts can branch on failures deterministically.

This spec formalizes the CLI's command surface, output formats (human default and a
`--json` mode), exit-code taxonomy, TTY/piping/clipboard behavior, authentication, and
its error-handling and concurrency guarantees.

### Commands → core operations (auth-by-default)

The CLI exposes **no HTTP endpoints of its own**; it consumes the core API. Every
request it makes is authenticated except the OAuth bootstrap, which is public by
necessity because it is the flow that *establishes* authentication.

| Command | Core operation | REST endpoint(s) (ADR-0012) | Auth |
|---------|----------------|-----------------------------|------|
| `cairn` (stdin pipe or path args) | create artifact | `POST /v1/artifacts` | Required |
| `cairn add f1 f2 …` | create bundle | `POST /v1/artifacts` (bundle) | Required |
| `cairn ls` | list the Bin | `GET /v1/bin` (keyset) | Required |
| `cairn ls` → `enter` (open) | fetch artifact | `GET /v1/artifacts/{id}` | Required |
| `cairn ls` → `s` (share) | set/adjust link access | `POST /v1/artifacts/{id}/share` | Required |
| `cairn whoami` | verify identity/session | token introspection / `GET /v1/workspaces/{id}` | Required |
| `cairn logout` | revoke this grant | RFC 7009 revocation (SPEC-0007) | Required |
| `cairn login` | OAuth authorize + token | `/v1/oauth/*`, `/v1/authorize`, `/v1/token` (SPEC-0007) | **Public** — this flow bootstraps authentication; PKCE is mandatory and the code is bound to a loopback redirect, so no ambient credential is required or accepted |

## Requirements

### Requirement: Command Surface and REST-Client Boundary

The CLI MUST expose exactly the commands `cairn` (bare pipe/path ingest), `cairn add`,
`cairn ls`, `cairn login`, `cairn logout`, and `cairn whoami`, and each MUST map onto a
core operation reachable through the `/v1` REST API. The CLI MUST NOT implement or
enforce domain rules (expiry, access-policy decisions, provenance capture, anchor
legality); it SHALL treat the server's response as authoritative. The CLI MUST send the
`/v1` versioned path prefix and MUST NOT depend on any endpoint outside the documented
contract.

#### Scenario: CLI defers a rule to the core

- **WHEN** the user pipes content that the server rejects for an access or expiry
  policy reason
- **THEN** the CLI MUST surface the server's decision unaltered and MUST NOT
  substitute its own policy judgment or retry with different policy

#### Scenario: Every mutating command carries auth

- **WHEN** any command other than `cairn login` issues a request
- **THEN** the CLI MUST attach the stored OAuth bearer token and MUST NOT fall back to
  an unauthenticated request

### Requirement: Pipe and Path Ingest (`cairn`)

Invoked with content on **stdin** (e.g. `cat file | cairn`) or with one or more file
**path arguments**, the bare `cairn` command MUST create a single artifact via
`POST /v1/artifacts`, streaming the body to the server rather than buffering the whole
payload in memory where feasible. On success it MUST print the `cairn.sh/<id>` link to
**stdout** and, when a clipboard is available and stdout is a TTY, copy that link to the
clipboard. The CLI MUST let the server assign the share type, provenance channel
(`via CLI`), access policy, and expiry, and it MUST surface the returned link, expiry,
and access policy to the user.

A repeatable `--tag` flag MUST attach tags to the created artifact (SPEC-0002 REQ
"Artifact Tags"). Each flag holds one tag or a comma-separated list. Locally the CLI
rejects only an empty tag, as a usage error reported before any network call. The tag
charset, size and count bounds, and deduplication, are the server's to decide.

#### Scenario: Tag a piped artifact

- **WHEN** the user runs `cat prompt.md | cairn --tag handoff --tag lane:auto,size:m`
- **THEN** the CLI MUST create one artifact carrying `handoff`, `lane:auto`, and
  `size:m`

#### Scenario: Empty tag flag

- **WHEN** the user passes `--tag ""` or `--tag handoff,,lane:s`
- **THEN** the CLI MUST exit with a usage error without contacting the server

#### Scenario: Pipe content in

- **WHEN** the user runs `cat notes.md | cairn`
- **THEN** the CLI MUST create one artifact, print its `cairn.sh/<id>` link to stdout,
  and exit 0

#### Scenario: Pass a path argument

- **WHEN** the user runs `cairn report.pdf`
- **THEN** the CLI MUST read the file, create one artifact for it, and return the link

#### Scenario: Empty input

- **WHEN** stdin is empty and no path arguments are given
- **THEN** the CLI MUST NOT create an empty artifact and MUST exit with a usage error

### Requirement: Bundle Creation (`cairn add`)

`cairn add f1 f2 f3 …` MUST create a **single bundle artifact** containing the named
files (mixed media permitted) via `POST /v1/artifacts`. During upload it SHOULD display
**per-file progress** on stderr when stderr is a TTY, and on success it MUST print a
**summary line** of the form `N files · <total size> · ⧗ expires <ttl> · 🔒 <access>`
followed by the `cairn.sh/<id>` link. The CLI MUST NOT report success or emit a link
unless the server confirms the **complete** bundle was created; a failure of any
constituent file MUST abort the bundle so no partial bundle is shared. `cairn add`
accepts the same repeatable `--tag` flag as the bare command, applying the tags to the
bundle as a whole.

#### Scenario: Push several files as a bundle

- **WHEN** the user runs `cairn add a.png b.log c.sql`
- **THEN** the CLI MUST create one bundle, show per-file progress, print the summary
  line and the `cairn.sh/<id>` link, and exit 0

#### Scenario: One file in the bundle fails

- **WHEN** the server rejects one file mid-bundle (e.g. oversize)
- **THEN** the CLI MUST abort the bundle, MUST NOT print a success link, and MUST exit
  non-zero with the mapped error

### Requirement: The Bin TUI (`cairn ls`)

`cairn ls` MUST render the Bin as a keyboard-driven Bubble Tea TUI backed by
`GET /v1/bin` using the API's keyset (cursor) pagination (ADR-0012), fetching further
pages as the user scrolls. It MUST support the keymap `↑/k` up, `↓/j` down, `/` filter,
`enter` open, `s` share, and `q` quit. Opening an item MUST fetch it via
`GET /v1/artifacts/{id}`; `s` MUST invoke `POST /v1/artifacts/{id}/share`. The TUI MUST
require an interactive terminal (see TTY behavior below).

#### Scenario: Browse and open

- **WHEN** the user runs `cairn ls`, moves with `j`/`k`, and presses `enter`
- **THEN** the CLI MUST fetch the highlighted artifact and present/open it

#### Scenario: Filter the listing

- **WHEN** the user presses `/` and types a query
- **THEN** the TUI MUST filter the visible rows and MUST continue to paginate the
  underlying keyset cursor without skipping or duplicating rows

#### Scenario: Quit

- **WHEN** the user presses `q`
- **THEN** the TUI MUST exit cleanly with exit code 0 and restore the terminal state

### Requirement: Authentication and Session Lifecycle

`cairn login` MUST perform the **same OAuth 2.1 authorization-code + PKCE** flow the
agent uses (ADR-0004, SPEC-0007), as a public client using a **loopback redirect** with
PKCE mandatory, hitting the same `/authorize` and `/token` endpoints. On success the CLI
MUST display `✓ authorized as <email> · via MCP OAuth`. `cairn whoami` MUST report the
authenticated identity and MUST exit non-zero when there is no valid session.
`cairn logout` MUST revoke the current grant (RFC 7009) and delete locally stored
credentials. The CLI MUST request only the granted scopes (`artifacts:read`,
`artifacts:write`, `annotations:write`) and MUST NOT request delete or sharing-management
scopes.

#### Scenario: First login

- **WHEN** the user runs `cairn login`
- **THEN** the CLI MUST open the browser to the authorization endpoint with a PKCE
  challenge and a loopback redirect, exchange the returned code for tokens, store them,
  and print `✓ authorized as <email> · via MCP OAuth`

#### Scenario: whoami with no session

- **WHEN** the user runs `cairn whoami` with no stored credentials
- **THEN** the CLI MUST print that no session exists and exit with the
  not-authenticated exit code

#### Scenario: Logout revokes the grant

- **WHEN** the user runs `cairn logout`
- **THEN** the CLI MUST call the revocation endpoint for this grant and delete the local
  token material, leaving other grants (agent, other machines) untouched

### Requirement: Secure Credential Storage

Access and refresh tokens MUST be stored securely at rest: the CLI MUST prefer the
operating-system secret store (macOS Keychain, the Secret Service / libsecret on Linux,
or an equivalent) and, when none is available, MUST fall back to a file created with
owner-only permissions (`0600`) under the user's config directory. Tokens MUST NOT be
written to logs, printed to stdout/stderr, or embedded in error messages, and the CLI
MUST redact them in any verbose/debug output.

#### Scenario: Keychain-backed storage

- **WHEN** the CLI stores tokens on a system with an OS secret store
- **THEN** it MUST use that store and MUST NOT also write the tokens to a plaintext file

#### Scenario: File fallback permissions

- **WHEN** no OS secret store is available and the CLI writes tokens to a file
- **THEN** that file MUST be created with `0600` permissions and MUST NOT be
  world-readable

#### Scenario: Tokens never leak into output

- **WHEN** the CLI emits verbose or debug output while authenticated
- **THEN** any bearer or refresh token MUST be redacted

### Requirement: Silent Token Refresh and Auth-Expiry Handling

When the short-lived access token is expired or the server returns `401`
(`unauthorized`), the CLI MUST attempt a single **transparent** refresh using the
rotating refresh token and retry the original request once. If refresh fails (refresh
token expired or revoked), the CLI MUST stop, MUST NOT loop, and MUST instruct the user
to run `cairn login`, exiting with the not-authenticated exit code. Refresh-token
rotation MUST be honored: the CLI MUST persist the newly issued refresh token and
discard the old one.

#### Scenario: Expired access token, valid refresh

- **WHEN** a request returns `401` and a valid refresh token is present
- **THEN** the CLI MUST refresh once, persist the rotated refresh token, retry the
  request once, and succeed transparently

#### Scenario: Refresh token revoked

- **WHEN** the refresh attempt is itself rejected
- **THEN** the CLI MUST NOT retry further, MUST tell the user to run `cairn login`, and
  MUST exit with the not-authenticated exit code

### Requirement: Machine-Readable Error Mapping and Exit Codes

The CLI MUST parse the API's structured error envelope
(`{"error":{"code","message","details","request_id"}}`, ADR-0012) and branch on the
stable machine `code`, never on prose. It MUST map each `code`, plus transport-level
failures that have no HTTP status, onto a stable exit-code taxonomy, and MUST print the
server's `message` (and `request_id` when present) to stderr. Exit codes MUST be:

| Exit | Meaning | Source `code` / condition |
|------|---------|---------------------------|
| `0` | success | 2xx |
| `1` | unexpected/internal error | `internal`, unclassified |
| `2` | usage error (bad flags/args, empty input) | client-side, `validation_failed` |
| `3` | not authenticated / auth expired and refresh failed | `unauthorized` |
| `4` | forbidden | `forbidden` |
| `5` | not found or expired | `not_found` |
| `6` | payload too large / oversize | `payload_too_large` |
| `7` | rate limited | `rate_limited` |
| `8` | network / server unreachable | transport error (DNS/TLS/connection) |
| `9` | conflict | `conflict` |
| `130` | interrupted (SIGINT) | Ctrl-C during operation |

#### Scenario: Not-found maps to exit 5

- **WHEN** a request returns the `not_found` error code
- **THEN** the CLI MUST print the server message to stderr and exit 5

#### Scenario: Rate limited surfaces Retry-After

- **WHEN** a request returns `rate_limited` (429) with a `Retry-After`
- **THEN** the CLI MUST report the retry hint to the user and exit 7 without silently
  hammering the endpoint

#### Scenario: Unreachable server is distinct from an HTTP error

- **WHEN** the server cannot be reached (DNS failure, TLS failure, connection refused)
- **THEN** the CLI MUST exit 8 and MUST NOT report it as an application-level error code

### Requirement: Output Formats and `--json` Mode

By default the CLI MUST emit human-readable output to stdout (links, summary lines,
status glyphs) in the terminal/dev-minimal style. When `--json` is passed, the CLI MUST
emit a single machine-readable JSON object to stdout for successful commands (e.g. the
created artifact's id, url, size, expiry, and access), and on failure MUST emit the
API's error envelope (or an equivalent transport-error object) as JSON to **stderr**
while still returning the mapped non-zero exit code. In `--json` mode the CLI MUST NOT
interleave progress bars, spinners, or decorative output into stdout.

#### Scenario: JSON success payload

- **WHEN** the user runs `cat f | cairn --json`
- **THEN** stdout MUST contain a single valid JSON object with at least the artifact
  `id` and `url`, and MUST contain no non-JSON decoration

#### Scenario: JSON error payload

- **WHEN** a command fails in `--json` mode
- **THEN** the error envelope MUST be written as JSON to stderr and stdout MUST NOT
  contain a partial success object

### Requirement: TTY, Piping, and Clipboard Behavior

The CLI MUST detect whether stdin, stdout, and stderr are TTYs and adapt: progress bars,
spinners, and the `cairn ls` TUI MUST require an interactive terminal, and the CLI MUST
NOT emit terminal control sequences to a non-TTY stream. When stdout is not a TTY (piped
or redirected), the CLI MUST still print the bare link so it composes in shell
pipelines, and MUST suppress clipboard copying. Clipboard copy MUST be best-effort: when
no clipboard is available (e.g. headless/SSH) the CLI MUST skip copying, MUST NOT fail
the command, and SHOULD note that copying was skipped. A `--no-copy` flag MUST disable
clipboard copying explicitly.

#### Scenario: Piped into another command

- **WHEN** the user runs `cairn f | pbpaste-consumer` (stdout not a TTY)
- **THEN** the CLI MUST print only the bare link to stdout, MUST NOT copy to the
  clipboard, and MUST NOT emit progress control sequences

#### Scenario: `cairn ls` without a terminal

- **WHEN** `cairn ls` is run with a non-interactive stdout
- **THEN** the CLI MUST NOT launch the TUI and MUST either emit the listing as
  `--json`/plain text or exit with a usage error explaining a TTY is required

#### Scenario: No clipboard available

- **WHEN** the CLI cannot reach a clipboard on an interactive session
- **THEN** it MUST still print the link, skip the copy, and exit 0

### Requirement: Oversize and Duplicate File Handling

When the server rejects an upload as too large (`payload_too_large`, HTTP 413) the CLI
MUST report which file exceeded the limit and exit 6 without leaving a partial artifact
reported as successful; where the server advertises a size limit, the CLI SHOULD check
obvious oversize files locally before uploading to fail fast. When `cairn add` is given
the **same path more than once**, the CLI MUST detect the duplicate argument and MUST
NOT upload it twice, either de-duplicating or refusing with a usage error. The CLI MUST
NOT depend on content-hash de-duplication behavior of the store — that is the core's
concern (ADR-0008).

#### Scenario: Oversize file

- **WHEN** a file exceeds the server's size limit
- **THEN** the CLI MUST name the offending file, exit 6, and report no successful link

#### Scenario: Same path passed twice

- **WHEN** the user runs `cairn add a.log a.log`
- **THEN** the CLI MUST NOT upload `a.log` twice and MUST either de-duplicate it or
  reject the invocation with a usage error

### Requirement: Configuration and Server Endpoint Resolution

The CLI MUST resolve the API base URL and other configuration from an explicit flag,
then an environment variable (e.g. `CAIRN_API`), then a config file, then a built-in
default, in that precedence order. It MUST validate that the base URL is well-formed and
uses HTTPS (except an explicit localhost override for development) before making
requests, and MUST send requests only to the configured host. Configuration errors MUST
be reported clearly and exit with the usage-error code before any network call.

#### Scenario: Endpoint override via environment

- **WHEN** `CAIRN_API=https://cairn.example.com` is set and no flag is given
- **THEN** the CLI MUST direct all `/v1` requests to that host

#### Scenario: Malformed base URL

- **WHEN** the configured base URL is not a valid HTTPS URL (and is not an allowed
  localhost dev override)
- **THEN** the CLI MUST exit with the usage-error code before making any request

### Requirement: Error Handling Standards

The CLI's Go implementation MUST wrap errors with context at each layer boundary
(filesystem read, HTTP transport, error-envelope decode) using `%w` so callers can
inspect the chain, and MUST define **sentinel errors** for the domain failures the CLI
branches on (e.g. `ErrNotAuthenticated`, `ErrExpired`, `ErrOversize`, `ErrConflict`).
It MUST NOT silently swallow errors: every failure MUST either be handled explicitly or
surfaced to the user with a mapped exit code. Diagnostic logging under `--verbose`/
`--debug` MUST be structured (key-value), MUST include the server `request_id` when
present to aid correlation, and MUST redact secrets.

#### Scenario: Error context is preserved to the boundary

- **WHEN** a file read fails deep in an upload path
- **THEN** the surfaced error MUST retain enough wrapped context to identify the file
  and the operation, and MUST NOT be reported as a generic unlabeled failure

#### Scenario: Sentinel error drives control flow

- **WHEN** the API returns `unauthorized`
- **THEN** the CLI MUST map it to its `ErrNotAuthenticated` sentinel and take the
  refresh/login path rather than string-matching the message

#### Scenario: request_id aids correlation

- **WHEN** a command fails with a server error that carries a `request_id`
- **THEN** verbose output MUST include that `request_id`

### Requirement: Concurrency Safety

`cairn add` MUST upload multiple files using a **bounded** worker pool (a configurable
concurrency limit with a sane default), and every concurrent upload MUST receive a
`context.Context` for cancellation and timeout that is propagated to the HTTP client.
On `SIGINT` (Ctrl-C) or a fatal error the CLI MUST cancel that context, abort in-flight
uploads promptly, perform a **graceful shutdown** (no orphaned goroutines or leaked
connections), and exit 130 on interrupt. Shared state — per-file progress and the
aggregate summary — MUST be updated race-free, and the build MUST pass Go's race
detector in CI.

#### Scenario: Ctrl-C mid-bundle

- **WHEN** the user presses Ctrl-C during a multi-file upload
- **THEN** the CLI MUST cancel the shared context, stop in-flight uploads, not report a
  successful bundle, and exit 130

#### Scenario: Bounded concurrency

- **WHEN** `cairn add` is given more files than the concurrency limit
- **THEN** no more than the configured number of uploads MUST be in flight at once

#### Scenario: Race-free progress accounting

- **WHEN** many files upload concurrently and update progress
- **THEN** the aggregate byte/file counts MUST remain consistent and MUST pass the race
  detector
