---
status: draft
date: 2026-09-22
implements: [ADR-0027]
requires: [SPEC-0002, SPEC-0012]
related: [SPEC-0003, SPEC-0004, SPEC-0007, SPEC-0008]
---

# SPEC-0021: Structured Receipts

## Overview

A receipt is the record an agent leaves when it finishes a piece of work. This capability
makes it structured: an artifact carries a typed **`metadata`** object from a closed,
versioned schema registry, and the first schema is **`cairn.receipt/v1`**. That schema
holds nine answers:

1. what changed;
2. who experiences it;
3. before;
4. after;
5. how we know it is active;
6. the outcome, or when it will be measured;
7. the human action (default "Nothing");
8. what remains and who owns it;
9. what the system now knows that it didn't.

A dedicated verb creates a receipt on every surface. The server renders the receipt's
markdown body from the fields, the web shell shows it as a card, and the metadata travels
in the `artifact.created` event.

This spec realizes ADR-0027. It builds on SPEC-0002 (the artifact core and tags) and
SPEC-0012 (the event payload). It links a receipt to its trace through SPEC-0004's
`produced_artifact_id`. It adds a verb to SPEC-0007's MCP tool surface and a command to
SPEC-0008's closed command list.

Several of the records this spec leans on are being written in parallel: ADR-0022/SPEC-0016
(events and human-only reactions), ADR-0023/SPEC-0017 (redaction), ADR-0025/SPEC-0019
(validation error details), ADR-0026/SPEC-0020 (permanent retention) and ADR-0029/SPEC-0023
(teams). They are cited here in prose only until they merge.

## Requirements

### Requirement: REQ-1 Artifact Metadata and the Closed Schema Registry

An artifact MAY carry one `metadata` object. When present, it MUST contain a `schema` string
naming an entry in Cairn's schema registry. The registry MUST be closed: a write whose
`schema` is not registered MUST be rejected with `400 validation_failed`, with reason
`unknown_metadata_schema`. The only registered schema in this spec is `cairn.receipt/v1`.

Metadata MUST be validated against its schema at write time. It MUST be immutable after
creation. It MUST be at most 16 KiB when serialized. Every artifact read (REST
`GET /v1/artifacts/{id}`, MCP `artifact_read`, the Bin) MUST return `metadata` when present
and MUST omit the key when absent.

#### Scenario: Unknown schema rejected

- **WHEN** a receipt write names `schema: "cairn.approval/v1"`
- **THEN** the server MUST return `400` with reason `unknown_metadata_schema` and create nothing

#### Scenario: Metadata is immutable

- **WHEN** any client tries to change an existing artifact's metadata
- **THEN** no surface MUST offer the operation, and the stored metadata MUST be unchanged

#### Scenario: Ordinary artifacts have no metadata key

- **WHEN** a client reads a markdown artifact created with `artifact_create`
- **THEN** the response MUST NOT contain a `metadata` key

### Requirement: REQ-2 The `cairn.receipt/v1` Schema

A `cairn.receipt/v1` object MUST contain the fields below. Each text field is trimmed; a
required text field MUST be non-empty after trimming, and each MUST be at most 2,000
characters.

- `changed` (text, required): what changed.
- `affected` (text, required): who experiences it.
- `before` (text, required).
- `after` (text, required).
- `evidence` (required, may be empty): a list of at most 20 `{label, url}` objects. `label`
  is optional text of at most 120 characters. `url` MUST use the `https`, `http` or
  `mcp://cairn/` scheme and MUST be at most 2,048 characters.
- `evidence_note` (text, optional).
- `outcome` (required): an object `{status, summary, measure_at}`. `status` MUST be one of
  `measured`, `pending` or `not_applicable`. `summary` is text, required when `status` is
  `measured`. `measure_at` is an RFC 3339 date or timestamp, required when `status` is
  `pending`.
- `human_action` (text, optional). When it is omitted or blank, the server MUST store the
  literal `Nothing`.
- `remaining` (required, may be empty): a list of at most 20 `{item, owner}` objects, both
  required text.
- `learned` (text, required): what the system now knows that it didn't. "Nothing new" is a
  valid value, but the field MUST NOT be blank.
- `follows` (optional): the id or `mcp://cairn/` handle of an earlier receipt (REQ-6).

Unknown fields MUST be rejected. A validation failure MUST name the offending field and the
reason (ADR-0025).

#### Scenario: Full receipt accepted

- **WHEN** an agent submits every field, with `outcome.status = "pending"` and a `measure_at`
- **THEN** the receipt MUST be created and every field MUST read back exactly as normalized

#### Scenario: Missing learned

- **WHEN** a receipt omits `learned` or sends it blank
- **THEN** the server MUST return `400` naming `learned` with reason `required`

#### Scenario: Default human action

- **WHEN** a receipt omits `human_action`
- **THEN** the stored metadata MUST carry `human_action: "Nothing"`

#### Scenario: Pending without a date

- **WHEN** `outcome.status` is `pending` and `measure_at` is absent
- **THEN** the server MUST return `400` naming `outcome.measure_at`

#### Scenario: Unsafe evidence scheme

- **WHEN** an evidence URL uses `javascript:` or `file:`
- **THEN** the server MUST return `400` naming `evidence[n].url`

### Requirement: REQ-3 Server-Rendered Body

A receipt MUST be a `markdown` artifact whose body the server renders from the metadata. The
body MUST be rendered with the template versioned with the schema, and the caller's optional
`details` markdown MUST be appended under a `## Details` heading. Identical metadata and
details MUST render to identical bytes, and therefore to an identical checksum.

A change to the template MUST ship as a new schema version and MUST NOT alter how an existing
version renders. A caller MUST NOT be able to supply the rendered portion of the body.

#### Scenario: Deterministic rendering

- **WHEN** two receipts are created with identical fields and details
- **THEN** their bodies MUST be byte-identical, and so MUST their `checksum` values

#### Scenario: Details appended

- **WHEN** a receipt carries `details` of "See the migration log."
- **THEN** the body MUST end with a `## Details` section containing that text after the rendered fields

### Requirement: REQ-4 The `receipt` Tag

Creating a receipt MUST add the tag `receipt` to the artifact's tags if the caller did not
include it. It MUST keep the caller's other tags, subject to SPEC-0002 REQ "Artifact Tags".
A `receipt` tag on an artifact without receipt metadata MUST remain allowed and MUST render
as an ordinary artifact. Structured behavior (the card, and routing on content) MUST key off
`metadata.schema`, never off the tag.

#### Scenario: Tag added

- **WHEN** a receipt is created with tags `["repo:stump.wtf/cairn"]`
- **THEN** the stored tags MUST be `["repo:stump.wtf/cairn", "receipt"]`

#### Scenario: Loose receipt still works

- **WHEN** `artifact_create` makes a markdown artifact tagged `receipt` with no metadata
- **THEN** it MUST be created, and MUST render without a receipt card

### Requirement: REQ-5 Create Verbs on Every Surface

Receipts MUST be creatable through:

- MCP `receipt_create`: every REQ-2 field as a named, described property, plus `title`,
  `details`, `tags` and `model`;
- REST `POST /v1/receipts`: a JSON body of the same shape;
- CLI `cairn receipt --from <file|->`: JSON or YAML, plus `--details <file>`, `--tag` and
  `--title`.

Each MUST require `artifacts:write`. Each MUST create the artifact with the default link
visibility and default TTL, exactly like any other create. Each MUST record provenance
server-side (SPEC-0009).

When `title` is omitted, the server MUST derive it as `Receipt: ` followed by `changed`,
truncated to 100 characters on a word boundary.

`artifact_create` MUST NOT accept a `metadata` argument.

#### Scenario: MCP creation

- **WHEN** an agent calls `receipt_create` with valid fields
- **THEN** the result MUST carry the new artifact's id, URL, handle and checksum, and a `metadata.schema` of `cairn.receipt/v1`

#### Scenario: CLI from stdin

- **WHEN** a user pipes a YAML receipt into `cairn receipt --from -`
- **THEN** the CLI MUST create the receipt and print its link, or print the server's artifact object with `--json`

#### Scenario: Agent without write scope

- **WHEN** a token lacking `artifacts:write` calls `receipt_create`
- **THEN** the call MUST fail with a scope error, and nothing MUST be created

#### Scenario: artifact_create refuses metadata

- **WHEN** a client passes `metadata` to `artifact_create`
- **THEN** the argument MUST be rejected as an unknown property

### Requirement: REQ-6 Follow-Up Receipts

`follows` MUST name a live receipt: an artifact with `cairn.receipt/v1` metadata and the same
owner (a user, or the same team under ADR-0029). Otherwise it MUST be rejected with
`400 validation_failed`, with reason `invalid_follows`. The rejection MUST NOT disclose
whether the named artifact exists.

The card MUST show "Follows" linking to the earlier receipt. It MUST also show "Followed by",
linking to later receipts with the same owner.

#### Scenario: Outcome measured later

- **WHEN** a second receipt with `outcome.status = "measured"` follows a first whose status was `pending`
- **THEN** both cards MUST link to each other

#### Scenario: Cross-owner follows refused

- **WHEN** a receipt names a `follows` receipt owned by someone else
- **THEN** the server MUST reject it with `invalid_follows`, using the same response it gives for a nonexistent id

### Requirement: REQ-7 The Receipt Card

For an artifact with `cairn.receipt/v1` metadata, the web shell MUST render a receipt card
above the markdown body, in this order:

1. `changed`, as the TL;DR;
2. `human_action`, as a prominent "You need to do:" chip;
3. `before` and `after`;
4. the evidence links;
5. the outcome, with its status and date;
6. the remaining items with their owners;
7. `learned`, headed "What the system now knows".

Absence MUST be honest. An empty evidence list MUST render a visible "No evidence linked"
warning, and an empty remaining list MUST render "Nothing remains".

The card header MUST attribute the receipt using server-derived provenance: actor, model
when reported, on-behalf-of harness, and channel. The card MUST NOT use the words "verified"
or "confirmed" about any field.

Evidence links MUST render with `rel="noopener noreferrer"`, and the server MUST NOT fetch
them.

#### Scenario: Card order and action chip

- **WHEN** a human opens a receipt whose `human_action` is "Approve the ADR in Slack"
- **THEN** the card MUST show that text in the action chip directly after the TL;DR

#### Scenario: No evidence

- **WHEN** a receipt's `evidence` is empty
- **THEN** the card MUST show "No evidence linked" in place of the link list

#### Scenario: Provenance header

- **WHEN** an agent created the receipt over MCP for sam@example.com with model `claude-opus-5`
- **THEN** the card header MUST name that model, that human and the MCP channel as the source of the claims

### Requirement: REQ-8 Trace Link via `produced_artifact_id`

A trace span whose `produced_artifact_id` names a receipt MUST link the two, as SPEC-0004
already records in `produced_edges`. The receipt card MUST show "Produced by run" links for
every producing run **owned by the same owner as the receipt**. Runs with any other owner
MUST be omitted. The docs MUST describe the ordering: create the receipt first, then append
or close the span that names it.

#### Scenario: Same-owner run shown

- **WHEN** a user's run appends a write span naming that user's receipt
- **THEN** the receipt card MUST link to the run

#### Scenario: Foreign run hidden

- **WHEN** a different user's run names the same receipt as a produced artifact
- **THEN** the receipt card MUST NOT list that run

### Requirement: REQ-9 Metadata in Events

The `artifact.created` event for an artifact with metadata MUST carry the stored object as
`data.metadata`. The key MUST be appended after every other field, including any that
ADR-0022 appends. It MUST be omitted for artifacts without metadata, and MUST be covered by
`X-Cairn-Signature` (SPEC-0012). This capability MUST NOT change the event of any artifact
that has no metadata.

#### Scenario: Receipt event

- **WHEN** a receipt is created and outbound webhooks are configured
- **THEN** the delivered body MUST carry `data.metadata.schema = "cairn.receipt/v1"`, all stored fields, and `data.tags` containing `receipt`

#### Scenario: Non-receipt event unchanged

- **WHEN** an untagged markdown artifact is created
- **THEN** its event body MUST be byte-identical to what the same build produced before this capability, with no `metadata` key

### Requirement: REQ-10 Redaction Covers Metadata

The ingest scanner of ADR-0023 MUST scan every metadata string field and the rendered body
before anything is persisted or emitted. A finding MUST be handled exactly as a finding in an
ordinary body. The scanner's reject or mask policy MUST apply field by field, and a masked
field MUST render masked in the body, the card and the event.

#### Scenario: Token in a field

- **WHEN** a receipt's `evidence_note` contains a credential that the scanner detects
- **THEN** the receipt MUST be rejected or masked according to the scanner policy, and no unredacted copy MUST reach storage, the event or the index

### Requirement: REQ-11 Plugin Template

The first-party Claude Code plugin's `/cairn:receipt` command SHOULD collect the nine answers
and call `receipt_create`. It SHOULD warn that a receipt expires on the default TTL unless it
is retained (ADR-0026). The plugin lives in its own repository; this requirement records the
contract it targets.

#### Scenario: Plugin uses the verb

- **WHEN** a user runs `/cairn:receipt` in a session with the Cairn MCP connected
- **THEN** the plugin SHOULD call `receipt_create` rather than `artifact_create`

### Requirement: Error Handling Standards

Receipt validation failures MUST be typed validation errors that carry a field path (such as
`outcome.measure_at` or `evidence[2].url`) and a reason. They MUST be rendered in the
ADR-0012 envelope and logged with structured key-value fields. Rendering failures MUST be
wrapped with context and MUST NOT be swallowed. A receipt whose body fails to render MUST NOT
be created.

#### Scenario: Field path in the error

- **WHEN** the third evidence entry has a malformed URL
- **THEN** the error details MUST name `evidence[2].url` and reason `invalid_url`

### Requirement: Database Operation Standards

A receipt create MUST insert the blob reference, the artifact row, the tags and the metadata
in the single transaction the existing create path uses. `follows` resolution MUST use a
parameterized query inside that transaction. Connections MUST be returned to the pool with
bounded timeouts.

#### Scenario: Atomic receipt create

- **WHEN** a receipt create fails after its body is rendered but before commit
- **THEN** no artifact row MUST exist, and no event MUST be emitted

## Endpoint Table

| Endpoint | Method | Purpose | Auth |
|---|---|---|---|
| `/v1/receipts` | POST | Create a structured receipt | Required: `artifacts:write` (humans and agents) |
| `/v1/artifacts/{id}` | GET | Read, including `metadata` | **Public**: the capability URL is the read token (ADR-0007) |
| `/{id}` | GET | Web shell with the receipt card | **Public**: same capability-URL rationale |

## Security Requirements

### Requirement: Authentication & Authorization

`POST /v1/receipts` and `receipt_create` MUST require authentication and `artifacts:write`.
Ownership MUST follow the standard create rules: the authenticated human, or a team under
ADR-0029. Metadata MUST never be trusted as identity.

#### Scenario: Unauthenticated create

- **WHEN** an unauthenticated client posts to `/v1/receipts`
- **THEN** the server MUST return `401` and create nothing

### Requirement: Rate Limiting

`/v1/receipts` MUST sit behind the existing per-IP create limiter. Exceeding it MUST return
`429` with `Retry-After`.

#### Scenario: Receipt burst

- **WHEN** a client exceeds the create rate on `/v1/receipts`
- **THEN** the server MUST return `429` with `Retry-After`

### Requirement: Security Headers

The card MUST render inside the existing shell with its strict CSP,
`X-Content-Type-Options: nosniff`, `Referrer-Policy`, `X-Frame-Options: DENY` and (over
HTTPS) HSTS. Field text MUST be HTML-escaped. It MUST never be rendered as markdown or HTML
inside the card.

#### Scenario: HTML in a field

- **WHEN** `changed` contains `<script>alert(1)</script>`
- **THEN** the card MUST show the literal text, and no script MUST execute

### Requirement: Request Body Size Limits

`POST /v1/receipts` MUST cap its body at 256 KiB (16 KiB of metadata plus details) before
buffering. Oversize requests MUST return `413`.

#### Scenario: Oversize receipt

- **WHEN** a receipt request exceeds 256 KiB
- **THEN** the server MUST return `413` and create nothing

### Requirement: CSRF Protection

A session-authenticated `POST /v1/receipts` MUST pass the existing CSRF seam.
Token-authenticated calls are exempt.

#### Scenario: Cross-site receipt

- **WHEN** a session-authenticated receipt create arrives without a valid CSRF token
- **THEN** the server MUST reject it

### Requirement: Redirect & SSRF Validation

The server MUST NOT fetch, preview or unfurl evidence URLs. No endpoint in this capability
redirects to a user-supplied URL.

#### Scenario: No server fetch

- **WHEN** a receipt links evidence at an internal-network URL
- **THEN** the server MUST store and render the link without ever requesting it

## Accessibility Requirements

### Requirement: WCAG 2.1 AA and Semantics

The receipt card MUST meet WCAG 2.1 AA. It MUST be a labelled region
(`aria-labelledby` pointing at its TL;DR heading). Before and after MUST be headed sections
rather than a visual-only split, and the remaining items MUST be a real table with header
cells.

#### Scenario: Screen reader structure

- **WHEN** a screen-reader user navigates by headings on a receipt
- **THEN** they MUST reach the TL;DR, Before, After, Evidence, Outcome, Remaining and "What the system now knows" headings in order

### Requirement: Icon-Only Controls

Any icon-only control on the card, such as copy-link or a trace link glyph, MUST carry an
`aria-label`. The evidence-warning icon MUST be accompanied by its text.

#### Scenario: Copy control label

- **WHEN** a screen reader reaches the card's copy-link control
- **THEN** it MUST announce "Copy receipt link"

### Requirement: Dynamic Content Regions

Updates to the card after it renders, such as "Followed by" links loaded after the page,
MUST be announced through an `aria-live="polite"` region.

#### Scenario: Late follow-up link

- **WHEN** a "Followed by" link is inserted after the initial render
- **THEN** it MUST be announced politely

### Requirement: Keyboard Navigation & Focus Management

Every link and control on the card MUST be reachable in visual order with Tab and operable
with Enter. The card MUST NOT trap focus.

#### Scenario: Keyboard traversal

- **WHEN** a keyboard user tabs through a receipt card
- **THEN** focus MUST move through the evidence links, the trace link and the follow links in visual order
