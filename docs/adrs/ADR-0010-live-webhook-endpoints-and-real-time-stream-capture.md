---
status: accepted
date: 2026-07-08
decision-makers: joestump
# optional forward-only graph edges (extends / enables / related: lists of ADR IDs).
# Do NOT author inverse edges. Do NOT add `governs`.
extends: [ADR-0002]
related: [ADR-0006, ADR-0008, ADR-0012]
---

# ADR-0010: Live Webhook Endpoints and Real-Time Stream Capture

## Context and Problem Statement

The `webhook` share type (badge `HK`) is a **live** requestbin: an endpoint an agent
or a service points at, whose inbound HTTP requests stream into a web inspector
(highlighted JSON, method/status, a status mix) and are readable by agents over MCP
as the *same* stream (`mcp://cairn/hook/<id>`). Unlike every other share type, its
"body" is not pushed by its owner — it is filled by unauthenticated third parties
hitting an open ingress URL. That raises questions no static artifact has: how are
captured requests stored and indexed, how many do we keep, how do live requests reach
open browsers and connected agents in real time, and how do we keep an
internet-facing, anonymous-write endpoint safe? And, per the design, webhook requests
are **reactable but not comment-threaded** — so where does discussion actually go?

This ADR realizes another entry in the ADR-0002 registry. It stores request bodies in
the object storage of ADR-0008 and indexes metadata in PostgreSQL; it constrains
annotations per ADR-0006 (reactions only, no threads); and it delivers live over the
SSE transport chosen in ADR-0012. It does not decide the inspector's visual design
(ADR-0011).

## Decision Drivers

* **Live, not batch** — the inspector's value is watching requests arrive; capture and
  fan-out must be near-real-time, to both browsers and MCP clients, off one stream.
* **One stream, two readers** — a human in the web inspector and an agent over MCP must
  see the *same* ordered request stream. There is one capture; the surfaces are views.
* **An open ingress is an attack surface** — the endpoint accepts anonymous requests
  from anywhere. It must never execute a payload, must bound request size, and must
  rate-limit to survive being flooded or used to fill storage.
* **Bounded retention** — a requestbin is a live buffer, not an archive. Captured
  requests must be capped so a chatty sender cannot grow one endpoint without limit,
  and the endpoint itself is ephemeral like every artifact (ADR-0007).
* **Bodies can be large and arbitrary** — payloads range from a 200-byte JSON ping to a
  multi-megabyte upload; bodies belong in object storage (ADR-0008), with only queryable
  metadata (method, status, size, content-type, timestamp) in Postgres.
* **Discussion moves to produced artifacts** — the design is explicit that requests are
  reactable but *not* threaded, "because discussion happens on the artifacts they
  produce, not here." The model must make that redirection natural, not accidental.

## Considered Options

* **Option A — Native capture: an artifact-typed endpoint, Postgres-indexed request
  ring buffer, object-storage bodies, SSE + MCP fan-out.** A `webhook` artifact owns an
  ingress URL; each inbound request becomes a capped, append-only request record (method,
  path, headers, status, size, content-type, received-at in Postgres; body in object
  storage). New records are pushed live to browsers over SSE and are readable by agents
  over MCP off the same ordered log. Reactions only, per ADR-0006.
* **Option B — Front a third-party requestbin** (RequestBin / webhook.site / smee.io):
  proxy or import their capture into Cairn for display.
* **Option C — Message broker as the store of record** (Redis Streams / NATS
  JetStream): the request log lives in the broker, which also does fan-out; Postgres and
  object storage are optional projections.
* **Option D — Object storage only, no index**: append each raw request as a blob; the
  inspector and MCP list by scanning a prefix; no Postgres request rows.

## Decision Outcome

Chosen option: **"Option A — native capture with a Postgres-indexed request ring
buffer, object-storage bodies, and SSE + MCP fan-out"**, because it keeps the webhook
inside the one-artifact model of ADR-0001 (so provenance, access, and expiry come for
free), gives the inspector fast metadata queries (status mix, method filter, counts)
without reading bodies, keeps arbitrary/large payloads in the right tier (ADR-0008),
and serves the identical ordered stream to both browsers (SSE, ADR-0012) and agents
(MCP). Option B outsources the core value, adds a dependency and a data-residency
problem, and breaks the "same stream over MCP" promise since the canonical data would
live off-platform. Option C is operationally heavier than v1 warrants — a broker as the
system of record adds a stateful dependency for a feature whose durability needs are
modest, and we would still project into Postgres/object storage for the inspector,
annotations, and expiry, so the broker is pure overhead until scale demands it (it
remains a defensible future fan-out layer, not a v1 store). Option D cannot answer
"status mix" or "how many POSTs" without scanning every blob and gives ADR-0006 no
stable per-request row to anchor a reaction to.

### The webhook endpoint and its two addresses

A `webhook` artifact (URL `cairn.sh/<id>`, `◆ mcp` affordance) exposes two addresses
for the *same* endpoint:

* **HTTP ingress** — a public URL (e.g. `hook.cairn.sh/<id>` or `cairn.sh/h/<id>`) that
  accepts inbound requests of any method. The `<id>` is the short, opaque, unguessable
  base62 id of ADR-0005; unguessability is the endpoint's first line of defense.
* **`mcp://cairn/hook/<id>`** — the MCP handle agents use to read the captured stream.

The endpoint is created by its owner (CLI/web/MCP) and is an ordinary artifact: it has
provenance, a link-based access policy, and a default TTL (ADR-0007). When it expires,
the ingress stops accepting requests and the captured stream is dropped with it.

### Request capture and storage

Each accepted inbound request becomes one **request record**:

* **Metadata in PostgreSQL** — `webhook_id`, a monotonic `seq` (stream order),
  `received_at`, `method`, `path`, `query`, selected/normalized `headers`, `status`
  (the fixed response Cairn returned), `content_type`, and `body_size`. This is the row
  the inspector queries for the status mix, method badges, and counts, and the stable
  anchor ADR-0006 reactions attach to.
* **Body in object storage** — the raw request body is written to the object storage of
  ADR-0008, content-addressed by SHA-256, and referenced from the record by a
  `body_ref` (hash + size). Bodies are fetched lazily when a request is expanded. A JSON
  body is stored verbatim; the inspector's syntax highlighting is a render-time concern
  (ADR-0011), so we never transform on ingest. Identical bodies dedup by hash.

Capture always returns a fixed, benign response (a `200` with a short acknowledgment by
default; the response is *data we record*, never behavior the payload can steer). Cairn
does not evaluate, deserialize-to-execute, or act on the payload — see security below.

### Retention and caps

A webhook is a **bounded live buffer**, enforced two ways:

* **Per-endpoint request cap** — an endpoint retains at most the last **N** requests
  (a ring buffer, e.g. N = 500); on overflow the oldest record is evicted and its body
  blob dereferenced. This bounds one endpoint's footprint regardless of sender volume.
* **Endpoint TTL** — the artifact's default expiry (ADR-0007) bounds lifetime; on expiry
  the endpoint and all its records and bodies are removed.

Together these cap both *how many* requests and *how long*, so an anonymous flood can at
worst churn a single endpoint's ring buffer, never exhaust global storage.

### Real-time delivery — one stream, two transports

Capture writes the record, then fans out the new `seq` to live subscribers:

* **Browsers** — the inspector opens an **SSE** stream (ADR-0012); each captured request
  is pushed as an event and prepended to the live list (the HTMX/`aria-live` mechanics
  are ADR-0011). New viewers first load the recent buffer, then subscribe for tail
  updates, so a freshly opened inspector shows history *and* live arrivals.
* **MCP clients** — agents read the same ordered log over `mcp://cairn/hook/<id>`:
  paginated reads for history plus a tailing/streaming read for live capture, yielding
  the identical `seq`-ordered records. "Agents read the same stream over MCP" is
  literally the same underlying log the SSE fan-out reads, not a parallel copy.

Because both transports read one `seq`-ordered capture log, a human and an agent
watching the same endpoint see the same requests in the same order.

### Security of an open ingress endpoint

The endpoint is anonymous-write and internet-facing, so it is constrained on ingest:

* **No code execution, ever** — payloads are stored bytes and indexed metadata. Cairn
  parses just enough (content-type, size) to store and display; it never executes,
  shells out, follows, or otherwise *acts on* a payload. The inspector renders payloads
  as inert, escaped text (ADR-0011).
* **Body size limit** — requests above a hard cap (e.g. a few MB) are rejected or
  truncated with a truncation flag on the record, so one request cannot blow past the
  object-storage tiering or memory.
* **Rate limiting** — per-endpoint and per-source-IP limits throttle floods; excess
  requests get a `429` and are not captured, protecting the ring buffer and fan-out.
* **Unguessable id** — the base62 id (ADR-0005) makes endpoints undiscoverable by
  enumeration; there is no listing of endpoints one does not own.
* **Header hygiene** — sensitive/hop-by-hop headers are normalized or dropped; the
  stored request is a sanitized projection, not a replayable credential dump.
* **Ephemerality as containment** — the default TTL guarantees an abused endpoint
  self-heals when it expires (ADR-0007).

### Relationship to the artifacts discussion moves to

Per ADR-0006, a captured request is **reactable but not comment-threaded** — you can
drop 🔥/👀 on a single request, but there is no comment thread on it. This is deliberate:
a live buffer is a poor place to hold a durable conversation. The intended flow is that
a human or agent *watches* the stream, then **produces an artifact** from what they saw
— a bug report, a captured-payload markdown, a summary — and **that** artifact is fully
comment-threaded. The webhook is the live capture surface; discussion happens on the
produced artifact, tied back by provenance/links exactly as in ADR-0009's
run-produces-artifacts pattern. This ADR does not force a hard foreign key from a
request to a produced artifact (a request may inspire an artifact that names the
endpoint in prose); it establishes the *social contract* the annotation constraint
encodes — threads live on produced artifacts, reactions live on requests.

### Explicit non-goals (v1)

Marked "try next" in the design brief and intentionally out of scope here:

* **Empty-state for an endpoint awaiting its first request** (a first-run affordance).
* **Filtering the stream by status/event** in the inspector.

Neither changes the capture model above; both are additive read-side features.

### Consequences

* Good, because one `seq`-ordered capture log backs both the SSE web inspector and the
  MCP stream, so humans and agents provably see the same requests in the same order,
  honoring "agents read the same stream over MCP."
* Good, because metadata-in-Postgres / bodies-in-object-storage (ADR-0008) makes the
  status mix, method badges, and counts cheap to compute while arbitrary large payloads
  stay in the right tier and dedup by hash.
* Good, because the ring-buffer cap plus TTL bound an anonymous-write endpoint's blast
  radius to a single self-healing buffer, and the no-execution rule removes the scariest
  class of open-ingress risk by construction.
* Bad, because a strict last-N ring buffer silently drops older requests; a user who
  wanted the 501st request back cannot get it, and there is no v1 archive/export — the
  mitigation ("produce an artifact from what mattered") is a workflow, not a guarantee.
* Bad, because near-real-time SSE fan-out to many concurrent inspectors on a hot endpoint
  is real per-connection work; without the deferred broker (Option C) fan-out is done in
  the app tier and will need attention if endpoints get very popular.
* Neutral, because forbidding comment threads on requests is a UX constraint some users
  will find surprising until they learn the "discuss on the produced artifact" flow; it
  is a deliberate steer, enforced by ADR-0006, not a technical limitation.

### Confirmation

* Confirmed by the webhook type's registration in the ADR-0002 registry, by request
  records existing as capped, `seq`-ordered PostgreSQL rows with bodies referenced by
  SHA-256 in object storage (ADR-0008), and by both an SSE endpoint (ADR-0012) and an
  `mcp://cairn/hook/<id>` reader serving that one log.
* A parity test points a browser (SSE) and an MCP client at the same endpoint, sends a
  sequence of requests, and asserts both observe the identical `seq`-ordered stream,
  including the recent-history-then-tail behavior for a late-joining subscriber.
* A capacity test exceeds the per-endpoint request cap and asserts the oldest records
  and their body blobs are evicted, keeping the endpoint's footprint bounded; an expiry
  test asserts an expired endpoint stops capturing and drops its stream.
* A safety test asserts: an over-cap body is rejected/truncated with the flag set; a
  flood trips the rate limit with `429`s and no capture; and stored payloads are
  displayed as inert escaped text with no server-side execution of any kind.
* An annotation test asserts a request accepts reactions but exposes no comment-thread
  affordance (ADR-0006), and the specs in `docs/openspec/specs/` record the "discussion
  moves to produced artifacts" invariant.
