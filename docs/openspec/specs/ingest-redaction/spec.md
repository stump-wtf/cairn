---
status: accepted
date: 2026-09-22
implements: [ADR-0023]
requires: [SPEC-0002]
related: [SPEC-0004, SPEC-0005, SPEC-0006, SPEC-0008, SPEC-0012, SPEC-0014]
---

# SPEC-0017: Secret Detection and Redaction at Ingest

## Graph Edges

- **Implements:** **ADR-0023**: gitleaks-engine scanning before persist, with a
  per-type reject or mask default.
- **Requires:** **SPEC-0002**: the artifact create and streaming-ingest path the
  scan sits inside.
- **Related:** **SPEC-0004** (trace spans), **SPEC-0005** (webhook captures),
  **SPEC-0006** (comments), **SPEC-0008** (CLI flags and messages), **SPEC-0012**
  (events carry stored text only), **SPEC-0014** (metrics).

## Overview

Every text Cairn stores is scanned for credentials before it is persisted:

- artifact bodies and titles;
- bundle members;
- comments;
- trace run titles and prompts, span names, args and outputs;
- webhook captures.

A hit either **rejects** the write with an actionable error that never contains
the secret, or **masks** the value as `[REDACTED]` and stores the rest. Which
one depends on the content type. The engine is the gitleaks v8 library with
gitleaks' default ruleset plus Harness's credential shapes. See ADR-0023.

Requirement IDs (`RD-n`) are stable and are cited as `SPEC-0017 RD-n`.

## Requirements

### Requirement: RD-1 Scan Before Persist

For every surface in RD-4, the system MUST complete the scan and apply its
outcome before any of these happens:

- a database transaction commits the scanned content;
- a staged blob is promoted to its content-addressed key;
- an event (SPEC-0012, SPEC-0016) is emitted for it;
- a preview or search index reads it.

A rejected write MUST leave no committed row, no promoted blob and no event, and
its staging object MUST be removed.

#### Scenario: Masked body never stored raw

- **WHEN** a markdown artifact containing a detectable token is created
- **THEN** the promoted blob contains `[REDACTED]` in place of the token, and no
  object under the content-addressed prefix contains the token

#### Scenario: Rejected write leaves nothing

- **WHEN** a code artifact containing a detectable token is created with the
  default mode
- **THEN** the response is `validation_failed`, no artifact row exists, the
  staging object is removed, and no `artifact.created` event is emitted

#### Scenario: Event carries only stored text

- **WHEN** a comment containing a token is created
- **THEN** the `comment.created` event body (SPEC-0016) contains `[REDACTED]`,
  not the token

### Requirement: RD-2 Detection Engine and Settings

Detection MUST use the gitleaks v8 `detect` package, configured from an embedded
config with `[extend] useDefault = true`, Cairn's additional rules (RD-3) and
the operator allowlist (RD-8). The detector MUST be configured with:

- `IgnoreGitleaksAllow = true`;
- `MaxTargetMegaBytes = 0`;
- `MaxArchiveDepth = 0`;
- an explicit, non-zero `MaxDecodeDepth`.

The system MUST NOT load a `.gitleaksignore`, a baseline, or any
uploader-supplied configuration. A detector that fails to build at startup MUST
stop cairnd from starting. Cairn MUST NOT run with scanning silently disabled.

#### Scenario: Inline allow comment does not exempt

- **WHEN** a body contains a detectable token on a line that also contains
  `gitleaks:allow`
- **THEN** the token is detected and handled per the surface's mode

#### Scenario: Encoded token is detected

- **WHEN** a body contains a base64 encoding of a detectable token
- **THEN** the token is detected, and in mask mode the encoded segment is masked

#### Scenario: Bad config stops startup

- **WHEN** the operator allowlist file does not parse
- **THEN** cairnd exits with an error naming the file, and serves nothing

### Requirement: RD-3 Harness-Compatible Rules and Mask

The system MUST add custom rules covering the credential shapes Harness's
redaction masks:

- URL userinfo with a password;
- `Authorization` and `Proxy-Authorization` values;
- API-key-style headers;
- secret-named assignments and flags;
- `curl`/`wget` `-u` basic auth.

Each rule MUST mask only the secret value (its `secretGroup`) and keep the label.
The mask MUST be the literal `[REDACTED]`. A shared corpus test MUST assert that
every Harness redaction fixture is detected. Its fixtures MUST be assembled from
split literals, so no contiguous credential exists in the repository.

#### Scenario: Label kept, value masked

- **WHEN** a trace span's args contain an Authorization header with a bearer value
- **THEN** the stored args keep the header name and scheme, and replace only the
  value with `[REDACTED]`

#### Scenario: Harness corpus parity

- **WHEN** the corpus test runs
- **THEN** every fixture Harness masks is also detected by Cairn's config

### Requirement: RD-4 Surfaces and Default Modes

The system SHALL scan these surfaces with these default modes:

| Surface | Scanned fields | Default |
|---|---|---|
| `markdown`, text-sniffed `file` | body, title | mask |
| `code` | body, title | reject |
| `bundle` | each text member, title | reject |
| trace run (batch create, append) | run title, prompt, span name, args, output (inline and spilled) | mask |
| comment (create; edit when it exists) | body | mask |
| webhook capture | captured headers and body | mask |

`CAIRN_REDACTION_REJECT_TYPES` (default `code,bundle`) MAY change which share
types reject. Every other scanned surface masks. A share type added later MUST
default to mask until it is listed.

#### Scenario: Live trace append is masked, not refused

- **WHEN** an agent appends a span whose output contains a token to an open run
- **THEN** the append succeeds, the stored output is masked, and the response
  reports one redaction

#### Scenario: Bundle rejection names the member

- **WHEN** a bundle's third member contains a token under the default mode
- **THEN** the whole bundle is rejected, and the error names that member and the
  rule

#### Scenario: Webhook capture is masked

- **WHEN** an anonymous sender posts a request with a bearer Authorization
  header to a webhook ingress URL
- **THEN** the captured request is stored with the header value masked

### Requirement: RD-5 Writer Downgrade, Never Disable

A writer MAY downgrade a reject-mode surface to mask for a single request:

- `X-Cairn-Redaction: mask` over REST;
- `--redact=mask` in the CLI;
- a `redaction: "mask"` argument on the MCP create tools.

Any other value MUST be `validation_failed`. No request input may disable
scanning or turn a mask-mode surface into an unscanned one.

#### Scenario: Downgrade stores masked code

- **WHEN** a code artifact containing a token is created with
  `X-Cairn-Redaction: mask`
- **THEN** it is stored masked and the response reports the redaction

#### Scenario: Disable attempt refused

- **WHEN** a request sends `X-Cairn-Redaction: off`
- **THEN** the response is `validation_failed`, with field `X-Cairn-Redaction`
  and reason `unknown_value`

### Requirement: RD-6 Binary Content

A body MUST be treated as text when its resolved media type is textual (`text/*`,
`application/json`, `application/xml`, `application/yaml`, `application/toml`,
`application/javascript`, `application/x-sh`, or a `+json` / `+xml` suffix). It
MUST also be treated as text when its first 8 KiB are valid UTF-8 containing no
NUL byte, whatever type was declared. A body whose leading bytes match a known
binary signature MUST NOT be scanned, and MUST be recorded as
`not_scanned_binary`. Archives and images are not unpacked or read.

#### Scenario: Declared image that is text is scanned

- **WHEN** a body declared `image/png` consists of UTF-8 text containing a token
- **THEN** it is scanned and handled per its surface's mode

#### Scenario: Real image is not scanned

- **WHEN** a PNG image is uploaded
- **THEN** it is stored unchanged with status `not_scanned_binary`

### Requirement: RD-7 Scan Size Cap

Each scanned field MUST be bounded by `CAIRN_REDACTION_MAX_SCAN_BYTES` (default
16 MiB). Fields larger than 1 MiB MUST be scanned in overlapping windows (at
least 4 KiB of overlap) read back from staging, never buffered whole. A text
field over the cap MUST be rejected with reason `too_large_to_scan` and the
limit, unless `CAIRN_REDACTION_OVERSIZE=store_unscanned` is set. In that case it
MUST be stored with status `not_scanned_oversize`, which the owner can see.
Gitleaks' own size skip MUST never apply (RD-2).

#### Scenario: Oversize text rejected by default

- **WHEN** a 20 MiB text body is uploaded with the default cap
- **THEN** the response is `validation_failed` with reason `too_large_to_scan`
  and a limit of 16777216 bytes

#### Scenario: Operator opts into storing unscanned

- **WHEN** `CAIRN_REDACTION_OVERSIZE=store_unscanned` and a 20 MiB text body is
  uploaded
- **THEN** it is stored, its status is `not_scanned_oversize`, and the scan
  metric counts it

#### Scenario: Token across a window boundary

- **WHEN** a token straddles the boundary between two scan windows
- **THEN** it is detected exactly once

### Requirement: RD-8 Operator Allowlist, by Value Only

`CAIRN_REDACTION_ALLOWLIST_FILE` MAY name an operator TOML containing:

- gitleaks allowlist `regexes` (matched against the secret);
- `stopwords`;
- rule IDs under `disabledRules`.

Path, member-name and commit allowlists MUST be rejected at startup. The file is
operator configuration and MUST NOT be settable by any user, token or request.

#### Scenario: Fixture value allowlisted

- **WHEN** the allowlist contains an anchored regex matching a known inert
  fixture value
- **THEN** that exact value is not masked or rejected, and a different real
  token in the same body still is

#### Scenario: Path allowlist refused

- **WHEN** the allowlist file contains a `paths` entry
- **THEN** cairnd refuses to start, and names the entry as unsupported

### Requirement: RD-9 Recorded Outcome Without Values

The system MUST record a scan outcome for each scanned artifact, bundle member,
comment and run. The outcome is a status (`clean`, `masked`,
`not_scanned_binary`, `not_scanned_oversize`, or `unscanned` for content stored
before this capability), a redaction count, and counts per rule ID. It MUST NOT store, log or emit:

- the secret;
- any hash or fingerprint of it;
- the matched line.

The artifact read surfaces MUST expose the outcome to the owner. The create
response and MCP tool output MUST include the outcome as `redactions: {count,
rules}` and `redacted: true|false`. The system MUST count outcomes in
`cairn_redactions_total{surface, outcome}`.

#### Scenario: Owner sees a summary

- **WHEN** the owner opens an artifact with two masked GitHub tokens
- **THEN** the artifact metadata reports `masked`, a count of 2, and the rule ID
  twice. The token appears nowhere in the response.

#### Scenario: Logs carry no value

- **WHEN** a rejection is logged
- **THEN** the log line carries the rule ID, surface and request id, and no
  substring of the secret

### Requirement: RD-10 Integrity and Checksums

A client-declared body checksum MUST be verified against the received bytes
before masking. The stored SHA-256 MUST be of the stored, masked bytes. A masked
response MUST set `redacted: true`, so a client that compares hashes can explain
the difference. Deduplication MUST key on the stored bytes.

#### Scenario: Declared checksum of a masked upload

- **WHEN** a client declares the SHA-256 of a markdown body containing a token
- **THEN** the upload is accepted, the response carries the masked body's
  SHA-256 and `redacted: true`, and no mismatch error is raised

### Requirement: RD-11 Actionable Rejection

A rejection MUST use the ADR-0012 envelope with code `validation_failed`, and
the structured violation shape of Cairn SPEC-0019. Each violation carries:

- `field` (for example `body`, `members[3].content`, `spans[12].args`);
- `reason` (`secret_detected` or `too_large_to_scan`);
- `rule` (the rule ID);
- `line` and `column`, where known;
- a human `message` naming the downgrade option.

It MUST NOT include the secret or the source line. The CLI MUST render it as one
line per violation.

#### Scenario: CLI shows how to proceed

- **WHEN** `cairn add patch.diff` is rejected for a token on line 42
- **THEN** the CLI prints the member, the line, the rule ID and a hint to use
  `--redact=mask`, and exits with the usage exit code

### Requirement: Error Handling Standards

- Errors MUST be wrapped with context at each layer boundary.
- Sentinel errors MUST exist for `secret_detected` and `too_large_to_scan`.
- A detector panic or error MUST fail the write closed (reject), never store
  unscanned.
- Structured logging MUST be used, and MUST NOT carry secret material.

#### Scenario: Detector error fails closed

- **WHEN** the detector returns an error for a field
- **THEN** the write is rejected as internal, nothing is stored, and the error is
  logged without the content

### Requirement: Concurrency Safety

- The detector is built once and shared. Implementation MUST prove that
  concurrent `DetectString` calls are race-free under the race detector, or
  guard the detector.
- Windowed scans MUST honour request context cancellation.
- Scan tests MUST run under `make ci`'s race detector.

#### Scenario: Concurrent scans

- **WHEN** 32 concurrent creates each contain a token
- **THEN** each is masked or rejected correctly, and the race detector reports
  nothing

### Requirement: Database Operation Standards

- Scan-outcome columns MUST ship in one migration, defaulting existing rows to
  status `unscanned` (created before this capability).
- Outcome writes MUST commit in the same transaction as the scanned content.
- Queries MUST be parameterized.

#### Scenario: Legacy artifact status

- **WHEN** an artifact created before this migration is read by its owner
- **THEN** its scan status is `unscanned`

## Security Requirements

- **Authentication**: Unchanged. Scanning applies to every authenticated write,
  and to anonymous webhook ingress, equally.
- **Rate limiting**: Unchanged inbound. Scan CPU is bounded per field by RD-7.
- **Security headers**: No new rendered surface. The owner's outcome summary is
  JSON- or template-escaped like other metadata.
- **Request body size limits**: The existing upload caps (SPEC-0002) still apply
  first. RD-7 adds the scan cap.
- **CSRF protection**: Unchanged. `X-Cairn-Redaction` is a request header on
  already-guarded writes.
- **Redirect validation**: Not applicable; no redirects are introduced.
- **Secret handling**: No secret value, hash, fingerprint or matched line may be
  stored, logged, emitted or returned (RD-9, RD-11). The repository's own
  tests MUST build credential fixtures from split literals, so its gitleaks CI
  stage stays green without a suppression.
- **Residual risk**: Secrets in unrecognised shapes, inside archives or images,
  or in binary bodies pass through. Scanning is defence in depth, not a
  guarantee, and the self-hosting guide says so.

## Accessibility Requirements

The owner-visible redaction summary in the viewer:

- **WCAG 2.1 AA** is the target.
- **ARIA landmarks**: the viewer's existing landmarks are unchanged.
- **Icon-only controls**: a redaction badge MUST carry an accessible name (for
  example "2 values redacted").
- **Dynamic content**: a post-create notice that content was masked MUST use a
  polite live region.
- **Keyboard navigation**: the summary's disclosure control MUST be keyboard
  operable.
- **Focus management**: not applicable; no modal is introduced.
