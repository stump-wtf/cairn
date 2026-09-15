---
status: accepted
date: 2026-09-15
decision-makers: Joe Stump
related: [ADR-0018, ADR-0020]
---

# ADR-0021: Cairn Exposes Prometheus Metrics, Led by Storage and Expiry

## Context and Problem Statement

Cairn has no metrics surface: no `/metrics`, no Prometheus dependency, no
counters. The fleet runs VictoriaMetrics and Grafana and scrapes what exposes an
endpoint; two sibling services are gaining metrics in the same cycle, and a
dashboard covering the agent pipeline has a hole where Cairn sits.

Cairn's case is weaker than its siblings' and deserves saying so plainly. Neither
of the 2026-09-14 outages involved Cairn, and no incident has yet turned on a
number Cairn could have published. This is prospective instrumentation, not a
response to a failure.

That does not make it unjustified, but it does change what the decision has to
argue. Cairn is the one service in the pipeline that **accumulates**: artifacts
carry TTLs, bodies occupy object storage, and both grow with every agent run
without anyone deciding to grow them. It also sits on the handoff path — an
artifact that fails to create is a work order that never reaches a worker, and
that failure currently surfaces only to whoever made the call.

## Decision Drivers

* Cairn is the only pipeline component whose resource use grows monotonically
  with normal successful operation. Everything else is throughput.
* TTL expiry is the mechanism that makes growth bounded. If expiry silently stops
  working, nothing fails — storage simply grows, and the first symptom is a full
  volume long after the cause.
* Cairn sits on the handoff path. Its availability is a precondition for work
  reaching a lane worker, so pipeline dashboards need its request health beside
  the queue's.
* The honest driver is uniformity: three services, one dashboard, one scrape
  convention. A hole in the middle of a pipeline view costs more than the small
  weight of the instrumentation.
* Cairn is public-facing. Its metrics describe artifact counts, sizes and
  actors — closer to business data than to process telemetry.

## Considered Options

* **Defer until Cairn has an incident that needs it.** Genuinely tempting, and
  the cheapest option. Rejected because the specific failure most likely here —
  expiry quietly stopping — is precisely the kind that has no symptom until it is
  expensive, and adding instrumentation after that is adding it too late.
* **Storage-only metrics from the object store.** The bucket can be measured
  without Cairn's help. Rejected as the whole answer: it cannot attribute growth
  to artifact kinds or distinguish "expiry stopped" from "creation surged", which
  is the question that matters.
* **`GET /metrics` in Prometheus text format, authenticated.** Chosen.

## Decision Outcome

Cairn exposes **`GET /metrics`** in Prometheus text format via
`prometheus/client_golang`'s `promhttp`, **requiring authentication** — the same
posture as its siblings, and for a sharper reason: Cairn's metrics describe what
users store, not merely how the process is faring.

The set is led by **storage and expiry**:

```
cairn_artifacts_total{share_type}          gauge
cairn_storage_bytes{share_type}            gauge
cairn_artifacts_expired_total              counter
cairn_expiry_run_timestamp                 gauge    last successful sweep
cairn_artifacts_past_ttl                   gauge    should be ~0
```

`cairn_artifacts_past_ttl` is the load-bearing one. Counting expirations tells you
the sweeper ran; counting artifacts *still present past their TTL* tells you it
ran **and worked**. Those differ exactly when a sweeper fails partway or silently
skips a class of artifact — the case worth catching, and the one a success
counter cannot distinguish from a quiet period.

Alongside it, ordinary request health (`cairn_http_requests_total` by route class
and status) so Cairn's availability appears on the pipeline dashboard next to the
queue it feeds.

SPEC-0014 defines names, labels and types.

### Consequences

* Good: unbounded growth becomes visible as a trend rather than as a full volume.
* Good: a broken expiry sweep is detectable by a gauge that should sit at zero,
  rather than inferred from storage rising.
* Good: the pipeline dashboard covers the whole path — artifact created, todo
  routed, worker claims — instead of stopping at the queue.
* Bad: a new dependency, and instrumentation whose value is anticipated rather
  than demonstrated. Worth stating in the record so a future reader does not
  mistake this for an incident response.
* Bad: cardinality. `share_type` is a small closed set; `actor` is deliberately
  **not** a label, because it is unbounded and would make the metrics a directory
  of who uses the service.
* Neutral: authentication means the scrape carries a credential, as for siblings.

## More Information

* SPEC-0014 (metrics).
* Sibling decisions in the same cycle: Switchboard exposes metrics led by queue
  liveness; Harness exposes metrics led by model reachability. Both were written
  against a real outage. This one was not, and the difference is deliberate:
  Cairn's risk is accumulation, which does not announce itself.
