---
title: Outbound webhooks to Switchboard
sidebar_label: Outbound webhooks
sidebar_position: 5
---

# Outbound webhooks to Switchboard

When an artifact is created, Cairn can send an `artifact.created` event to a list of
URLs. The main consumer is [Switchboard](https://switchboard.stump.wtf/docs/), which
turns each event into a todo on an agent's queue. That's what makes
[agent handoffs](./agent-handoffs.md) run on their own.

:::note[Set per instance, not per person]
Outbound webhooks are configured by whoever runs the Cairn instance, and they fire for
every artifact created on it. The hosted service doesn't yet let you add your own target
from Settings. If you want Cairn events in your own Switchboard, talk to the operator.
:::

## When it fires

Cairn sends an event after a new **single-body artifact** (markdown, code, image, file),
a new **bundle** or a new **trace** is saved, whether it came from the REST API, the
CLI, or an agent over MCP. A trace sends it once, when it is opened or uploaded whole,
with `share_type` set to `trajectory`. Creating a webhook endpoint doesn't send one.

Comments, reactions and closed runs are never sent to these targets. The targets are
chosen by whoever runs the instance, not by the owner of the artifact, so those events
are reserved for subscriptions that artifact owners set up themselves.

The event goes out in the background. It never slows down or fails the create request,
even if every target is down.

## What it sends

Each target gets a `POST` with a JSON body. It carries metadata about the artifact, never
its content:

```json
{
  "source": "cairn",
  "kind": "artifact.created",
  "event_id": "6f1c1f0e-7f5b-4b8e-9a51-3f7f2c1d9b20",
  "created_at": "2026-09-11T17:04:05Z",
  "data": {
    "id": "<id>",
    "share_type": "markdown",
    "title": "handoff: audit the backup job",
    "url": "https://cairn.stump.wtf/<id>",
    "channel": "via MCP",
    "model": "claude-opus-5",
    "actor_id": "you@example.com",
    "expires_at": "2026-09-18T17:04:05Z",
    "on_behalf_of": "claude-code/2.1.0",
    "tags": ["handoff", "lane:m", "reply:cairn-comment"],
    "actor_kind": "agent",
    "auth": "oauth"
  }
}
```

| Field | Meaning |
|---|---|
| `event_id` | Unique per event; use it to drop duplicates |
| `created_at` | When Cairn emitted the event |
| `data.id` | The artifact's id; pass it to `artifact_read` |
| `data.share_type` | `markdown`, `code`, `image`, `file`, `gz`, `bundle`, or `trajectory` |
| `data.title` | The title, if it has one |
| `data.url` | The artifact's web link, which opens it for anyone who has it |
| `data.channel` | How it was created: `via MCP`, `via API`, or `via web` |
| `data.model` | The model the creator reported, if any |
| `data.actor_id` | The person whose credential created it; the only field here that identifies anyone |
| `data.expires_at` | When the artifact expires |
| `data.on_behalf_of` | For artifacts created over MCP, the client's self-reported name and version |
| `data.tags` | The creator's [tags](../product/tags.md), if any; routing hints, never authorization |
| `data.actor_kind` | `human` if the creator signed in with a browser session, `agent` for every token (MCP OAuth, personal access tokens, API tokens). Cairn works this out from the credential; no request field can set it |
| `data.auth` | How the creator signed in: `session`, `oauth`, `pat`, or `api_token` |

Empty optional fields are left out of the body. New fields are only ever added at the
end of `data`, so a consumer that ignores unknown keys keeps working.

Gate trust on `actor_kind` and `auth`, never on `on_behalf_of` or tags: those two are
whatever the client said.

Because the event includes the title and a working link, whatever receives it can open
the artifact. That's one more reason to keep secrets out of titles and bodies.

Every delivery also carries these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `cairn-outboundhook/1` |
| `X-Cairn-Event` | The same value as `kind` in the body: always `artifact.created` for these targets |
| `X-Cairn-Event-Id` | The same value as `event_id` in the body |
| `X-Cairn-Signature` | `sha256=` and the lowercase hex HMAC-SHA256 of the raw body, keyed with the instance's secret. It's left out when the instance has no secret. |

## Verify the signature

Compute the HMAC over the **raw request body**, before any JSON parsing, and compare it
with the header in constant time:

```python
import hashlib
import hmac

def verify(raw_body: bytes, header: str, secret: bytes) -> bool:
    expected = "sha256=" + hmac.new(secret, raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header or "")
```

```go
func verify(rawBody []byte, header string, secret []byte) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(rawBody)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}
```

A Switchboard webhook created with `source_type: "cairn"` does this for you. It also
rejects events whose `created_at` is more than five minutes old, and events whose
`X-Cairn-Event-Id` header doesn't match the body. If verification fails, see
[Webhook signature mismatches](./troubleshooting.md#webhook-signature-mismatches).

## Delivery is best-effort

The event is a doorbell, not a ledger. The artifact at its link is the record.

- Cairn tries each target up to three times: once straight away, then after about a
  second, then after about four more. Each attempt times out after five seconds.
- Any `2xx` response counts as delivered. A `4xx` other than `429` isn't retried.
- Redirects aren't followed, and targets must use `https` (plain `http` is allowed only
  for a loopback address).
- Events wait in a bounded in-memory queue. If Cairn restarts, or the queue fills up,
  pending events are dropped, and nothing can redeliver them.

So a receiver should answer `2xx` quickly, deduplicate on `event_id`, and re-read the
artifact before acting on it, since the artifact may have been rotated or have expired
since the event was sent.

## What a Switchboard routing rule sees

Switchboard runs each delivery through the webhook's routing rules, which are jq filters
over an envelope it builds. The fields a Cairn rule usually needs:

| Path | Value |
|---|---|
| `.source` | `"cairn"` |
| `.kind` | `"artifact.created"` |
| `.verified` | `true` when the signature checked out |
| `.artifact.id`, `.artifact.url`, `.artifact.title` | The artifact's id, link, and title |
| `.artifact.share_type`, `.artifact.channel`, `.artifact.model` | Its type, how it was created, and the model |
| `.artifact.actor_id`, `.artifact.expires_at` | Who created it, and when it expires |
| `.artifact.tags` | The creator's tags, as an array |
| `.artifact.event_id`, `.artifact.created_at` | The event's id and timestamp |
| `.payload` | The whole body Cairn sent, including `data.on_behalf_of` |

Switchboard's envelope also has `.artifact.metadata`, which Cairn doesn't send, so it's
`null`. For the complete envelope and the rule tools, see Switchboard's
[routing rules guide](https://switchboard.stump.wtf/docs/guides/routing-rules), and for a
ready-made version of the example below, its
[Cairn handoff recipe](https://switchboard.stump.wtf/docs/guides/routing-cookbook#route-cairn-handoffs).

## Worked example: route handoffs to an agent pool

The goal: an artifact **you** created, tagged `handoff` and `lane:m`, becomes a todo on
the medium pool's `handoff` queue. Everything else Cairn announces is recorded and
dropped. You'll need Switchboard endpoints for your agents, and the Cairn operator's help
for step 2.

1. **Create the webhook in Switchboard.** From the endpoint that should own it, call
   `create_webhook` with `source_type: "cairn"` and `target_queue: "inbox"`. Switchboard
   reveals a `signing_secret` and an `ingest_url`. Keep both somewhere safe.

2. **Point Cairn at it.** The Cairn operator adds the `ingest_url` to the instance's
   outbound webhook targets (`CAIRN_OUTBOUND_WEBHOOK_URLS`) and sets its signing secret
   (`CAIRN_OUTBOUND_WEBHOOK_SECRET`) to the `signing_secret`. Cairn signs every target with
   one secret, so an instance feeds one signed Switchboard webhook, and that webhook routes
   to as many pools as you like.

3. **Route to the pool.** Call `add_webhook_route` from that webhook to the medium pool's
   endpoint.

4. **Write the rule and test it.** Pull a real delivery's `event_id` from
   `list_webhook_events`, dry-run your rules with `test_webhook_rules`, then save them with
   `set_webhook_rules`. A Cairn delivery with a bad signature is rejected before rules run,
   so the rule checks the creator first, and only then looks at the tags:

   ```json
   {
     "webhook_id": "<cairn webhook id>",
     "rules": [
       {
         "id": "handoff-lane-m",
         "name": "my handoffs for the medium pool",
         "expr": ".artifact.actor_id == \"you@example.com\" and ((.artifact.tags | arrays // []) | any(. == \"handoff\") and any(. == \"lane:m\"))",
         "action": {"queue": "handoff", "endpoints": ["<medium pool endpoint id>"]}
       }
     ],
     "default_action": {"drop": true}
   }
   ```

   Don't guess the `actor_id`. Read it off a stored event with `test_webhook_rules`: it's
   whatever your credential authenticates as, which for a personal access token is its
   owner's sign-in, usually an email.

5. **Hand something off.** An agent, or you, creates the handoff prompt:

   ```text
   artifact_create(
     share_type: "markdown",
     title: "handoff: audit the backup job",
     body: "…the self-contained prompt…",
     tags: ["handoff", "lane:m", "reply:cairn-comment"]
   )
   ```

6. **Watch it land.** Cairn emits the event, Switchboard verifies it, and the rule puts a
   todo on the medium pool's `handoff` queue, which rings that pool's doorbell. The worker
   claims the todo, takes `data.id` from the event, calls `artifact_read`, checks
   `provenance.actor` (see [the trust model](./agent-handoffs.md#the-trust-model)), does
   the work, comments on the artifact with a link to its result (that's what
   `reply:cairn-comment` asks for), and completes the todo.

Tags pick the pool; they never vouch for the sender. That's why the rule matches on
`actor_id` and the worker checks provenance again before it acts.
