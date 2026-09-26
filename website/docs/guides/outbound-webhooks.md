---
title: Outbound webhooks to Switchboard
sidebar_label: Outbound webhooks
sidebar_position: 5
---

# Outbound webhooks to Switchboard

When one of your artifacts is created, Cairn can send an `artifact.created` event to
URLs you choose. The main consumer is [Switchboard](https://switchboard.stump.wtf/docs/),
which turns each event into a todo on an agent's queue. That's what makes
[agent handoffs](./agent-handoffs.md) run on their own.

:::note[Yours, not the instance's]
Each target is an **outbound subscription** that you own, added in Settings →
**Outbound subscriptions**. An event about your artifact goes only to your
subscriptions: never to another user's, and never to an instance-wide list, because
there isn't one. Someone else acting on your artifact doesn't send it to their
subscriptions either. Team subscriptions arrive with teams.
:::

## Add a subscription

In Settings → **Outbound subscriptions**, give:

- **Target URL.** It must be `https`, unless the operator has allowed plain `http`, and
  its host must resolve to a public address. Loopback, private (RFC 1918), link-local and
  unique-local addresses are refused.
- **Signing secret, optional.** Paste the secret your receiver issued, such as a
  Switchboard `cairn` webhook's `signing_secret`, and deliveries verify there with no
  change on the receiver's side. It must be at least 32 characters. Leave it empty and
  Cairn mints one starting `whsec_`.
- **Filters, optional.** Event types, share types and tags. An empty filter admits
  everything; a tag filter admits an event carrying any one of its tags.

The secret is shown **once**, when you create the subscription or rotate its secret.
Cairn stores it encrypted and never shows it again, so copy it into the receiver
straight away. Each user can have up to 5 subscriptions (the operator can change the
limit).

The same actions are on the API, for a browser session or the operator's human-role
[static token](./self-hosting.md#static-tokens-for-the-operator). Agent tokens and
personal access tokens are refused, so an agent can't point your events somewhere new:

| Request | Does |
|---|---|
| `POST /v1/subscriptions` | Create one: `{"url": "…", "secret": "…", "event_types": […], "share_types": […], "tags": […]}`. Answers `201` with the secret, once. |
| `GET /v1/subscriptions` | List yours, with their health, never their secrets |
| `GET /v1/subscriptions/{id}` | Read one |
| `PATCH /v1/subscriptions/{id}` | `{"paused": true}` pauses it; `{"paused": false}` resumes it |
| `POST /v1/subscriptions/{id}/rotate` | Replace the secret with `{"secret": "…"}`, or a minted one; answers with it, once |
| `DELETE /v1/subscriptions/{id}` | Delete it |

A subscription that isn't yours answers `404`, the same as one that doesn't exist.

## When it fires

Cairn sends an event after a new **single-body artifact** (markdown, code, image, file)
or a new **bundle** is saved, whether it came from the REST API, the CLI, or an agent
over MCP. Creating a trace or a webhook endpoint doesn't send one.

The event goes out in the background. It never slows down or fails the create request,
even if every subscription's target is down.

## What it sends

Each matching subscription gets a `POST` with a JSON body. It carries metadata about the
artifact, never its content:

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
    "tags": ["handoff", "lane:m", "reply:cairn-comment"]
  }
}
```

| Field | Meaning |
|---|---|
| `event_id` | Unique per event; use it to drop duplicates |
| `created_at` | When Cairn emitted the event |
| `data.id` | The artifact's id; pass it to `artifact_read` |
| `data.share_type` | `markdown`, `code`, `image`, `file`, `gz`, or `bundle` |
| `data.title` | The title, if it has one |
| `data.url` | The artifact's web link, which opens it for anyone who has it |
| `data.channel` | How it was created: `via MCP`, `via API`, or `via web` |
| `data.model` | The model the creator reported, if any |
| `data.actor_id` | The person whose credential created it; the only field here that identifies anyone |
| `data.expires_at` | When the artifact expires |
| `data.on_behalf_of` | For artifacts created over MCP, the client's self-reported name and version |
| `data.tags` | The creator's [tags](../product/tags.md), if any; routing hints, never authorization |

Empty optional fields are left out of the body.

Because the event includes the title and a working link, whatever receives it can open
the artifact. That's one more reason to keep secrets out of titles and bodies.

Every delivery also carries these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `cairn-outboundhook/1` |
| `X-Cairn-Event` | `artifact.created` |
| `X-Cairn-Event-Id` | The same value as `event_id` in the body |
| `X-Cairn-Signature` | `sha256=` and the lowercase hex HMAC-SHA256 of the raw body, keyed with that subscription's own secret |

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

- Cairn tries each subscription up to three times: once straight away, then after about
  a second, then after about four more. Each attempt times out after five seconds.
- Any `2xx` response counts as delivered. A `4xx` other than `429` isn't retried.
- Redirects aren't followed: a `3xx` is a failed delivery.
- The target's host is resolved again before every attempt, and Cairn doesn't dial it if
  any address it resolves to is loopback, private or otherwise non-public. A name that
  pointed somewhere public when you added it and somewhere private later fails.
- Events wait in a bounded in-memory queue. If Cairn restarts, or the queue fills up,
  pending events are dropped, and nothing can redeliver them.

So a receiver should answer `2xx` quickly, deduplicate on `event_id`, and re-read the
artifact before acting on it, since the artifact may have been rotated or have expired
since the event was sent.

Settings shows each subscription's health: when it was last tried, what happened, and
how many deliveries in a row have failed. After **20 consecutive failures** Cairn
disables the subscription and says so. Fix the receiver, then **Resume** it, which also
resets the count.

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
dropped. You'll need Switchboard endpoints for your agents.

1. **Create the webhook in Switchboard.** From the endpoint that should own it, call
   `create_webhook` with `source_type: "cairn"` and `target_queue: "inbox"`. Switchboard
   reveals a `signing_secret` and an `ingest_url`. Keep both somewhere safe.

2. **Point Cairn at it.** In Settings → **Outbound subscriptions**, add the `ingest_url`
   as the target and paste the `signing_secret` as its signing secret. To send only
   handoffs, set the tag filter to `handoff`. Every subscription has its own secret, and
   one Switchboard webhook routes to as many pools as you like.

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

Tags pick the pool; they never vouch for the sender. Your subscription only ever carries
events about your own artifacts, but the rule still matches on `actor_id`, and the
worker checks provenance again before it acts.
