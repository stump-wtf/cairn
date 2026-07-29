# Design: Trajectory Share

## Context

The flagship new share type (design brief, turn 7) is the **trajectory**: a whole agent
run, shared. The viewer pins an OTel-style span waterfall above a readable activity stream;
a RUN panel shows derived stats; a `write` span links out to the artifact it produced.
**ADR-0009** decided the capture format and ingestion path — a native run/span tree with
content-addressed outputs and dual (batch + incremental) ingestion — and explicitly chose
to be *OTel-inspired, not OTLP-compliant*. This spec formalizes that model's observable
behavior.

The tree-semantics, timing-validity, and size-bound requirements were added in July 2026
after a production stress test: a harness posted a 162-span, multi-megabyte capture (over
MCP) organized as eleven category-labelled "phase" containers. Every one of its 147 child
spans carried a placeholder `duration_ms` of 1; 40 children started up to four hours
outside their parent's interval; and the category breakdown read `plan 26,104s` against
`0.14s` of everything else, because the containers' durations dominated a sum that also
counted their children. Each new requirement targets one of those observed failures
mechanically, rather than trusting guidance alone.

Constraints inherited from the ADRs:

- **SPEC-0002 / ADR-0002** — a trajectory is an ordinary artifact and a registry entry; it
  gets provenance, link access, expiry, and the app shell for free.
- **ADR-0008** — bodies (large span outputs, produced-artifact bodies) live in
  content-addressed object storage; metadata lives in PostgreSQL.
- **ADR-0006 / SPEC-0006** — reactions and comments attach through the unified annotation
  layer via typed anchors (`trajectory_turn`, `trajectory_toolcall`, `trajectory_span`).
- **ADR-0012** — one Go core service, REST/JSON under `/v1`, SSE for live push, keyset
  pagination, a structured error envelope.
- **ADR-0007** — link-capability reads, immutable trustworthy provenance, ephemeral TTL.

## Goals / Non-Goals

### Goals

- Persist a run faithfully enough to draw the waterfall, compute the RUN stats, and tie the
  run to the artifacts it produced.
- Support both "share a finished run" (batch) and "watch a running one" (incremental),
  converging to the identical final run.
- Keep oversized tool output out of PostgreSQL by content-addressing it in object storage.
- Derive all run stats from the span tree so they can never disagree with the waterfall.
- Give ADR-0006 a stable per-span anchor for reactions and comments.

### Non-Goals

- OTLP wire compliance, trace-context propagation, sampling, metrics/logs signals — Cairn
  is a sharing surface, not a tracing backend.
- Token-cost lane on the waterfall, run-vs-run diff, errored-run red-span rendering — all
  "try next," additive, and deliberately absent from the v1 schema (no span error/status
  field).
- Deciding the pixel-level rendering (ADR-0011) or the SSE transport internals (ADR-0012);
  this spec fixes the model and the observable ingest/read behavior.

## Decisions

### Native span tree over verbatim OTLP

**Choice**: Store runs as Cairn-defined span rows keyed by `(run_id, span_id)` with a
recommended `category` set (any non-empty string accepted), `parent_span_id`/`seq`
nesting, and run-relative timing.

**Rationale**: The model carries exactly the fields the waterfall and RUN panel need and
nothing else; stats are derivable; sub-agents fall out of `parent_span_id`. The
recommended vocabularies — by operation kind `reason · exec · read · net · write · search · plan · tool · analyze · test · fix · fail · meta`, or by workflow phase `research · implementation · review · testing · debug · build · docs · delivery · deploy · wait · prompt`
— cover what agents naturally produce, by operation and by workflow phase respectively, and accepting arbitrary non-empty
strings means agents are never forced to remap their categories. Known categories get
their designated accent color; unknowns fall back to a neutral default.

**Alternatives considered**:
- Verbatim OTLP storage: OTel `SpanKind` does not map onto the recommended categories, so
  we would translate at read time anyway — verbatim storage buys nothing and drags in a
  telemetry vocabulary far larger than needed.
- Closed five-value enum (original ADR-0009 design): too rigid — agents producing spans
  with categories like `search`, `analyze`, `tool`, or `fail` were rejected at ingest,
  forcing lossy remapping.
- Opaque event log: forfeits server-side stat derivation, querying, and stable per-span
  anchoring that ADR-0006 needs.
- Single immutable blob per run: makes a live run a repeated whole-blob re-upload and forbids
  per-span reactions without re-parsing.

### Stats derived, never stored as truth

**Choice**: Wall time, span count, tool-call count, and time-by-category are computed from
span rows (+ the run's token count); aggregates may be *cached* for a closed run only.

**Rationale**: A derived panel is provably consistent with the waterfall and lets a live
run's stats tick upward as spans append. Storing them as authoritative fields invites drift.

### Inline-vs-referenced output split

**Choice**: Outputs ≤ 16 KB inline on the span row; larger outputs go to object storage
(SHA-256) and the span holds an `output_ref`, fetched lazily on expand.

**Rationale**: A `bash` span can emit megabytes; that must not bloat Postgres. Content
addressing dedups identical outputs and makes downloads cacheable (ADR-0008).

### `produced` edge, not embedded body

**Choice**: A `write` span's produced artifact is an ordinary Cairn artifact; the link is a
directed `produced` row resolvable from both run and artifact.

**Rationale**: Honors ADR-0001's "a run produces artifacts" as a real queryable edge rather
than prose, and lets the produced artifact carry its own provenance, access, and TTL.

### Nesting means temporal containment, enforced at ingest

**Choice**: A child's interval must lie within its parent's; violations reject the payload
atomically. Siblings may overlap freely. Depth is capped at 32.

**Rationale**: The waterfall, the collapsed-time axis, the pan, and self-time accounting
all assume a parent covers its children — an assumption the schema previously never
checked, and the first harness to lean on nesting broke it four hours wide. Enforcing
containment makes "parent" mean something again without banning legitimate structure:
sub-agents, composite ops, and honest phase groupings all satisfy it naturally.

**Alternatives considered**:
- Guidance only (`run_capture` prompt): already tried for sibling overlap discipline; a
  prompt cannot stop a determined mis-modeller, and a broken tree breaks the *viewer*, not
  just the offender's own capture.
- Clamping children into the parent at ingest: launders wrong data into plausible-looking
  data; the reader can no longer tell the capture was wrong.
- Rejecting sibling overlap too: parallel tool calls are real (observed in Cairn's own
  session captures); concurrency is not an error.

### Zero and placeholder durations are rejected, not repaired

**Choice**: `duration_ms` must be present and ≥ 1; `start_offset_ms` ≥ 0. No clamping, no
defaulting.

**Rationale**: Nothing happens in zero time — a zero duration is always a capture bug, and
it poisons shared surfaces (breakdown percentages, zoom statistics, bar geometry) rather
than just its own row. Rejection with a clear message teaches the capturing agent to mine
its transcript for real timings (which every harness has); silently repairing to 1 ms
would preserve exactly the pathology observed in production, where every child span
carried a fabricated 1 ms and the trace answered no timing question at all. The 1 ms
floor is the resolution boundary: an instantaneous op measured at the model's granularity
is 1 ms, and that is measurement, not placeholder.

### Self-time category accounting

**Choice**: Time-by-category sums each span's duration minus the union of its children's
intervals, on every surface (server render and live client recompute), with legacy
uncontained children clipped to the parent before the union.

**Rationale**: With containers in the tree, summing raw durations double-counts every
child millisecond and lets the containers' label dominate the breakdown — the panel then
describes the capture's *organization*, not the run's *time*. Self-time is the standard
flame-graph answer, is insensitive to how many grouping layers an agent wraps around the
same work, and needs no schema change.

**Alternatives considered**:
- Leaf-only accounting: erases real parent self-work (a sub-agent's own reasoning between
  its tool calls would vanish).
- Excluding containers by heuristic (e.g. "has children ⇒ ignore"): same erasure, plus a
  cliff where adding one child re-classifies a span.

### Per-request and per-run span bounds, with MCP steered to the CLI

**Choice**: Cap spans per ingest request (default 500) and per run (default 10,000), both
configurable; reject over-bound requests with a message naming the paging path. MCP tool
descriptions and the `run_capture` prompt direct captures beyond the request bound to the
`cairn` CLI or paged REST appends.

**Rationale**: The server can digest a large run paged; what cannot digest it is the MCP
transport, where a tool call transits the agent's own context window — the observed 4 MB
single-call attempt is pathological even when the server would accept it. Bounds give the
steering teeth, and the error message makes the right path discoverable at the moment of
failure. This answers the former open question "what is the maximum span count per run."

## Architecture

The trajectory subsystem is a thin set of core-service methods plus a viewer. Ingestion
adapters (REST, MCP) call the same core; the core persists spans to Postgres, spills large
outputs to object storage, and fans appended spans out over SSE to live viewers and over the
MCP tail read to agents. The viewer reads the span tree once and derives stats client-agnostically.

```mermaid
erDiagram
    RUNS ||--o{ SPANS : contains
    SPANS ||--o| BLOBS : "output_ref (large output)"
    SPANS ||--o{ PRODUCED_EDGES : "write span produces"
    PRODUCED_EDGES }o--|| ARTIFACTS : "target artifact"
    RUNS ||--|| ARTIFACTS : "is a"
    ARTIFACTS ||--o{ REACTIONS : "anchored (turn/toolcall)"
    ARTIFACTS ||--o{ COMMENTS : "anchored (span/selection)"

    RUNS {
        bigint id PK
        text public_id
        text prompt
        timestamptz started_at
        timestamptz ended_at
        text status "open|closed"
        bigint token_count
    }
    SPANS {
        bigint run_id FK
        text span_id
        text parent_span_id
        int depth
        int seq
        text category "free-form; recommended set: reason|exec|read|net|write|search|plan|tool|analyze|test|fix|fail|meta"
        text name
        text tool
        jsonb args
        text output_inline
        char output_ref_sha256 FK
        int start_offset_ms
        int duration_ms
    }
    PRODUCED_EDGES {
        bigint run_id FK
        text span_id
        bigint artifact_id FK
    }
```

Incremental ingest and live fan-out flow like this — one `seq`-ordered log backing both the
SSE web waterfall and the MCP tail read:

```mermaid
sequenceDiagram
    participant Agent
    participant Core as Core Service
    participant DB as PostgreSQL
    participant OS as Object Storage
    participant Web as Web viewer (SSE)
    participant MCP as Agent reader (MCP)

    Agent->>Core: POST /v1/runs (open)
    Core->>DB: INSERT run (status=open)
    Core-->>Agent: run id + cairn.sh/run/<id>
    Web->>Core: GET /v1/runs/{id}/stream (SSE)
    Core-->>Web: replay existing spans, then subscribe
    loop each span as it happens
        Agent->>Core: POST /v1/runs/{id}/spans
        alt output > 16 KB
            Core->>OS: put blob (sha256)
            OS-->>Core: output_ref
        end
        Core->>DB: INSERT span (tx)
        Core-->>Web: SSE span-append event
        Core-->>MCP: tail read yields same seq
    end
    Agent->>Core: POST /v1/runs/{id}/close
    Core->>DB: UPDATE run (ended_at, status=closed)
    Core-->>Web: SSE close event
```

## Risks / Trade-offs

- **No OTLP bridge** → teams already exporting OpenTelemetry get no free ingest; they must
  emit Cairn's run shape (a thin adapter). Accepted per ADR-0009 — verbatim OTLP would cost
  a read-time translation for no gain.
- **Open category set** → unknown categories render in a color hashed from the category
  name rather than being rejected. This is a deliberate trade-off: it means agents can always post their
  natural vocabulary without remapping, at the cost of a slightly less polished legend for
  non-standard categories. The recommended set is expanded to thirteen values to cover the
  most common agent activities.
- **No v1 error modeling** → a failed run is not schema-distinguishable from a successful
  one until the errored-run "try next" lands. Mitigation: failures are captured as ordinary
  spans; the feature is additive.
- **App-tier SSE fan-out** → many concurrent viewers on a hot live run is real per-connection
  work (ADR-0010/ADR-0012 note the deferred broker). Mitigation: context-cancelled teardown
  of dropped subscribers, `Last-Event-ID` resume, and race-tested subscriber state.
- **Derived stats on large runs** → recomputing time-by-category on every read could be
  costly; mitigated by caching aggregates for closed (immutable) runs while keeping rows the
  source of truth.
- **Orphaned blob on crash** → a span-output blob written before its row commits can orphan;
  mitigated by ADR-0008's DB-authoritative refcounted GC reaper.
- **Stored runs predate the new validation** → runs ingested before containment/timing
  rules exist violate them and must still render. Mitigation: validation applies at ingest
  only; readers clip legacy child intervals to their parent for self-time and never reject
  stored data. Self-time also changes the displayed breakdown of existing container-heavy
  runs — deliberately, since the old numbers were the misleading ones.
- **Stricter ingest rejects captures that used to succeed** → an agent sending zero
  durations or uncontained children now gets `validation_failed`. Mitigation: the error
  message names the exact rule and the `run_capture` prompt teaches the fix (mine the
  transcript); the alternative — accepting the data — was observed to produce unreadable
  shares, which is the worse failure for a sharing product.

## Open Questions

- What is the exact inline-output threshold, and is it configurable per deployment? (ADR-0009
  suggests ≤ 16 KB as an example.)
- Should a run's token count be re-derivable/validated, or is the agent-reported figure
  authoritative (the ADR treats it as reported)?
- How long may a run stay `open` before it is auto-closed as abandoned, and does auto-close
  freeze stats identically to an explicit close?
- Does the `produced` edge support many artifacts per `write` span, or exactly one?
