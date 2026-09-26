---
status: draft
date: 2026-09-15
implements: [ADR-0021]
related: [ADR-0018, ADR-0020]
---

# SPEC-0014: Prometheus Metrics

## Overview

Cairn exposes a Prometheus text-format endpoint at `GET /metrics`, led by storage
and expiry — the two properties that grow without anyone deciding they should.

Cairn is the pipeline's only accumulating component. Artifacts carry TTLs, bodies
occupy object storage, and both increase with every successful agent run. The
metrics that matter are therefore about *bounded growth working*, not throughput.

## Requirements

### REQ-1: The endpoint

`GET /metrics` MUST serve Prometheus text format (`text/plain; version=0.0.4`)
via `promhttp`.

It MUST require authentication; an unauthenticated request MUST receive `401`
with no body. Cairn's metrics describe what users store, so the endpoint MUST NOT
be reachable with an ordinary artifact-scoped token — only the operator
credential.

Go collectors (`go_*`, `process_*`) MUST be registered.

### REQ-2: Storage and expiry — the mandatory set

```
cairn_artifacts_total{share_type}     gauge
cairn_storage_bytes{share_type}       gauge
cairn_artifacts_expired_total         counter
cairn_expiry_run_timestamp            gauge    unix seconds, last SUCCESSFUL sweep
cairn_artifacts_past_ttl              gauge
```

`share_type` is the closed set Cairn already models (`markdown`, `code`, `file`,
`bundle`, `run`, …) and MUST be reported for every value including zeros.

`cairn_artifacts_past_ttl` is REQUIRED and MUST count artifacts whose
`expires_at` is in the past and which still exist. It SHOULD sit at or near zero.

This is the distinction the spec turns on: `cairn_artifacts_expired_total`
increments when the sweeper deletes something, which tells you it **ran**.
`cairn_artifacts_past_ttl` tells you it ran **and worked**. They diverge exactly
when a sweep fails partway or skips a class of artifact — and a success counter
cannot tell that case apart from a quiet period with nothing to expire.

`cairn_expiry_run_timestamp` MUST advance only on a sweep that completed without
error. A sweep that errors MUST leave it unchanged, so
`time() - cairn_expiry_run_timestamp` is an honest staleness measure. It MUST be
omitted, not zeroed, before the first successful sweep.

### REQ-3: Request health

```
cairn_http_requests_total{route_class,method,status_class}  counter
cairn_http_request_duration_seconds{route_class}            histogram
```

`route_class` MUST be a bounded, hand-maintained classification
(`artifact_read`, `artifact_create`, `bundle`, `run`, `comment`, `asset`, `web`,
`other`) — never a raw path. Artifact handles appear in paths; a raw-path label
would make every artifact its own time series and publish the handles besides.

`status_class` MUST be `2xx`/`3xx`/`4xx`/`5xx`, not the exact code.

### REQ-4: Pipeline participation

```
cairn_artifacts_created_total{share_type,tagged_handoff}  counter  tagged_handoff: true|false
```

A handoff artifact that fails to create is a work order that never reaches a
worker, and that failure is currently visible only to its caller. Splitting
creation by whether the artifact carried the `handoff` tag lets a dashboard line
up "handoffs created" against the queue's "todos created" and see a gap.

`tagged_handoff` is a boolean, not the tag set. Tags are client-asserted and
unbounded (ADR-0018); they MUST NOT be labels.

### REQ-5: Cardinality

The following MUST NOT appear as labels: actor id or email, artifact id or
handle, tag values, filename, bucket key, or any client-supplied string.

`actor` is specifically excluded. It is unbounded and would turn the metrics into
a directory of who uses the service — a disclosure the endpoint's auth is not
meant to be the only defence against.

### REQ-6: Honest absence

A value that cannot be computed MUST be omitted rather than reported as zero, and
`cairn_metrics_collection_errors_total{collector}` MUST increment.

This applies with particular force to `cairn_artifacts_past_ttl`: a zero means
"nothing is overdue" and is the healthy reading, so a collector that fails and
reports zero would signal perfect health at the exact moment it had gone blind.
It MUST omit instead.

## Scenarios

### Scenario: the expiry sweep silently stops

A sweep begins failing on a permission error and is retried forever.

* `cairn_artifacts_expired_total` stops increasing — ambiguous alone; it looks
  the same as a quiet period
* `cairn_expiry_run_timestamp` stops advancing — now it is unambiguous
* `cairn_artifacts_past_ttl` climbs from ~0 into the hundreds
* `cairn_storage_bytes` rises steadily with no corresponding create surge

Today the first symptom would be a full volume.

### Scenario: storage growth attributed

Storage climbs 20% in a week.

* `cairn_storage_bytes{share_type="run"}` carries the increase while others are flat
* `cairn_artifacts_created_total{share_type="run"}` shows the matching create rate
* `cairn_artifacts_past_ttl` stays at zero — expiry is fine; this is real new load

Growth and a broken sweeper look identical on a storage graph alone.

### Scenario: handoffs created but not routed

Artifacts are created with the `handoff` tag; todos do not appear.

* `cairn_artifacts_created_total{tagged_handoff="true"}` increments normally
* the queue's `todos_created_total` does not move

The gap localises the fault to routing rather than to artifact creation — a
question that took direct inspection of both systems to answer on 2026-09-14.

## Out of Scope

* Per-artifact or per-actor series. Excluded by REQ-5.
* Artifact content or titles. Never a metric.
* Trace span volume for captured runs — Cairn stores traces; measuring what is
  inside them is the producer's job.
