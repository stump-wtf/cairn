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

`400 validation_failed` means something in the request itself was wrong. The usual
suspects:

- **A TTL over 30 days.** A longer `X-Cairn-Ttl-Seconds` or `--ttl` is rejected, not
  shortened.
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
  fixing it, so lowercase run ids and timestamps yourself.

The MCP create tools also reject any field they don't define, such as `visibility` or
`ttl`.

`413 payload too large` means an upload is over the size limit (64 MiB per body by
default), a webhook capture is over 5 MiB, or an MCP call is too big. For a large trace,
page the spans or post it over REST.

`429 rate limit exceeded` comes from a webhook ingress URL, which limits requests per
sending address and per endpoint. Back off and try again.

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

To compute the signature yourself, with the captured body in `body` and the shared secret
in `CAIRN_WEBHOOK_SECRET`:

```bash
printf 'sha256=%s\n' "$(openssl dgst -sha256 -hmac "$CAIRN_WEBHOOK_SECRET" -r body | cut -d' ' -f1)"
```

## Outbound events never arrive

- The instance may not have outbound webhooks configured. On the hosted service, that's up
  to the operator.
- The target returned a `4xx` other than `429`. Cairn doesn't retry those.
- Cairn restarted while the event was queued. Queued events aren't saved, and there's no
  way to redeliver one.

In every case the artifact itself is fine. Read it at its link.
