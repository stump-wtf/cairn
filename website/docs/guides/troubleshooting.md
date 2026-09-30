---
title: Troubleshooting
sidebar_label: Troubleshooting
sidebar_position: 8
---

# Troubleshooting

Cairn's errors are deliberately short, so this page maps each one to its usual causes.
REST errors come back as JSON:

```json
{"error": {"code": "not_found", "message": "not found or expired", "details": {"id": "<id>"}, "request_id": "…"}}
```

MCP tools report the same pair as their error text, for example
`not_found: not found or expired`. If you ask someone for help, include the `request_id`.
A rejected request also lists what was wrong with it; [Errors](../product/errors.md) is
the full reference.

## A read came back truncated

**`body_truncated: true` from `artifact_read`.** The tool inlines at most 1 MiB of body.
Nothing is wrong with the artifact; the rest is at its `url`. An agent should work with
what it got, read a smaller bundle file with `path`, or hand the web link to a person.
There's no offset parameter, so looping to page through the rest over MCP won't work.

**"…(truncated — read the full artifact via artifact_read)" in an A2UI view.** A2UI views
cap how much body they render. Call `artifact_read` for the full text.

**`body_encoding: "base64"` where you expected text.** The body isn't valid UTF-8, so it
arrived base64-encoded. Decode it, or check that it was uploaded as text in the first
place.

**An image or binary file created over MCP is corrupt.** `artifact_create` stores its
`body` string as-is, and binary content doesn't survive that. Upload binary files over
REST or with the CLI.

**A trace span shows an empty row.** The span was sent without `output`. Large outputs are
fine, since they're stored separately and loaded when you expand the span, but a missing
output can't be recovered.

## "Not found or expired"

A single `404 not_found` covers all of these on purpose, so a stranger can't tell which
ids exist:

- **It expired.** Artifacts expire 7 days after creation unless the owner changed that,
  and expired content is deleted. Ask whoever shared it to push it again.
- **The link was rotated.** Rotating mints a new id and kills the old link and the old MCP
  handle.
- **The id is wrong.** Ids are case-sensitive.
- **You passed a web link to `artifact_read`.** It takes a bare id or an `mcp://cairn/…`
  handle. Drop the `https://cairn.stump.wtf/` prefix.
- **A webhook ingress URL returns `404`.** The endpoint expired, or the id is wrong. Create
  a new endpoint; an expired one doesn't come back.

## Authentication and scopes

On your own instance, check first that each setting is on the right side: the server reads
`CAIRN_API_TOKENS` and `CAIRN_BASE_URL`, while the CLI reads `CAIRN_TOKEN` and `CAIRN_URL`.
[Which variable is which](./self-hosting.md#which-variable-is-which) lists them all.

| You see | Likely cause | Fix |
|---|---|---|
| `401` with `unauthorized: authentication required` | No token, a typo, a revoked token, or a header without `Bearer` | Send `Authorization: Bearer cairn_pat_…`, and mint a new token if the old one was revoked |
| `401` from `/mcp` with a `WWW-Authenticate` header | The client sent no credentials | Configure a token header, or let the client run OAuth ([Connect your agent](./connect-your-agent.md)) |
| An agent's Cairn calls stop working mid-task | Its token was revoked, or its session was ended in Settings | Reconnect it. An agent should report the failure and stop, not retry in a loop. |
| `insufficient_scope: artifact_create requires the artifacts:write scope` | The token or OAuth grant lacks that scope | Mint a token with the scope, or re-authenticate the client and approve it |
| `403 forbidden` when deleting | The token is marked as an agent token | Delete with a token that isn't an agent token |
| `404` when deleting an artifact you can open | You aren't its owner; only the owner can delete | Ask the owner, or let it expire |
| Can't change sharing or expiry with a token | Those controls are web-only | Change them from the share dialog, signed in as the owner |
| The CLI can't reach its server | It's aimed at a different deployment than you expect — the default is the hosted service | Check `cairn whoami`, then point it with `--url` or `export CAIRN_URL=https://your-cairn.example` |

## The request was rejected

`400 validation_failed` means something in the request itself was wrong. The response's
`violations` list says which field, why, and the limit it broke; each `reason` is
explained in [Errors](../product/errors.md#reasons). The usual suspects:

- **A TTL over 30 days.** A longer `X-Cairn-Ttl-Seconds` or `--ttl` is rejected, not
  shortened. See [`--ttl` is over the maximum](#--ttl-is-over-the-maximum).
- **The wrong create tool.** `artifact_create` won't make a bundle or a trace. Use
  `bundle_create` or `run_create`.
- **An empty body.** `artifact_create` needs a non-empty `body`.
- **An anchor that doesn't fit the type.** Comment and reaction anchors are checked against
  the share type, so a `code_line` anchor on a markdown artifact fails. Use
  `anchor_type: "artifact"` to target the whole thing.
- **A span with an unknown parent.** `run_append_spans` rejects the whole batch. Send
  parents before their children.
- **A tag that breaks the rules.** Tags are lowercase `a-z`, `0-9`, and `. _ : / # -`,
  1 to 64 bytes each, with at most 32 per artifact. Cairn rejects a bad tag rather than
  fixing it, so lowercase run ids and timestamps yourself. (The CLI lower-cases
  `--tag` for you; see [Uppercase tags](#uppercase-tags).)

The MCP create tools also reject any field they don't define, such as `visibility` or
`ttl`.

### `secret_detected`: a credential was found

Cairn scans what you store for credentials. Code artifacts and bundles are refused when
one turns up; everything else is masked (see
[Secret redaction](./self-hosting.md#secret-redaction)). A refusal is a
`400 validation_failed` with one violation per finding. Each names the field, the rule,
and, where known, the line and column. None names the value:

```json
{"error": {"code": "validation_failed", "message": "…", "violations": [
  {"field": "members[2].content", "location": "body", "reason": "secret_detected",
   "rule": "github-pat", "line": 42, "column": 10,
   "message": "a credential was detected (rule github-pat, line 42); remove it or resend with --redact=mask"}
], "request_id": "…"}}
```

The field is `body` or `title` for a single artifact, and `members[n].content` for a
bundle member, counted from zero. Nothing from a refused create is stored.

- **Remove the credential** and send again. If it was real, rotate it too: it has already
  left the machine it came from.
- **Or store it masked.** The value becomes `[REDACTED]` and the rest is kept. With the
  CLI, pass `--redact=mask`, for example `cairn add patch.diff --redact=mask`. Over REST,
  send `X-Cairn-Redaction: mask`. Over MCP, pass `redaction: "mask"` to `artifact_create`
  or `bundle_create`.
- **`mask` is the only accepted value.** Scanning can't be turned off, so
  `X-Cairn-Redaction: off` is itself `validation_failed`, with reason `unknown_value`.
- **Refused although the content masks.** Rarely, Cairn finds a value it can't replace
  cleanly. It refuses the write rather than store it, and `--redact=mask` doesn't help.
  Remove the value and send again.
- **The value is a harmless fixture.** Cairn can't tell a test value from a real one. The
  operator can exempt known fixture values in the
  [allowlist](./self-hosting.md#operator-allowlist); no request can.

### `too_large_to_scan`: a field is over the scan cap

Each text field is scanned up to a cap, 16 MiB by default. A larger text field is refused
with reason `too_large_to_scan`, and `limit` gives the cap in bytes:

```json
{"field": "body", "location": "body", "reason": "too_large_to_scan", "limit": 16777216, "unit": "bytes", "message": "…"}
```

Split the content into smaller artifacts, or ask the operator whether the cap
(`CAIRN_REDACTION_MAX_SCAN_BYTES`) can be raised. Binary bodies such as images and
archives aren't scanned, so the cap doesn't apply to them. A webhook capture is never
refused for being over the scan cap: the field is replaced with a notice and listed in
the capture's `redaction_withheld`.

### `--ttl` is over the maximum

```text
cairn: --ttl 60d exceeds the server's maximum of 30d
```

An artifact can live for at most 30 days. Cairn rejects a longer request rather than
quietly shortening it, so you never believe a share lasts longer than it does. Over
REST the same failure is a violation on `X-Cairn-Ttl-Seconds` with reason
`exceeds_max`, `limit` `2592000` and unit `seconds`.

Ask for 30 days or less: `--ttl 30d`, or `X-Cairn-Ttl-Seconds: 2592000`.
The maximum isn't a server setting, so a self-hosted instance has the same 30-day cap.
Only the default expiry, `CAIRN_DEFAULT_TTL` (7 days), is configurable. MCP create
tools take no TTL at all, and always get the default.

A TTL that isn't a number of seconds, such as `X-Cairn-Ttl-Seconds: 7d`, is
`invalid_format`, and zero or a negative number is `not_positive`. The CLI converts
`30m`, `24h` and `7d` to seconds for you.

### Uppercase tags

```json
{"field": "tag", "location": "header", "reason": "uppercase", "value": "Size:M",
 "message": "\"Size:M\" must be lowercase"}
```

The server rejects any tag with a capital letter, on every surface. It doesn't fold the
case for you, because a routing rule downstream matches tags byte for byte, and a tag
that silently changed would route somewhere you didn't expect. Send `size:m`.

Every bad tag on a request is reported, not only the first, so a create carrying
`Size:M` and `lane m` gets two violations and a message that starts `2 problems:`.
Over MCP the field is the argument's index, `tags[0]`, rather than `tag`.

### The CLI changed my tag

```text
cairn: warning: tag "size:M" sent as "size:m"
```

The CLI is the one place tag case is fixed for you. Before it sends a request, `cairn`
lower-cases each `--tag` value that contains an uppercase ASCII letter, and prints this
warning to stderr for each tag it changed. The warning goes to stderr, so `--json`
output on stdout stays clean. The create goes ahead with the lower-cased tag.

It changes only the case. Any other character that tags don't allow, such as the space
in `--tag "lane m"`, is sent as you typed it, and the server's `invalid_charset`
violation is printed instead. REST and MCP callers get no folding: an uppercase tag
from them is rejected.

### Payload too large

`413 payload_too_large` means an upload is over the size limit (64 MiB per body by
default), a webhook capture is over 5 MiB, or an MCP call is too big. Its violation has
reason `too_large`. On an upload, `limit` is the cap in bytes; the other 413s don't
state their cap yet. For a large trace, page the spans or post it over REST.

### Rate limited

`429 rate limit exceeded` comes from a webhook ingress URL, which limits requests per
sending address and per endpoint. Back off and try again.

## A checksum differs after upload

You hashed your file, uploaded it, and the `checksum` in the response (or the
`X-Cairn-Checksum` header on a read) is different. Look for `redacted: true` in the
create response. It means Cairn found a credential and masked it: the stored body has
`[REDACTED]` where the value was, and the stored SHA-256 is the hash of those masked
bytes, not of your file.

- **A declared checksum still works.** If you send `X-Cairn-Sha256` with your file's hash,
  Cairn checks it against the bytes it received, before masking. The upload is accepted,
  and the response carries the masked body's hash.
- **Downloads match the masked hash.** Hash what you download from Cairn, not your local
  copy.
- **Deduplication uses the masked bytes.** Two uploads that differ only in the masked
  value are stored once.

The owner can see what was masked: the artifact's metadata shows status `masked`, a count,
and the rule IDs, never the values.


## Webhook signature mismatches

If Switchboard or your own receiver rejects Cairn's `X-Cairn-Signature`, work down this
list:

1. **Hash the raw bytes.** The signature is an HMAC-SHA256 of the exact request body.
   Parsing the JSON and serializing it again changes the bytes, and the signature won't
   match. Capture a delivery with the
   [webhook inspector](./webhook-inspector.md#debug-a-delivery-to-switchboard) and hash the
   downloaded body.
2. **Compare like with like.** The header is `sha256=` followed by lowercase hex.
3. **Check the secret.** The receiver's secret must match Cairn's byte for byte. A
   trailing newline, from `echo` or a copied file, is the classic culprit. After a
   rotation, both sides need the new value.
4. **No signature header at all?** The Cairn instance has no signing secret configured, so
   its deliveries are unsigned, and a signed Switchboard webhook rejects them.
5. **Look for a proxy.** Anything in between that re-encodes or decompresses the body
   breaks the signature.
6. **Check the clocks.** Switchboard also requires the signed `created_at` to be within
   five minutes, and `X-Cairn-Event-Id` to match the body's `event_id`. A receiver with a
   badly skewed clock fails the first check.

To compute the signature yourself, put the captured body in a file named `body` and the
secret in a local shell variable. `SECRET` below is only a name for this command; Cairn
never reads it. Its value must equal the signing secret of the delivery you are checking:
the secret of the subscription that sent it, shown once when the subscription was created
or its secret rotated.

```bash
SECRET='…'   # that subscription's signing secret
printf 'sha256=%s\n' "$(openssl dgst -sha256 -hmac "$SECRET" -r body | cut -d' ' -f1)"
```

## Outbound events never arrive

- You may have no outbound subscription, or its filters don't admit the event. Check
  Settings → **Outbound subscriptions**. Only your own artifacts reach your
  subscriptions: an artifact someone else created isn't yours to be told about.
- The subscription may be paused, or disabled after 20 failed deliveries in a row.
  Settings shows why; fix the receiver and **Resume** it.
- The target redirected (a `3xx` is a failed delivery), or it resolves to a private or
  loopback address, which Cairn never dials. Settings shows the last result.
- The target returned a `4xx` other than `429`. Cairn doesn't retry those.
- Cairn restarted while the event was queued. Queued events aren't saved, and there's no
  way to redeliver one.

In every case the artifact itself is fine. Read it at its link.
