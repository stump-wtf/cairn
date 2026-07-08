---
status: accepted
date: 2026-07-08
decision-makers: joestump
# optional forward-only graph edges (include only those that apply):
extends: [ADR-0001]
enables: [ADR-0004, ADR-0011, ADR-0012]
---

# ADR-0003: Triple-Surface Parity — Web, CLI, and MCP over One Core Service

## Context and Problem Statement

ADR-0001 makes human/agent symmetry a property of the domain: every core operation —
create, read, list, comment, react, share — must be available to a human (web and
CLI) and to an agent (MCP) alike. Cairn therefore has three surfaces: the **web app**
(the human viewer and the Bin), the **CLI** (`cat file | cairn`, `cairn add`,
`cairn ls`), and **MCP** (the agent interface, authorized via MCP OAuth). The risk
is obvious and common: three independently built surfaces drift, so agents can do
things humans cannot or vice versa, business rules (expiry, access, provenance,
anchor legality) are re-implemented three times and diverge, and the "same auth as
your agent, same operations as your agent" promise quietly breaks. How do we
structure the system so all three surfaces are genuinely the *same* capability
expressed three ways, with no surface able to fork the rules?

## Decision Drivers

* **Parity is a product promise, not an aspiration.** The design pitches "the same
  operations humans get, over MCP" and "same auth as your agent." Parity of the core
  operations across all three surfaces must be structurally guaranteed, not left to
  discipline.
* **Single source of truth for business rules.** Expiry, access-policy enforcement,
  provenance capture, share-type resolution (ADR-0002), and annotation-anchor
  legality (ADR-0006) must be defined once and obeyed identically no matter which
  surface calls in.
* **Surfaces are genuinely different in shape.** Web is server-rendered HTML for a
  human reader; MCP is a tool/resource protocol for an agent; CLI is a
  pipe-and-clipboard tool plus a TUI. Their ergonomics differ; their *semantics* must
  not.
* **Provenance channel must be attributable.** The same "create artifact" operation
  must record *how* it was invoked (`via web`, `via CLI`, `via MCP`) — so the core
  needs the caller's channel/identity, but the *rule* of capturing provenance stays
  in one place.
* **Coherent house stack.** One Go codebase, one static binary for the server, a
  separate Go binary for the CLI (Bubble Tea TUI). REST/JSON for CRUD with SSE for
  live streams. The layering must fit this without a second service or a shared
  database reached three ways.
* **Testability and evolvability.** A new operation should be addable in one place
  and become available (or deliberately not) to all surfaces predictably; conformance
  should be checkable.

## Considered Options

* **Option A — One core domain-service package; REST/JSON API in front of it; web
  handlers and the MCP server both call the core in-process; the CLI is a REST
  client.** The core owns all rules; REST is the network boundary; web handlers and
  the MCP server are thin adapters over the same core package in the same binary; the
  CLI talks to the deployed REST API (sharing the agent's OAuth credentials).
* **Option B — Three surfaces, each talking directly to the database.** Web handlers,
  an MCP server, and the CLI each hold their own persistence/business logic against
  shared tables. Fast to start; no core package.
* **Option C — API-only core; every surface (including the web app) is a client of
  the REST API over the network,** with no in-process path. The web server holds no
  domain logic and proxies to the API like the CLI does.
* **Option D — Separate microservices per surface** behind a gateway, sharing rules
  via an internal RPC service. Independent deploys per surface.

## Decision Outcome

Chosen option: **"Option A — one core domain-service package with a REST API in
front, web and MCP as in-process adapters, and the CLI as a REST client"**, because
it makes parity a matter of *layering* rather than diligence: the core package is the
one place create/read/list/comment/react/share and all their rules live, and every
surface reaches those operations through it, so a rule cannot be enforced on one
surface and skipped on another. The layering is:

```
        ┌─────────────┐   ┌──────────────┐   ┌──────────────────────┐
        │ web handlers│   │  MCP server  │   │  CLI (cat|cairn, add,│
        │ (html/tmpl, │   │ (MCP OAuth,  │   │  ls TUI — Bubble Tea)│
        │  HTMX/Alpine)│  │  ADR-0004)   │   │                      │
        └──────┬──────┘   └──────┬───────┘   └───────────┬──────────┘
               │ in-process      │ in-process             │ HTTP (REST/JSON,
               │                 │                        │ SSE for streams)
               ▼                 ▼                        │
        ┌───────────────────────────────────┐            │
        │        REST/JSON API layer         │◀───────────┘
        │  (HTTP transport, auth/authz edge, │
        │   SSE for webhook/trajectory feeds)│
        └──────────────────┬────────────────┘
                           ▼
        ┌───────────────────────────────────┐
        │      CORE DOMAIN SERVICE (Go pkg)  │
        │  Artifact ops + all business rules │
        │  (ADR-0001 model, ADR-0002 types,  │
        │   ADR-0006 annotations, ADR-0007   │
        │   provenance/access/expiry)        │
        └──────────────────┬────────────────┘
                           ▼
             Postgres (metadata) + S3 (bodies)  — ADR-0008
```

The web handlers and the MCP server are **thin adapters in the same binary** that
call the core package directly (no self-HTTP), and also share the REST layer's
handlers where convenient; the CLI is a first-class REST client that authenticates
with the *same* OAuth as the agent, delivering the "same auth as your agent"
promise literally. This is what enables the MCP surface as a first-class citizen
(ADR-0004), the unified web shell (ADR-0011), and the backend/API shape (ADR-0012).

Option B (three surfaces on the database) is the drift factory this ADR exists to
prevent — every rule triplicated, parity impossible to guarantee. Option C (web is
also a network client of the API) is clean and tempting, and we keep the *contract*
purity by insisting web and MCP go through the same core, but we reject the mandatory
network hop: an in-process call avoids a second round trip and a self-referential
auth dance for server-rendered pages, and it lets the web layer stream trajectory/
webhook feeds from the core without proxying its own SSE. (The REST API remains the
canonical external contract, so nothing stops a future fully-decoupled web client.)
Option D (microservices) multiplies deploy and failure surface for a self-hostable
single-binary product and would push the shared rules into an internal RPC service
that is just Option A's core package with a network boundary bolted on — cost without
benefit at this scale.

### Parity, precisely

Parity means the **core operations** — create, read, list, comment, react, share —
are available on all three surfaces with identical semantics and identical rule
enforcement. It does **not** mean identical ergonomics or an identical feature list:

* **Web** is the human viewer: it renders share-type viewers (ADR-0002), the
  collapsible metadata/comments panel, and the Bin listing. It adds human-only
  presentation (rendered markdown, image pins, the trajectory waterfall) but invents
  no operation the core does not offer.
* **CLI** is *pbcopy for cairn*: `cat file | cairn` creates an artifact and returns a
  link; `cairn add f1 f2 …` creates a bundle; `cairn ls` is the Bin as a Bubble Tea
  TUI (browse, filter, open, share). It is a REST client carrying the user's OAuth.
* **MCP** is the agent surface (ADR-0004): read artifacts, create & push artifacts,
  comment & react — and *read* live webhook and trajectory streams
  (`mcp://cairn/hook/<id>`). Scoped consent maps to the same operations.

Surface-specific *shapes* are allowed (a TUI keymap, an HTMX partial, an MCP tool
schema); surface-specific *rules* are not. If a new operation is added to the core,
each surface makes a deliberate, reviewed choice to expose it or not — the default is
that a core operation is reachable everywhere.

### Where provenance and channel enter

The core's create/comment/react operations take the caller's identity and channel
from the calling adapter (web session, CLI OAuth, MCP OAuth token) and *the core*
records provenance (ADR-0007). The adapters supply facts; the core owns the rule. So
`via CLI` vs `via MCP` vs `via web` is captured uniformly even though the three entry
points differ.

### Consequences

* Good, because parity is structural: one core package is the only place operations
  and rules exist, so no surface can enforce expiry, access, or anchor legality
  differently, and the human/agent symmetry of ADR-0001 is guaranteed by
  construction rather than by review vigilance.
* Good, because the REST/JSON API is a real, documented contract (ADR-0012) that the
  CLI already depends on, which keeps the boundary honest and leaves the door open to
  additional clients (a mobile viewer, third-party tools) without new core work.
* Good, because "same auth as your agent" is literal: the CLI and the agent present
  the same OAuth credentials to the same API, so the auth model (ADR-0004) is built
  and tested once.
* Bad, because the in-process path for web/MCP plus the network path for the CLI
  means two invocation styles over one core; care is needed so a rule enforced at the
  REST edge (e.g. request-size limits, authz checks) is not accidentally the *only*
  place it is enforced — such rules must live in or below the core, not in the REST
  handler, or the in-process callers would bypass them.
* Bad, because insisting all three surfaces stay at parity adds coordination cost:
  every new core operation forces a conscious decision (and often work) on three
  surfaces, which slows down surface-specific experiments.
* Neutral, because the CLI being a pure REST client makes it slightly heavier than a
  direct-library tool (it needs the server reachable and authenticated), but this is
  exactly what keeps it honest as a first consumer of the public contract.

### Confirmation

* Confirmed by the package layout: a single core domain-service package that both the
  web handlers and the MCP server import and call directly, a REST/JSON API layer in
  front of it, and a CLI that contains no domain logic and only speaks REST. A review
  check rejects domain rules (expiry, access enforcement, provenance capture, anchor
  validation) implemented in a surface adapter instead of the core.
* A cross-surface parity test suite exercises create/read/list/comment/react/share
  through the REST API and through the MCP tools and asserts identical outcomes and
  identical rule enforcement (same expiry behavior, same access decisions, same
  provenance channel recorded) for equivalent inputs.
* The CLI's integration tests run against a live REST API using OAuth credentials of
  the same shape the agent uses, confirming the "same auth as your agent" property
  end-to-end.
* ADR-0004, ADR-0011, and ADR-0012 each build on this layering; their confirmations
  (MCP tool/authorization conformance, web shell rendering through the core, REST
  contract tests) collectively demonstrate that the three surfaces remain thin over
  one core.
