---
status: proposed
date: 2026-10-30
decision-makers: [joestump]
extends: [ADR-0021]
related: [ADR-0029, ADR-0023]
---

# ADR-0034: The Metrics Scrape Credential Is A Resolved Static Token

## Context and Problem Statement

ADR-0021 decided that `GET /metrics` is authenticated and never said with what.
ADR-0029 then listed "the metrics scrape credential ADR-0021 assumed but never
defined" among the operator surfaces still outstanding, which makes it a gap in
the tenancy design rather than a deployment detail: the answer decides which of
Cairn's credential families can reach instance-wide aggregate data.

SPEC-0014 REQ-1 constrains the answer from the other side: only the operator
credential may read the endpoint. Cairn now has four things a request can
present as authorization, and they mean very different things. A browser session
proves a signed-in human. A personal access token proves the holder for one
user's artifacts. An OAuth access token proves an agent for the artifacts a human
approved, and nothing else. A `CAIRN_API_TOKENS` entry proves an operator's user,
and boot refuses an entry that names anyone else (SPEC-0023 REQ "Static API
Tokens Act as an Operator's User").

Cairn's metrics are aggregate: artifact counts and byte totals by share type,
expiry sweep progress, request rates by route class. They describe everybody's
usage. Reading them is therefore not an artifact-scoped act, and a credential
whose whole design is "reach these artifacts" is the wrong shape for it.

## Decision Drivers

* A Prometheus scrape is a machine presenting a long-lived secret with no human
  in the loop, so sessions and anything requiring an interactive grant are out.
* Least privilege across tenancy: the credential that reaches aggregate counts
  should not be one that also reaches artifact bodies (ADR-0029's rule that an
  operator surface may bound tenant data and never reveal it).
* Rotation and revocation already exist for some credential families; inventing
  a sixth kind means a new environment variable, boot path, rotation path, and
  Settings surface, for one endpoint.
* An unauthenticated prober should learn nothing at all, not even whether the
  endpoint is populated.

## Considered Options

* **An operator's browser session.** Rejected: a scraper cannot complete the
  login redirect or hold the CSRF state, and a session cookie in a Prometheus
  config is a credential that expires on a human's schedule.

* **A personal access token belonging to an operator.** Rejected. A PAT is
  artifact-scoped by construction (SPEC-0023): it reads and writes its owner's
  artifacts, and it is the credential people paste into CI and shell profiles.
  Granting it the instance's aggregate counts would widen the one credential
  designed to be handed around.

* **An OAuth access token.** Rejected for the same reason in stronger form: the
  grant is per-artifact consent a human clicked. No consent flow produces
  "instance-wide aggregate telemetry", and minting one that does would quietly
  redefine what an MCP grant means.

* **A new dedicated `CAIRN_METRICS_TOKEN`.** Viable, and the likely eventual
  shape if scrape access needs to be granted without operator reach. Rejected for
  now: it is a second token vocabulary with its own boot and rotation story,
  solving a problem the static token table already solves, and it would be
  deployed before anyone has asked for metrics access that is not operator
  access.

* **A resolved `CAIRN_API_TOKENS` entry.** Chosen.

## Decision

The scrape credential is a static API token that survived boot resolution.
`GET /metrics` answers 200 when the bearer secret the request *presented* is one
of the resolved `CAIRN_API_TOKENS` entries, and 401 with an empty body and a
`WWW-Authenticate` header for everything else: no credential, a session, a PAT,
an OAuth access token, or an unresolved secret.

Two details are load-bearing.

The check is on the presented secret, not on the resolved principal. The
insecure dev bearer shortcut stamps its principal `AuthAPIToken` and resolves it
to a user, so "is this principal an operator's user?" would wave through anyone
who typed that operator's actor name on a box with the shortcut enabled. The
secret is the credential; a user id is not.

`/metrics` is served outside the rate limiter and outside request instrumentation.
A scrape every 15 seconds counted as traffic would put the classifier's only
control, the `other` bucket, permanently in motion, and a scrape being throttled
is an outage of visibility rather than of service. Where metrics are not wired at
all the endpoint answers 501, after the credential check, so the route's
existence stays invisible to a prober.

## Consequences

* Scraping an instance means naming an operator in `CAIRN_API_TOKENS`, which the
  fleet already configures. There is nothing new to deploy, and revoking that
  entry revokes the scrape.
* Aggregate data and artifact data stay in different credential families, so a
  leaked PAT in a CI log does not disclose instance usage patterns.
* The scrape secret is an operator secret. Anyone holding the Prometheus config
  can act as that user anywhere, not only at `/metrics`. The mitigation available
  today is to name a dedicated operator user for scraping, one that owns no
  artifacts; the mitigation available later is the option below.
* Adding a read-only metrics credential remains compatible: a `role` on a static
  token entry, checked in the same place, narrows this decision rather than
  reversing it.
* `/metrics` does not appear in `cairn_http_requests_total`, so "is the scrape
  alive" is answered by Prometheus's own `up`, which is the right place for it.

## More Information

* SPEC-0014 REQ-1 (auth), REQ-3 (route classes); design section "Auth".
* SPEC-0023 REQ "Static API Tokens Act as an Operator's User" (why a resolved
  entry can only name an operator).
* ADR-0021 (the endpoint), ADR-0029 sections 1 and 7 (the undefined credential
  this ADR closes).
* Tests: `TestIntegrationMetricsEndpointCredential` (no credential, PAT, MCP
  token, operator token) and `TestIntegrationMetricsDevBearerIsNotACredential`.
* Status is `proposed` pending review: this is instance security posture, and the
  thread decided the endpoint and its labels, not its credential.
