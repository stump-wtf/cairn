---
title: Errors
sidebar_label: Errors
sidebar_position: 5
slug: /errors
---

# Errors

Every failed Cairn request returns the same small JSON envelope, whether it came
from `curl`, the CLI, or an agent over MCP. When the request itself was wrong, the
envelope also lists each problem: the field, the reason, the limit it broke, and what
you sent. An agent can fix its own call from that without a person in the loop.

The envelope is [ADR-0012](../decisions/ADR-0012.md). The `violations` list is
[ADR-0025](../decisions/ADR-0025.md), specified in
[SPEC-0019](../specs/validation-errors/index.md).

## The envelope

Every non-2xx REST response has this shape:

```json
{
  "error": {
    "code": "not_found",
    "message": "not found or expired",
    "details": {"id": "<id>"},
    "request_id": "<request id>"
  }
}
```

| Key | Type | Meaning |
|---|---|---|
| `code` | string | A stable machine code. Branch on this, never on `message`. |
| `message` | string | A human sentence. For a rejected request it names the problem (see [The top-level message](#the-top-level-message)). |
| `details` | object of strings | Identifiers the handler echoes back, such as the `id` from the path. Optional. It never repeats violation data. |
| `violations` | array | Only on `validation_failed` and `payload_too_large`. See below. |
| `request_id` | string | Quote it when you ask for help. The server log line for the request carries the same id. |

| `code` | HTTP status |
|---|---|
| `validation_failed` | 400 |
| `unauthorized` | 401 |
| `forbidden` | 403 |
| `not_found` | 404 |
| `conflict` | 409 |
| `payload_too_large` | 413 |
| `rate_limited` | 429 |
| `internal` | 500 |

## Violations

A `validation_failed` or `payload_too_large` response always carries at least one
violation. This one is a create that asked for a 60-day TTL from a server whose maximum
is 30 days:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "X-Cairn-Ttl-Seconds: \"5184000\" exceeds the maximum of 2592000 seconds",
    "violations": [
      {
        "field": "X-Cairn-Ttl-Seconds",
        "location": "header",
        "reason": "exceeds_max",
        "limit": 2592000,
        "unit": "seconds",
        "value": "5184000",
        "message": "\"5184000\" exceeds the maximum of 2592000 seconds"
      }
    ],
    "request_id": "<request id>"
  }
}
```

Each violation has these keys:

| Key | Always present | Meaning |
|---|---|---|
| `field` | yes | The name you used: a header (`X-Cairn-Ttl-Seconds`), a query parameter or form field (`tag`), or a JSON path into the body (`tags[2]`, `members[3].content`, `spans[12].args`). |
| `location` | yes | Where that field travelled: `header`, `query`, `form`, `body`, or `path`. |
| `reason` | yes | A code from the [reason registry](#reasons). Branch on this. |
| `message` | yes | A sentence built from the other keys. It doesn't repeat the field name. |
| `limit` | no | The bound you broke. Usually a number; a string where the bound is a list of accepted values. |
| `unit` | no | The unit of `limit`: `seconds`, `bytes`, `count`, or `chars`. |
| `value` | no | What you sent, capped at 64 bytes. See [Echoed values](#echoed-values). |
| `rule`, `line`, `column` | no | Only on `secret_detected`: which detection rule matched, and where. |

A client should ignore keys it doesn't know. The order of keys inside a violation isn't
significant.

### The top-level message

The envelope's `message` is built from the violations:

- **One violation:** `<field>: <message>`, for example
  `X-Cairn-Ttl-Seconds: "5184000" exceeds the maximum of 2592000 seconds`.
- **Several:** `<n> problems: ` followed by each `<field>: <message>`, joined with
  `; `. A create carrying the tags `Size:M` and `lane m` gets
  `2 problems: tag: "Size:M" must be lowercase; tag: "lane m" contains a character that is not allowed: use only lowercase a-z, 0-9 and ._:/#-`.
- **An endpoint that hasn't said what was wrong yet:** the generic
  `the request was invalid`, with a single `invalid` violation.

Validators over lists report every bad element, not only the first: every bad tag
from every source on one request, every bad bundle member, and every structural
error in one batch of spans. One response names at most 64 violations.

### Echoed values

`value` lets you see which of your inputs was rejected. It is never more than 64 bytes,
and is cut on a UTF-8 character boundary, never through a character. It is **never**
present when:

- the reason is `secret_detected`;
- the field carries content: `body`, `members[n].content`, a comment body, or a span's
  `args` or `output`;
- the field is a header that carries a credential, such as `Authorization`.

`value` and `message` are JSON strings, and no Cairn surface renders them as HTML
without escaping them. If you display them in a page of your own, escape them too: they
are what the caller sent.

## Reasons

`reason` is always one of these codes. The list is closed: a new reason is added here
before anything emits it.

| Reason | Meaning | `limit` |
|---|---|---|
| `required` | The field is missing or empty. | — |
| `invalid_format` | The value doesn't parse as the expected kind of thing. | — |
| `invalid_charset` | The value contains a character the field doesn't allow. | — |
| `uppercase` | The value is right apart from its case. Lower-case it. | — |
| `too_long` | The value is longer than allowed. | the maximum |
| `too_short` | The value is shorter than allowed. | the minimum |
| `too_many` | A list has more entries than allowed. | the maximum count |
| `exceeds_max` | A number is over the server's maximum. | the maximum |
| `not_positive` | A number must be greater than zero. | — |
| `unknown_value` | The value isn't one of the accepted values. | the accepted values, when listed |
| `duplicate` | The value repeats one that must be unique. | — |
| `mismatch` | The value doesn't agree with something else in the request. | — |
| `not_allowed` | The value is well-formed but isn't permitted here. | — |
| `checksum_mismatch` | The body's SHA-256 isn't the one you declared. | — |
| `too_large` | The body or part is over the size cap. Returned as `413 payload_too_large`. | the cap |
| `too_large_to_scan` | A text field is over the cap for scanning it for credentials. | the cap |
| `secret_detected` | A credential was found in content that is rejected rather than masked. | — |
| `invalid` | A generic rejection from a check that doesn't name its reason yet. | — |

The examples below show the shape of each. The field a reason appears on depends on the
endpoint, so treat the field names as illustrations.

### `required`

An `artifact_create` call with an empty `body`:

```json
{"field": "body", "location": "body", "reason": "required", "message": "is required"}
```

### `invalid_format`

`X-Cairn-Ttl-Seconds: 7d`. The header takes whole seconds; the CLI converts `7d` for you.

```json
{"field": "X-Cairn-Ttl-Seconds", "location": "header", "reason": "invalid_format", "value": "7d",
 "message": "\"7d\" is not valid: it must be a positive integer number of seconds"}
```

### `invalid_charset`

A tag with a space in it:

```json
{"field": "tag", "location": "header", "reason": "invalid_charset", "value": "lane m",
 "message": "\"lane m\" contains a character that is not allowed: use only lowercase a-z, 0-9 and ._:/#-"}
```

### `uppercase`

A tag with a capital letter. Cairn rejects it rather than changing it, so what a
downstream rule matches is exactly what you sent. (The CLI is the one exception: it
lower-cases `--tag` values before sending them, and warns you. See
[Troubleshooting](../guides/troubleshooting.md#uppercase-tags).)

```json
{"field": "tag", "location": "header", "reason": "uppercase", "value": "Size:M",
 "message": "\"Size:M\" must be lowercase"}
```

### `too_long`

A 74-byte tag, sent to `artifact_create` as the first entry of `tags`. The echoed value
stops at 64 bytes.

```json
{"field": "tags[0]", "location": "body", "reason": "too_long", "limit": 64, "unit": "bytes",
 "value": "summary:this-tag-is-far-too-long-to-be-useful-as-a-routing-hint-",
 "message": "\"summary:this-tag-is-far-too-long-to-be-useful-as-a-routing-hint-\" is too long: the maximum is 64 bytes"}
```

### `too_short`

A declared checksum that has been cut short:

```json
{"field": "X-Cairn-Sha256", "location": "header", "reason": "too_short", "limit": 64, "unit": "chars",
 "value": "9f86d081884c", "message": "\"9f86d081884c\" is too short: the minimum is 64 chars"}
```

### `too_many`

More than 32 distinct tags across the `?tag=` parameters:

```json
{"field": "tag", "location": "query", "reason": "too_many", "limit": 32, "unit": "count",
 "message": "has too many entries: the maximum is 32"}
```

### `exceeds_max`

A 60-day TTL against the 30-day maximum:

```json
{"field": "X-Cairn-Ttl-Seconds", "location": "header", "reason": "exceeds_max", "limit": 2592000,
 "unit": "seconds", "value": "5184000", "message": "\"5184000\" exceeds the maximum of 2592000 seconds"}
```

### `not_positive`

A TTL of zero:

```json
{"field": "X-Cairn-Ttl-Seconds", "location": "header", "reason": "not_positive", "value": "0",
 "message": "\"0\" is not valid: it must be a positive integer number of seconds"}
```

### `unknown_value`

A `run_create` mode that doesn't exist. Here `limit` is a string listing the accepted
values.

```json
{"field": "mode", "location": "body", "reason": "unknown_value", "limit": "batch, open",
 "value": "stream", "message": "\"stream\" is not a recognised value: expected batch, open"}
```

### `duplicate`

Two bundle members with the same name:

```json
{"field": "members[1].name", "location": "body", "reason": "duplicate", "value": "notes.md",
 "message": "\"notes.md\" is a duplicate"}
```

### `mismatch`

An anchor whose `anchor_ref` doesn't fit the shape its `anchor_type` needs:

```json
{"field": "anchor_ref", "location": "body", "reason": "mismatch",
 "message": "does not match the code_line locator schema"}
```

### `not_allowed`

A `code_line` anchor on a markdown artifact. Use `anchor_type: "artifact"` to comment on
the whole thing.

```json
{"field": "anchor_type", "location": "body", "reason": "not_allowed", "value": "code_line",
 "message": "\"code_line\" is not allowed: a markdown artifact does not take this anchor"}
```

### `checksum_mismatch`

The body's SHA-256 isn't the one in `X-Cairn-Sha256`, so it changed in transit or the
checksum was computed over different bytes:

```json
{"field": "X-Cairn-Sha256", "location": "header", "reason": "checksum_mismatch",
 "message": "does not match the SHA-256 of the received body"}
```

### `too_large`

A body over the upload cap (64 MiB by default). This is the one reason that changes the
status: when every violation is `too_large`, the response is `413 payload_too_large`.

```json
{"field": "body", "location": "body", "reason": "too_large", "limit": 67108864, "unit": "bytes",
 "message": "is too large: the maximum is 67108864 bytes"}
```

### `too_large_to_scan`

A text field over the credential-scanning cap (16 MiB by default). Cairn won't store
text it hasn't scanned unless the operator has opted into that.

```json
{"field": "body", "location": "body", "reason": "too_large_to_scan", "limit": 16777216, "unit": "bytes",
 "message": "is too large to scan for credentials: the maximum is 16777216 bytes"}
```

### `secret_detected`

A credential in content that is rejected rather than masked. `rule` names the detection
rule, and `line` and `column` say where it starts. There is never a `value`: echoing the
credential back would leak it a second time.

```json
{"field": "body", "location": "body", "reason": "secret_detected", "rule": "github-pat", "line": 12, "column": 9,
 "message": "a credential was detected (rule github-pat, line 12); remove it or resend with --redact=mask"}
```

### `invalid`

The generic rejection. It comes from a check that hasn't been taught to name its field
and reason yet, and the top-level message is the bare `the request was invalid`. If you
hit one, the `request_id` is the useful thing to report.

```json
{"field": "request", "location": "body", "reason": "invalid", "message": "the request was invalid"}
```

## On MCP and the CLI

The violations are the same on every surface; only the wrapping differs.

- **MCP.** A tool call that fails validation returns an error result. Its text is the
  top-level message, and its structured content is
  `{"code": "validation_failed", "violations": [...]}`, with the same violations REST
  returns for the equivalent request. MCP fields use the tool's argument names, so a bad
  tag is `tags[0]` rather than `tag`.
- **CLI.** `cairn` prints one line per violation to stderr, in terms of its own flags:
  `X-Cairn-Ttl-Seconds` is `--ttl`, `tag` and `tags[n]` are `--tag`, and `X-Cairn-Type`
  is `--type`. TTL limits are shown in the largest whole unit, so a 60-day request reads
  `cairn: --ttl 60d exceeds the server's maximum of 30d`. A rejected request exits with
  the usage exit code.

## Errors that never carry violations

Only `validation_failed` and `payload_too_large` carry `violations`. Every other code
keeps its short, fixed message and no field detail:

| `code` | Why there is no detail |
|---|---|
| `not_found` | An unknown id, an id you may not see, and an expired id all return the identical `not found or expired`. Anything more would tell a stranger which ids exist. |
| `unauthorized` | Cairn checks who you are before it looks at the request's fields, so an unauthenticated request gets the same `401` whatever it sent. |
| `forbidden` | The request was understood, and you aren't allowed to make it. There is no field to fix. |
| `conflict`, `rate_limited`, `internal` | The request was well-formed. The problem is state on the server, not something you can fix field by field. |

This is deliberate. A violation only ever names a field you sent and a limit the server
publishes. It never names internal state, another user's data, an internal id, or a
storage error.

For what to do about the common ones, see [Troubleshooting](../guides/troubleshooting.md).
