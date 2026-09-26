---
title: Webhook inspector
sidebar_label: Webhook inspector
sidebar_position: 6
---

# Webhook inspector

A webhook endpoint is Cairn's requestbin. It gives you a URL that captures every request
sent to it (method, path, query, headers, and body) and shows them as they arrive. Use one
to see exactly what a service sends before you connect it to something real, or to find
out why a receiver rejected a delivery.

## Create an endpoint

The web app, the CLI, and MCP can't create endpoints yet, so use the REST API with a token
(see [Mint a personal access token](./first-share.md#mint-a-personal-access-token)):

```bash
curl -sS https://cairn.stump.wtf/v1/hooks \
  -H "Authorization: Bearer $CAIRN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title": "github push payloads", "request_cap": 100}'
```

Both fields are optional. `request_cap` is how many recent requests to keep. It defaults
to 500, and the oldest requests drop off as new ones arrive. The response carries the
addresses you need:

```json
{
  "id": "<id>",
  "url": "https://cairn.stump.wtf/<id>",
  "ingress_url": "https://cairn.stump.wtf/h/<id>",
  "mcp": "mcp://cairn/hook/<id>",
  "request_cap": 100,
  "expires_at": "…",
  "requests": []
}
```

| Address | Use it to |
|---|---|
| `ingress_url` | Receive requests. Give it to the sender; it accepts any HTTP method without authentication. |
| `url` | Watch requests arrive, in a browser |
| `mcp` | Let an agent read and subscribe to the same stream |

Like any artifact, the endpoint expires after 7 days by default.

## Send it something

```bash
curl -sS -X POST "https://cairn.stump.wtf/h/<id>?source=test" \
  -H 'Content-Type: application/json' \
  -H 'X-Hub-Signature-256: sha256=0123abcd' \
  -d '{"hello": "cairn"}'
```

Every captured request gets the same answer: `200` with the body `captured`. The sender
never gets anything else back, and can't read what was captured. The other responses:

| Status | Meaning |
|---|---|
| `404` | No such endpoint, or it has expired |
| `413` | The body is over the capture limit (5 MiB) |
| `429` | Too many requests from one address, or to one endpoint |

## Watch requests

- **In a browser:** open the endpoint's `url`. Requests stream in with highlighted JSON
  and a status mix. You can react to a single request, say 👀 on the one that broke.
  Requests don't take comments.
- **From an agent:** read the resource `mcp://cairn/hook/<id>` to get the endpoint and its
  retained requests, newest first, and subscribe to hear about new ones. Agents can only
  read the stream. Nothing writes into it except the ingress URL.
- **From a script:**

  ```bash
  curl -sS "https://cairn.stump.wtf/v1/hooks/<id>/requests"                      # newest first
  curl -sS "https://cairn.stump.wtf/v1/hooks/<id>/requests/<seq>"                # one request
  curl -sS "https://cairn.stump.wtf/v1/hooks/<id>/requests/<seq>/body" -o body   # its exact bytes
  curl -sSN "https://cairn.stump.wtf/v1/hooks/<id>/stream"                       # live, as server-sent events
  ```

:::warning[The viewer link shows everything captured]
Anyone with the endpoint's `url` can read every captured body. Don't point a sender at an
inspector if its payloads carry secrets or personal data you wouldn't paste into a shared
document.
:::

## What gets recorded

Cairn stores each request as inert data. It never runs, follows, or fetches anything in a
payload. Before it stores the headers, it lower-cases their names and drops credentials
and connection-level headers: `Authorization`, `Cookie`, `Set-Cookie`,
`Proxy-Authorization`, and hop-by-hop headers such as `Connection`. Signature headers such
as `X-Hub-Signature-256` and `X-Cairn-Signature` are kept, which is what makes the
inspector useful for signature problems.

## Debug a delivery to Switchboard

When Switchboard rejects a signed webhook, it doesn't store the payload, so there's
nothing to look at afterwards. Send a copy to an inspector to see what was actually on the
wire:

1. Create an endpoint, as above.
2. Add its `ingress_url` as a **second** target next to the Switchboard URL. For a forge
   such as GitHub or Gitea, add a second webhook with the **same secret**. For Cairn's own
   outbound events, add the endpoint as a second
   [subscription](./outbound-webhooks.md#add-a-subscription) and give it the same
   signing secret; Cairn sends the same body to both, signed with that secret.
3. Trigger the event again.
4. Compare the capture with what Switchboard expects:
   - **The signature.** Recompute the HMAC over the exact captured bytes (download them
     from `/body`; don't copy text out of the formatted view) and compare it with the
     captured signature header. See
     [Webhook signature mismatches](./troubleshooting.md#webhook-signature-mismatches).
   - **The event headers.** For Cairn, `X-Cairn-Event` should be `artifact.created`, and
     `X-Cairn-Event-Id` should equal the body's `event_id`.
   - **The content type and size.** Switchboard reads JSON bodies and enforces a size
     limit.
5. Remove the extra target when you're done.
