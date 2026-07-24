---
status: accepted
date: 2026-07-08
decision-makers: joestump
# optional forward-only graph edges (extends / enables / related: lists of ADR IDs).
# Do NOT author inverse edges. Do NOT add `governs`.
extends: [ADR-0002]
related: [ADR-0006, ADR-0008]
---

# ADR-0009: Trajectory Capture and the OTel-Inspired Span Model

## Context and Problem Statement

The flagship new share type (design brief, turn 7) is the **trajectory**: a whole
agent run, shared. A run is a human prompt followed by an ordered, nested sequence
of reasoning turns, tool calls, and sub-agent excursions, rendered as an OTel-style
span waterfall pinned above a readable activity stream. A run also *produces*
artifacts — a `write` span emits `checkout-web-audit.md`, which is itself a separate
markdown share the trajectory links to. What data model captures a run faithfully
enough to draw the waterfall, compute the run stats, and tie the run to the artifacts
it produced; and how do agents get that data into Cairn — while the run is still
executing, or after it finishes, over REST and MCP?

This ADR realizes one entry in the share-type registry of ADR-0002. It leans on the
storage/content model of ADR-0008 for span outputs and produced-artifact bodies, and
on the annotation model of ADR-0006 for reactions and comments anchored to spans. It
does **not** decide the waterfall's rendering (ADR-0011) nor the live-delivery
transport (ADR-0010/ADR-0012); it decides the *capture format and ingestion path*.

## Decision Drivers

* **Faithful waterfall** — the model must carry everything the waterfall needs:
  ordering, nesting/depth, a start offset and duration per span, a category from the
  recommended set (`reason · exec · read · net · write · search · plan · tool · analyze · test · fix · fail · meta`),
  a name, and — for tool spans — a tool name, arguments, and output. A sub-agent must
  render as a nested span group.
* **Derived run stats, not stored redundantly** — wall time, span count, tool-call
  count, token count, and time-by-category are shown in the RUN panel; they should be
  *computable* from the span tree so they cannot drift from it.
* **Capture during OR after the run** — the brief says agents "POST/stream a run over
  MCP/REST while or after it runs." A finished run must be ingestible as one payload;
  a running one must be appendable so the web viewer can watch it fill in live.
* **Familiar to instrumentation authors, but not a tracing backend** — engineers know
  OpenTelemetry spans/traces; borrowing that shape lowers the conceptual cost. But
  Cairn is a sharing surface, not an APM, so it must borrow the *shape* without taking
  on OTLP wire compliance, trace-context propagation, sampling, or metrics/logs.
* **Unbounded outputs must not bloat the metadata store** — a `bash` span can emit
  megabytes of stdout. Large outputs belong in object storage (ADR-0008), referenced
  from the span, not inlined in Postgres.
* **Production is a first-class link** — a run that produces `checkout-web-audit.md`
  must record a durable, navigable edge to that artifact, honoring the ADR-0001
  principle that "a run produces artifacts."

## Considered Options

* **Option A — A native run/span tree, content-addressed outputs, dual ingestion
  (batch POST + incremental append).** A `trajectory` artifact whose body is an
  ordered tree of Cairn-defined spans stored as rows in PostgreSQL; each span carries
  category/name/timing/depth/parent and optional tool+args, with large output bodies
  spilled to object storage and referenced by SHA-256. Agents either POST a complete
  run in one call or open a run, append spans as they occur, and close it. Run stats
  are derived views over the span rows.
* **Option B — Ingest OTLP directly and store OpenTelemetry spans verbatim.** Accept
  the OpenTelemetry protocol (OTLP/HTTP), persist resource/scope/span structures as
  emitted, and map OTel `SpanKind`/attributes onto the viewer at read time.
* **Option C — Opaque event log.** Store the run as an append-only list of free-form
  JSON events (`{type, ts, ...}`) with no server-side schema; the viewer interprets
  the shape. Nesting and categories are conventions the client enforces.
* **Option D — Single immutable blob per run.** The whole run is one JSON document in
  object storage; Postgres holds only the artifact row. No per-span rows; the viewer
  fetches and parses the entire document; live runs re-upload a growing blob.

## Decision Outcome

Chosen option: **"Option A — a native run/span tree with content-addressed outputs
and dual (batch + incremental) ingestion"**, because it is the only option that
carries exactly the fields the waterfall and RUN panel need, lets stats be *derived*
from the spans (so they cannot disagree with the picture), supports both "share a
finished run" and "watch a running one," and keeps oversized tool output out of the
metadata store by pushing it to ADR-0008's object storage. Option B saddles a sharing
product with a tracing ingestion surface and a semantic-conventions vocabulary far
larger than the five categories the design uses, and OTel's `SpanKind` (server /
client / producer / consumer / internal) does not map onto `reason/exec/read/net/write`
— we would translate at read time anyway, so verbatim OTLP storage buys nothing.
Option C pushes the schema into every client and forfeits server-side stat derivation,
querying, and per-span annotation anchoring (ADR-0006 needs stable span identity).
Option D makes a live run a repeated whole-blob re-upload, forbids per-span reactions
without re-parsing, and cannot answer "how many tool calls" without reading the entire
body. So Cairn is **OTel-*inspired*, not OTel-*compliant*.**

### The span schema

A **trajectory** artifact (share type `trajectory`, badge `TRJ`, URL scheme
`cairn.sh/run/<id>` per ADR-0005) owns a **run** and an ordered tree of **spans**.
A run records: the human prompt, an absolute `started_at`, an optional `ended_at`
(absent while live), a `status` of `open` or `closed`, and a token count reported by
the agent. Each **span** carries:

* **`span_id`** — stable within the run; the anchor target for ADR-0006 reactions and
  comments, so it must not change once assigned.
* **`parent_span_id`** — null for top-level spans; set for nested spans. A **sub-agent**
  is simply a span whose children are its own tool spans (e.g. an `advisory lookup`
  span containing `web_search`, `web_fetch`, `summarize`).
* **`depth`** and **`seq`** — derived nesting depth and a monotonic sibling order, so
  the waterfall renders deterministically without re-deriving the tree on every draw.
* **`category`** — a free-form string drawn from the recommended set
  `reason · exec · read · net · write · search · plan · tool · analyze · test · fix · fail · meta`.
  The recommended categories are color-mapped in the waterfall legend; any other
  non-empty string is accepted and rendered with a neutral default color, so agents
  are never forced to remap their natural vocabulary. An empty or missing `category`
  is rejected at ingest. The recommended set covers the categories agents most
  commonly produce — thinking/reasoning (`reason`), command execution (`exec`),
  reading files/data (`read`), network calls (`net`), writing files/data (`write`),
  searching/looking things up (`search`), planning/decomposing (`plan`), generic tool
  calls that don't fit a narrower category (`tool`), analyzing or synthesizing results
  (`analyze`), applying a change (`fix`), a failed attempt (`fail`), and run
  overhead/metadata (`meta`).
* **`name`** — the human-readable label (`plan the audit`, `npm ls --all`,
  `read package.json`).
* **`start_offset_ms`** and **`duration_ms`** — start is relative to the run's
  `started_at`, keeping the time ruler (`0s … 34.2s`) independent of wall-clock skew.
* **`tool`** (optional) — the tool name for tool-category spans (`bash`, `grep`,
  `read`, `web_fetch`, `write`); null for `reason` spans.
* **`args`** (optional) — structured tool arguments, shown expandable in the stream.
* **`output`** — the tool result. Small outputs (below a fixed byte threshold, e.g.
  ≤ 16 KB) are inlined on the span row; larger outputs are stored as a content-addressed
  blob in object storage (ADR-0008) and the span holds an **`output_ref`** (SHA-256 +
  size + optional truncation flag) instead. A `bash npm ls --all · exit 0` output that
  runs to megabytes therefore never lands in Postgres.

Spans are rows in PostgreSQL keyed by `(run_id, span_id)`. The tree is reconstructed
by `parent_span_id`/`seq`; ADR-0008's content addressing deduplicates identical large
outputs across spans and runs.

### Derived run stats

The RUN panel figures — wall time `34.2s`, `11` spans, `8` tool calls, `48.1k`
tokens, and the time-by-category breakdown (`reason 13.7s · net 10.8s · exec 4.8s ·
read 3.4s · write 2.7s`) — are **computed from the span rows plus the run's token
count**, not stored as authoritative fields. Wall time is `ended_at − started_at`
(or `now − started_at` while open); span count and tool-call count are row counts
(tool-calls = spans with a non-null `tool`); time-by-category sums `duration_ms` per
category. This makes the panel provably consistent with the waterfall and lets a live
run's stats tick upward as spans append. Server-side aggregates may be *cached* for a
closed run, but the span rows remain the source of truth.

### How OTel concepts map (and where they deliberately don't)

| OpenTelemetry | Cairn trajectory | Note |
|---|---|---|
| Trace | Run | One run = one trajectory artifact. |
| Span (parent/child) | Span (`parent_span_id`) | Nesting kept; sub-agent = span with children. |
| Span name, start, duration | `name`, `start_offset_ms`, `duration_ms` | Kept, but offsets are run-relative. |
| Span attributes | `tool`, `args`, `output`/`output_ref` | Narrowed to the fields the viewer shows. |
| `SpanKind` | *(not adopted)* | Replaced by the `category` recommended set. |
| Trace context propagation (W3C) | *(not adopted)* | Agents send us a self-contained run. |
| OTLP wire protocol | *(not adopted)* | Ingest is Cairn REST/MCP JSON, not OTLP. |
| Resource / scope, sampling | *(not adopted)* | No infra topology, no sampling. |
| Metrics / logs signals | *(not adopted)* | Trajectories are spans only. |
| Span events / status | *(v1: not modeled)* | Errored-run rendering is a "try next" (below). |

The lesson borrowed from OTel is the **ordered nested-span tree with timing**; the
weight left behind is everything that makes OTel an interoperable telemetry pipeline.

### Ingestion path

Two shapes, both available over REST (ADR-0012) and MCP (ADR-0004), because the brief
requires capturing a run "while or after it runs":

1. **Batch** — `POST` a complete run (prompt + full span tree) in one call for an
   already-finished run. The server assigns the public id, validates the tree
   structure (categories are accepted as-is, not rejected for being outside the
   recommended set), spills oversized outputs to object storage, and closes the run.
2. **Incremental** — `open` a run (returns its id and URL immediately, so the human can
   be handed a link to a live run), `append` spans as they occur (each append is
   validated and persisted, and pushed to any live viewers), then `close` it (stamps
   `ended_at`, freezes derived stats). Appends are additive only; a closed run is
   immutable except for its annotation stream.

Live delivery of appended spans to browsers and MCP readers is the concern of
ADR-0010/ADR-0012 (SSE for the web waterfall, MCP stream reads for agents); this ADR
only fixes that the *capture* model is append-friendly.

### The artifact-produced-by-run link

A `write` span may declare that it **produced an artifact**. The produced thing —
`checkout-web-audit.md` — is an ordinary Cairn artifact (its own share, its own id,
its own body in object storage per ADR-0008), not data embedded in the run. The link
is a directed **`produced`** edge from the span (and thus the run) to that artifact,
stored as a row in PostgreSQL. In the stream, the `write` span renders a link to the
markdown share; on the produced artifact, provenance (ADR-0007) can name the run that
made it. The edge is created either by the agent naming the artifact id in the `write`
span at ingest, or by pushing the artifact and the run in the same MCP session and
letting Cairn resolve the reference. This is the concrete mechanism behind ADR-0001's
"a run produces artifacts, and a captured run links to the artifacts it produced."

### Explicit non-goals (v1)

Deferred, and marked "try next" in the design brief — none of these change the schema
above; each is additive:

* **Token-cost lane** on the waterfall (a per-span cost track).
* **Run-vs-run diff / compare** (two-push comparison of trajectories).
* **Errored-run rendering** — a red span for a failed step; v1 has no span-level
  error/status field (the OTel `status` row above is intentionally unmodeled), so
  failures are captured only as ordinary spans until this lands.
* **Filter the stream by category** (client-side scoping of the activity stream).

### Consequences

* Good, because the waterfall, the activity stream, and the RUN stats all read from
  one authoritative span tree, so they cannot disagree, and live runs update coherently
  as spans append.
* Good, because oversized tool output is content-addressed in object storage
  (ADR-0008), keeping Postgres rows small, deduplicating identical outputs, and letting
  the viewer lazy-load a large stdout only when a span is expanded.
* Good, because stable `span_id`s give ADR-0006 a durable anchor for per-span reactions
  and comments, and the `produced` edge makes the run-to-artifact relationship a real,
  queryable link rather than prose in a body.
* Bad, because declining OTLP compliance means we cannot ingest a stock OpenTelemetry
  exporter's output directly; agents must emit Cairn's run shape (a thin adapter, but a
  real one), and teams already exporting OTel get no free bridge.
* Neutral on categories — the recommended set is a superset of the original five
  (`reason · exec · read · net · write`) plus eight additions that cover common agent
  activities (`search · plan · tool · analyze · test · fix · fail · meta`). Because the
  `category` field accepts any non-empty string, a genuinely new category does not
  require a migration; it simply renders with a neutral default color until the
  recommended set and color legend are updated to include it.
* Neutral, because deferring error/status modeling keeps v1 lean but means a failed run
  is currently indistinguishable from a successful one in the schema until the
  errored-run "try next" is picked up; the append-only, immutable-when-closed rule is a
  deliberate simplification that forecloses post-hoc editing of a run.

### Confirmation

* Confirmed by the trajectory type's registration in the ADR-0002 viewer registry and
  by the span schema existing as PostgreSQL rows keyed by `(run_id, span_id)` with a
  `category` field that accepts any non-empty string, `parent_span_id`/`seq` nesting,
  run-relative timing, and an `output`/`output_ref` split at the size threshold.
* An ingestion conformance test drives both paths: a batch POST of a multi-span run
  with a nested sub-agent renders the documented example waterfall and computes the
  documented stats; an incremental open→append→close sequence yields the identical
  final run, proving batch and streaming converge.
* A storage test asserts that a span output above the inline threshold is written to
  object storage and referenced by SHA-256 (never inlined), and that two spans with
  identical large outputs share one blob (ADR-0008 dedup).
* A link test asserts that a `write` span declaring a produced artifact creates a
  `produced` edge resolvable from both the run and the artifact, satisfying the
  ADR-0001 "a run produces artifacts" principle.
* The specs derived from this ADR (in `docs/openspec/specs/`) assert the invariants:
  run stats are derived from spans (not independently stored as truth), empty or
  missing categories are rejected but any non-empty string is accepted, a closed run is
  immutable except for annotations, and the v1 non-goals above are absent from the schema.
