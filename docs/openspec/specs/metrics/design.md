---
status: draft
date: 2026-09-15
implements: [ADR-0021]
---

# Design: Prometheus Metrics

## Two collection styles, chosen per metric

**Counters inline.** Creation, expiry deletions and HTTP outcomes increment where
they happen.

**Gauges at scrape time.** `cairn_artifacts_total`, `cairn_storage_bytes` and
`cairn_artifacts_past_ttl` come from aggregate queries run by a custom collector:

```sql
SELECT share_type, count(*), coalesce(sum(size),0) FROM artifacts GROUP BY share_type;
SELECT count(*) FROM artifacts WHERE expires_at < now();
```

Maintaining these inline would require every delete path, TTL edit and failed
upload to adjust a gauge correctly forever. One missed adjustment yields a
permanently wrong number that still looks plausible — and for `artifacts_past_ttl`
specifically, drifting toward zero would mean silently reporting health.

## The past-TTL gauge is the point

`expired_total` and `past_ttl` answer different questions and only the second is
load-bearing:

| observation | sweeper healthy, nothing to do | sweeper broken |
|---|---|---|
| `expired_total` rate | 0 | 0 |
| `past_ttl` | 0 | climbing |

A success counter cannot separate these. That is why the spec requires the gauge
be **omitted** on collector failure rather than zeroed: a zero here is the
healthy reading, so a blind collector reporting zero would assert health at the
moment it stopped being able to see.

## Route classification

A hand-maintained map from handler to `route_class`, applied in middleware.
Deliberately not derived from the path: artifact handles are in paths, so a
derived label would both explode cardinality and publish handles to anyone who
can read the endpoint.

New routes default to `other`. `cairn_http_requests_total{route_class="other"}`
rising is the signal that the map needs updating — the classifier's own control,
without which it decays silently into one bucket.

## Storage bytes

Summed from the size recorded on each artifact row, not by asking the object
store. The database is authoritative for what Cairn believes it has stored;
divergence from the bucket's own accounting is a real finding and the two should
be measured independently rather than one derived from the other.

## Auth

Handled ahead of artifact-scoped authorization, against the operator credential
only. An artifact token grants access to artifacts, not to aggregate counts of
everyone's.

`401` with an empty body: share types and counts describe usage patterns, and an
unauthenticated prober should learn nothing, including whether the endpoint is
populated.

## Testing

* A broken-sweeper test: expiry fails, `past_ttl` climbs, `expiry_run_timestamp`
  stops advancing while `expired_total` stays flat — the three-signal shape from
  the spec's first scenario.
* A test that `past_ttl` is **omitted** and the error counter increments when the
  collector fails — explicitly asserting it is not zero, since zero means healthy.
* A test that `expiry_run_timestamp` does not advance on a failed sweep.
* A test that it is omitted before any successful sweep, rather than 1970.
* A cardinality test: an artifact whose handle appears in the request path
  produces no label containing it.
* An auth test: no credential `401`, artifact token `401`, operator `200`.
