---
status: draft
date: 2026-09-22
implements: [ADR-0025]
requires: [SPEC-0002]
related: [SPEC-0006, SPEC-0007, SPEC-0008, SPEC-0004, SPEC-0009]
---

# SPEC-0019: Actionable Validation Errors

## Graph Edges

- **Implements:** **ADR-0025**: validation errors carry field, reason and limit.
- **Requires:** **SPEC-0002**: the REST surface and ADR-0012 envelope this
  extends.
- **Related:** **SPEC-0006** (annotation validation), **SPEC-0007** (MCP tool
  errors), **SPEC-0008** (CLI rendering and tag folding), **SPEC-0004** (span
  validation), **SPEC-0009** (policy and TTL endpoints).

## Overview

A `validation_failed` response today says only "the request was invalid". This
capability defines a `violations` array: field, location, reason code, limit, a
bounded echo of the value, and a human message. It is rendered identically on
REST, MCP and the CLI. It is additive to the ADR-0012 envelope, and leaves
not-found, unauthorized and forbidden responses byte-identical. The CLI also
folds tag case, with a warning. See ADR-0025.

Requirement IDs (`VE-n`) are stable and are cited as `SPEC-0019 VE-n`.

## Requirements

### Requirement: VE-1 Violation Shape

A `validation_failed` or `payload_too_large` error envelope MUST carry a
`violations` array with at least one entry. Each entry MUST have:

- `field`: the caller-facing name (header, query parameter, form field, or a
  JSON path such as `tags[2]`, `members[3].content`, `spans[12].args`);
- `location`: one of `header`, `query`, `form`, `body`, `path`;
- `reason`: a code from the VE-2 registry;
- `message`: a human sentence built from the other fields.

An entry MAY have:

- `limit`: a JSON number or string;
- `unit`: one of `seconds`, `bytes`, `count`, `chars`;
- `value`;
- reason-specific keys that VE-2 documents (for example `rule`, `line`,
  `column`).

#### Scenario: TTL over the cap

- **WHEN** a create sends `X-Cairn-Ttl-Seconds: 5184000` to a server whose maximum is 30 days
- **THEN** the response is 400 `validation_failed`, with one violation whose field is `X-Cairn-Ttl-Seconds`, location `header`, reason `exceeds_max`, limit `2592000`, unit `seconds`, and value `"5184000"`

#### Scenario: Malformed TTL

- **WHEN** a create sends `X-Cairn-Ttl-Seconds: 7d`
- **THEN** the violation's reason is `invalid_format`, and its message says the value must be a positive integer number of seconds

#### Scenario: Oversize body

- **WHEN** a body exceeds the upload cap
- **THEN** the response is 413 `payload_too_large`, with one violation whose field is `body`, reason `too_large`, limit the cap in bytes, and unit `bytes`

### Requirement: VE-2 Closed Reason Registry

`reason` MUST be one of: `required`, `invalid_format`, `invalid_charset`,
`uppercase`, `too_long`, `too_short`, `too_many`, `exceeds_max`, `not_positive`,
`unknown_value`, `duplicate`, `mismatch`, `not_allowed`, `checksum_mismatch`,
`too_large`, `too_large_to_scan`, `secret_detected`, `invalid`.

`invalid` is reserved for sites not yet migrated (VE-6). A new reason MUST be
added to this registry, and to the public error reference, before any code
emits it. `secret_detected` entries MAY carry `rule`, `line` and `column`.

#### Scenario: Registry is enforced

- **WHEN** a test enumerates every reason constant in code
- **THEN** each is in this registry, and each registry entry is documented in the public error reference

### Requirement: VE-3 Safe Value Echo

`value` MUST be the offending input, truncated to at most 64 bytes on a UTF-8
boundary. It MUST be omitted when:

- the reason is `secret_detected`;
- the field carries body content (`body`, `members[*].content`, comment bodies,
  span args or outputs);
- the field is a credential-bearing header.

`value` and `message` MUST be JSON-encoded, and MUST NOT be rendered as HTML by
any Cairn surface without escaping.

#### Scenario: Tag echoed

- **WHEN** a create carries the tag `Size:M`
- **THEN** the violation's field is `tag`, its reason is `uppercase`, and its value is `"Size:M"`

#### Scenario: Secret never echoed

- **WHEN** a code artifact is rejected for a detected credential (Cairn SPEC-0017)
- **THEN** the violation has reason `secret_detected`, `rule`, `line` and `column`, and no `value` key

### Requirement: VE-4 Cumulative Where Cheap

Validators over lists MUST report every failing element, not only the first:

- tags from every source on one request;
- bundle members' names and sizes;
- span-tree structural errors within one batch.

The top-level `message` MUST be the first violation's `"<field>: <message>"`
when there is one violation, and `"<n> problems: <first field>: <first message>; …"`
when there are several.

#### Scenario: Two bad tags

- **WHEN** a create carries tags `Size:M` and `lane m`
- **THEN** there are two violations, `uppercase` for `Size:M` and `invalid_charset` for `lane m`, and the message begins `2 problems:`

### Requirement: VE-5 Envelope Compatibility and Uniformity

- `code`, `message`, `details` and `request_id` MUST keep their ADR-0012 types.
- `details` MUST remain a flat string map, keeping its existing keys. It MUST
  additionally carry `field`, equal to the first violation's field.
- Responses with codes other than `validation_failed` and `payload_too_large`
  MUST carry no `violations`, and MUST be byte-identical to their pre-change
  form. Not-found stays uniform across unknown, unauthorized and expired ids.

#### Scenario: Old client decodes

- **WHEN** a client that decodes `details` as `map[string]string` and ignores unknown keys receives a new validation error
- **THEN** it decodes without error, and shows the new top-level message

#### Scenario: Not-found unchanged

- **WHEN** an unknown artifact id is read
- **THEN** the response body is byte-identical to the pre-change body (apart from `request_id`)

### Requirement: VE-6 Domain-Built Violations and Migration Guard

Violations MUST be built in the domain layer as a typed error that wraps
`ErrValidation`, so `CodeOf` and `errors.Is` are unchanged. Transports MUST NOT
derive violations by parsing error strings. A bare validation error MUST render
as one violation with field `request`, reason `invalid`, and the generic message.

A test MUST enumerate the client-reachable validation sites for artifact create,
bundles, tags, TTL, policy, annotations, runs and hooks. It MUST fail if any of
them returns a bare validation error.

#### Scenario: Unmigrated site still renders

- **WHEN** an unmigrated validator fails
- **THEN** the response carries one violation with reason `invalid`, and the top-level message is the generic one

#### Scenario: Guard catches a regression

- **WHEN** a contributor changes a migrated validator back to a bare validation error
- **THEN** the migration guard test fails, naming the site

### Requirement: VE-7 MCP Parity

A failing MCP tool call caused by validation MUST return:

- an error result whose text content is the top-level message;
- structured content `{"code": "validation_failed", "violations": [...]}` with
  the same violations REST would return for the equivalent request.

It MUST NOT return the bare string `validation_failed: the request was invalid`.

#### Scenario: Agent learns the tag rule

- **WHEN** an agent calls `artifact_create` with the tag `Handoff`
- **THEN** the tool error's structured content contains a violation with field `tags[0]`, reason `uppercase`, and value `"Handoff"`

### Requirement: VE-8 CLI Rendering

The CLI MUST decode `violations`, and print one line per violation to stderr. It
MUST map wire fields to its own flags: `X-Cairn-Ttl-Seconds` is `--ttl`, `tag`
and `tags[n]` are `--tag`, and `X-Cairn-Type` / `type` is `--type`. It MUST
express TTL limits in the largest whole unit (`30d`). It MUST exit with the
usage exit code for `validation_failed`, as today.

#### Scenario: The reported TTL failure explains itself

- **WHEN** `cairn add notes.md --ttl 60d` is sent to a server with a 30-day maximum
- **THEN** stderr reads `cairn: --ttl 60d exceeds the server's maximum of 30d`, and the exit code is the usage code

### Requirement: VE-9 CLI Tag Case Folding

Before sending, the CLI MUST lower-case each `--tag` value that contains
uppercase ASCII. For each changed tag it MUST print
`cairn: warning: tag "<given>" sent as "<folded>"` to stderr. It MUST NOT alter
any other character, and MUST NOT fold on any non-CLI surface. The server's
rejection of uppercase tags (ADR-0018) is unchanged.

#### Scenario: Uppercase tag folded with a warning

- **WHEN** `cairn add plan.md --tag size:M` runs
- **THEN** the request carries `size:m`, stderr shows the warning, and the create succeeds

#### Scenario: Charset still the server's call

- **WHEN** `cairn add plan.md --tag "lane m"` runs
- **THEN** the CLI sends it unchanged, and renders the server's `invalid_charset` violation

### Requirement: Error Handling Standards

- Errors MUST be wrapped with context at each layer. The violation MUST survive
  wrapping (`errors.As`).
- Sentinel errors MUST remain discoverable with `errors.Is(err, errs.ErrValidation)`.
- The server log line for a validation failure MUST keep the full internal error
  string. Only the response is shaped by violations.
- Structured logging MUST be used.

#### Scenario: Log keeps the internal detail

- **WHEN** a TTL violation is rendered
- **THEN** the log line still carries the wrapped internal error, and the response carries only the violation

## Security Requirements

- **Authentication**: Unchanged. Violations are returned only on requests that
  reach validation. An unauthenticated request still gets the uniform 401 first.
- **Information disclosure**: Only `validation_failed` and `payload_too_large`
  gain detail. Violations name caller-supplied fields and server-published
  limits, never internal state, other users' data, internal ids or storage
  errors. Not-found uniformity is preserved (VE-5).
- **Rate limiting**: Unchanged.
- **Security headers**: Unchanged. Error bodies are JSON.
- **Request body size limits**: Unchanged. The 413 now states the limit.
- **CSRF protection**: Unchanged.
- **Redirect validation**: Not applicable.
- **Output encoding**: Echoed values are capped (VE-3) and JSON-encoded, and
  secrets are never echoed.

## Accessibility Requirements

Web forms that surface these errors (the share dialog's TTL control, and the
comment composer):

- **WCAG 2.1 AA** is the target.
- **ARIA landmarks**: unchanged.
- **Icon-only controls**: an error icon MUST carry an accessible name.
- **Dynamic content**: a violation message MUST be announced through a polite
  live region, and associated with its control through `aria-describedby`.
- **Keyboard navigation**: focus MUST move to the first invalid control after a
  rejected submit.
- **Focus management**: in the share dialog, focus stays trapped in the dialog
  while an error is shown.
