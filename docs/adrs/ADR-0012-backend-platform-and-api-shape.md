---
status: accepted
date: 2026-07-08
decision-makers: joestump
extends: [ADR-0003]
related: [ADR-0008]
---

# ADR-0012: Backend Platform and API Shape

## Context and Problem Statement

Cairn's three surfaces — the web app (ADR-0011), the CLI, and the MCP server
(ADR-0004) — must all sit on **one** core service, per the triple-surface parity
decision (ADR-0003). That service has to do artifact CRUD, list the Bin, carry the
annotation layer (ADR-0006), handle sharing and workspace/auth, move bodies to and
from object storage (ADR-0008), and push two live streams (webhook requests in
ADR-0010, trajectory events in ADR-0009) to connected clients. What language,
framework, API style, versioning scheme, error contract, pagination model,
real-time transport, and deployment shape do we commit to so that all three surfaces
share the same behavior from a single, self-hostable binary?

## Decision Drivers

* **One core, thin adapters.** ADR-0003 requires the web handlers and the MCP server
  to call the *same* service package, not two parallel implementations. The platform
  choice must make a transport-agnostic core natural.
* **Single static binary, self-hostable.** The whole product should deploy as one
  artifact plus Postgres plus an S3-compatible store, runnable by one operator.
* **Server-push streams.** Webhook and trajectory streams are inherently
  server→client; the transport must fit the single binary and pass through ordinary
  proxies.
* **Machine-consumable errors.** The CLI and agents parse failures programmatically,
  so errors need a stable, structured contract — not prose.
* **A listable Bin under churn.** The Bin is paginated and mutates constantly as
  artifacts arrive and expire; pagination must stay stable and cheap.
* **Small dependency surface.** Fewer moving parts is better for a self-hosted,
  security-sensitive service that stores others' shared bytes.

## Considered Options

* **Framework — stdlib `net/http` + `chi` router** vs. a batteries-included framework
  (Gin / Echo / Fiber) vs. gRPC + a JSON gateway vs. GraphQL.
* **Real-time transport — Server-Sent Events (SSE)** vs. WebSocket vs. long-polling.
* **API versioning — URL path prefix (`/v1`)** vs. header/media-type versioning vs.
  unversioned.
* **Bin pagination — keyset (cursor) pagination** vs. offset/limit pagination.

## Decision Outcome

Chosen options: **Go** on **stdlib `net/http` + `chi`**, a **REST/JSON** API
versioned by **URL path prefix (`/v1`)**, **SSE** for live streams, and **keyset
(cursor)** pagination for the Bin — all served from a **single static binary** over
Postgres + object storage.

* **Go + `net/http`/`chi`, not a heavy framework.** Go compiles to the single static
  binary the deployment story wants, has first-class `context.Context` for the core
  service, and its `io.Reader`/`io.Writer` streaming is exactly what ADR-0008's
  through-the-API uploads and SSE both need. `chi` is a thin, idiomatic router over
  the standard library — middleware, URL params, sub-routers — without pulling in a
  framework's own request/response abstractions, keeping handlers plain
  `http.Handler`s that delegate to the core package. Gin/Echo/Fiber would add an
  abstraction layer and (for Fiber) a non-`net/http` core for little gain. gRPC
  would force a JSON gateway for the browser and complicate the "same core backs web
  and MCP" story; GraphQL's flexibility is unneeded for a fixed, small resource set
  and would make the SSE stream and object-body endpoints awkward.
* **SSE, not WebSocket.** Both live streams are one-way (server→client): webhook
  requests arriving, trajectory spans/events landing. SSE is plain HTTP with no
  upgrade handshake, so it traverses proxies and the single binary trivially; it has
  built-in reconnection with `Last-Event-ID` for resumable streams; and it is dead
  simple to consume from the browser (`EventSource`), from the CLI, and from agents
  reading the same stream over MCP (ADR-0010). WebSocket's bidirectionality is
  overkill here and costs us proxy friendliness and reconnection semantics we would
  have to rebuild.
* **`/v1` path versioning.** An explicit prefix is unambiguous for CLI and agent
  clients, trivially routable, and cache/proxy visible, unlike media-type versioning.
* **Keyset pagination.** A cursor over `(created_at, id)` is stable while artifacts
  are inserted and expired mid-scroll and stays O(page) instead of degrading like
  `OFFSET`; it suits both the web infinite-scroll Bin and the `cairn ls` TUI.

### API shape

REST/JSON resources under `/v1`, all backed by the core service package:

```
POST   /v1/artifacts                      create (pipe/add; streams a body → ADR-0008)
GET    /v1/artifacts/{id}                 fetch metadata + rendered/preview payload
GET    /v1/artifacts/{id}/body            download raw bytes (checksum verifiable)
DELETE /v1/artifacts/{id}                 delete (also honors expiry, ADR-0007)
GET    /v1/bin                            list the Bin (keyset paginated)
GET    /v1/artifacts/{id}/reactions       list reactions (ADR-0006)
POST   /v1/artifacts/{id}/reactions       react (idempotent per ADR-0006)
DELETE /v1/artifacts/{id}/reactions/{rid} un-react
GET    /v1/artifacts/{id}/comments        list comments
POST   /v1/artifacts/{id}/comments        comment (registry-gated anchors, ADR-0006)
POST   /v1/artifacts/{id}/share           set/adjust link access (ADR-0007)
GET    /v1/hooks/{id}/stream              SSE: live webhook requests (ADR-0010)
GET    /v1/runs/{id}/stream               SSE: live trajectory events (ADR-0009)
GET    /v1/workspaces/{id}                workspace + membership
...    /v1/auth/*, /v1/oauth/*            session auth and MCP OAuth (ADR-0004)
```

The MCP server (ADR-0004) and the html/template + HTMX handlers (ADR-0011) are thin
adapters over the *same* core methods these routes call — the REST surface is one
adapter among peers, not the core itself. This is how ADR-0003's parity is
mechanically guaranteed: there is one place to implement "create an artifact,"
"react," "list the Bin," and every surface reaches it.

### Error contract

Every non-2xx response is a single structured envelope, so the CLI and agents branch
on a stable machine code rather than parsing prose:

```json
{
  "error": {
    "code": "not_found",
    "message": "artifact 9qz1a does not exist or has expired",
    "details": { "id": "9qz1a" },
    "request_id": "req_7Kx2p"
  }
}
```

`code` is a stable enum (`not_found`, `unauthorized`, `forbidden`,
`validation_failed`, `conflict`, `payload_too_large`, `rate_limited`,
`internal`, …) aligned with the HTTP status. The same envelope is produced whether
the caller came in over REST, and the MCP adapter maps these codes onto MCP errors,
so an agent and a `curl` see the same failure taxonomy. `request_id` correlates the
response to structured server logs.

### Backend quality guidance (inherited by the specs)

The specs and implementations governed by this ADR inherit three non-negotiable
backend practices:

* **Structured error wrapping.** Wrap errors with context on the way up
  (`fmt.Errorf("load artifact %s: %w", id, err)`), preserving the chain with `%w`
  so handlers can map a domain error to an error `code` without string matching.
* **Context propagation.** Every core service method takes `context.Context` as its
  first argument; the request context (deadline, cancellation, request-scoped
  identity/auth, `request_id`) flows from the transport adapter through the service
  to the database and object-store calls, so a cancelled request or a hung SSE
  client releases its resources.
* **Parameterized queries only.** All SQL uses bound parameters (`$1`, `$2`); no
  query is assembled by string concatenation of caller input. This closes SQL
  injection by construction and is a review gate.

### Deployment shape

One static Go binary that **embeds** the web templates and the HTMX/Alpine.js assets
(`embed.FS`, per ADR-0011), bundles its schema migrations, and reads all config from
the environment. It connects to PostgreSQL (ADR-0008 metadata) and an S3-compatible
object store (ADR-0008 bodies). The same binary serves the web app, the `/v1`
REST/JSON API, the SSE stream endpoints, and the MCP server — the MCP surface runs as
a mode/subcommand of the one binary over the one core package. The result is the
self-hostable triple — **binary + Postgres + object store** — that an operator can
stand up without a fleet of services.

### Consequences

* Good, because a plain-`net/http` core keeps handlers as thin adapters over one
  service package, mechanically enforcing ADR-0003 parity across web, CLI, and MCP.
* Good, because SSE gives resumable server-push over ordinary HTTP that the browser,
  the CLI, and MCP consumers all read the same way, with no WebSocket upgrade or
  proxy special-casing.
* Good, because a single static binary + Postgres + object store is a genuinely
  self-hostable footprint, matching the product's self-host promise.
* Good, because a stable structured error contract plus keyset pagination make the
  API pleasant and predictable for the CLI and for agents.
* Bad, because SSE is one-way: any future client→server realtime need (e.g. live
  collaborative cursors) would require adding WebSocket alongside it. Accepted —
  today's streams are strictly server→client.
* Bad, because rolling our own router-plus-stdlib stack means we hand-build a little
  of what a framework ships (validation helpers, some middleware). Accepted as the
  cost of a small, auditable dependency surface.
* Neutral, because `/v1` path versioning commits us to carrying old versions
  side-by-side when the API changes incompatibly, rather than negotiating per
  request — a deliberate, boring choice.

### Confirmation

* A **parity test** drives artifact create / react / comment / list through the REST
  adapter and the MCP adapter and asserts identical core behavior and identical
  error `code`s, confirming ADR-0003.
* An **error-contract test** provokes each `code` and asserts the JSON envelope,
  HTTP status alignment, and a present `request_id`.
* A **pagination test** lists the Bin while inserting and expiring artifacts and
  asserts the keyset cursor neither skips nor duplicates rows.
* An **SSE test** connects to `/v1/hooks/{id}/stream`, drops the connection, and
  reconnects with `Last-Event-ID`, asserting resumption without loss or duplication.
* A **quality gate** in review/CI checks that core methods take `context.Context`,
  that errors are wrapped with `%w`, and that no SQL is built by string concatenation
  (parameterized queries only).
* A **deployment smoke test** boots the single binary against Postgres + MinIO/Garage
  from environment config and exercises web, REST, SSE, and MCP from the one process.
